package radius

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

// RADIUS accounting — FR-AAA-003 | DDS §5.2.
//
// This is what writes subscriber_session_history, and almost everything that
// reads usage depends on it existing: the FUP scanner finds over-quota sessions
// there (FR-FUP-001), the CoA sender resolves which Acct-Session-Id to re-shape
// (FR-FUP-002), LEA answers "who held this IP at this time" from it
// (FR-OBS-003), and the subscriber portal draws its usage history from it.
//
// Until this file existed the daemon acknowledged Accounting-Requests and threw
// them away, so every one of those features read an empty table — each was
// individually correct and collectively inert.

var (
	radiusAcctProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_acct_processed_total",
		Help: "Accounting requests persisted, by status type and outcome",
	}, []string{"status", "outcome"})
	// A session that stops or updates without a matching open row: the daemon
	// was down when it started, or the NAS is accounting for a session this
	// system never authorised. Either way the usage is lost, so it is worth a
	// number an operator can alert on rather than a silent no-op.
	radiusAcctUnmatched = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_acct_unmatched_total",
		Help: "Interim/Stop records with no matching open session",
	})
)

// AccountingStore persists the session lifecycle. Satisfied by *db.FUPStore.
type AccountingStore interface {
	// StartSession must be idempotent on sessionID: a NAS retransmits an
	// unacknowledged Start, and a duplicate open row would be double-counted by
	// the FUP scanner's SUM over open sessions.
	StartSession(ctx context.Context, subscriberID int, sessionID, nasIP, assignedIP string) error
	UpdateSessionOctets(ctx context.Context, sessionID string, inputOctets, outputOctets int64) (bool, error)
	StopSession(ctx context.Context, sessionID string, inputOctets, outputOctets int64, cause string) (bool, error)
}

// SetAccountingStore enables session persistence.
//
// Optional, and its absence is loud rather than silent: without a store the
// daemon still acknowledges accounting (a NAS that gets no reply retransmits
// forever and eventually stops accounting altogether) but records nothing, and
// says so at startup.
func (d *RadiusDaemon) SetAccountingStore(s AccountingStore) { d.acctDB = s }

// handleAccounting persists one Accounting-Request.
//
// The NAS is acknowledged in every path, including failures. RADIUS accounting
// has no "try again later" response: a NAS whose Accounting-Request goes
// unanswered retransmits, and on repeated failure many implementations drop the
// record entirely or stop the session. Nothing recoverable is gained by
// withholding the ACK, so persistence problems are logged and counted here
// rather than pushed back onto the NAS.
//
// FR: FR-AAA-003 | DDS §5.2
func (d *RadiusDaemon) handleAccounting(ctx context.Context, w radius.ResponseWriter, r *radius.Request) {
	defer func() {
		w.Write(r.Response(radius.CodeAccountingResponse)) //nolint:errcheck,gosec
	}()

	statusType := rfc2866.AcctStatusType_Get(r.Packet)
	status := acctStatusLabel(statusType)

	// Accounting-On/Off announce a NAS rebooting. They carry no session, and
	// answering them is the whole handling they need — the stale sessions such a
	// reboot leaves open are a separate cleanup concern, not this packet's job.
	if statusType == rfc2866.AcctStatusType_Value_AccountingOn ||
		statusType == rfc2866.AcctStatusType_Value_AccountingOff {
		radiusAcctProcessed.WithLabelValues(status, "acknowledged").Inc()
		return
	}

	// The real Acct-Session-Id (RFC 2866, attribute 44). An earlier version of
	// this handler read NAS-Identifier here — a per-device string, not a
	// per-session one — so every session on one NAS shared a key.
	sessionID := rfc2866.AcctSessionID_GetString(r.Packet)
	if sessionID == "" {
		// Without it there is nothing to key a session on, and guessing would
		// mean attributing usage to the wrong row.
		radiusAcctProcessed.WithLabelValues(status, "no_session_id").Inc()
		log.Warn().Str("nas", accountingNASIP(r)).
			Msg("radius: accounting request with no Acct-Session-Id — ignored")
		return
	}

	// Deduplication keyed on what actually identifies a retransmit: the same
	// session, the same record type, the same counters. The previous key used
	// NAS-Identifier — a per-device string, not a per-session one — so two
	// subscribers on one NAS could suppress each other's records.
	//
	// Checked before the store, because a retransmit is a property of the packet
	// stream rather than of what happens to be persisting it: keeping the order
	// the other way made the dedup counter silently depend on whether a store
	// was configured.
	inputOctets, outputOctets := acctOctets(r.Packet)
	dedupKey := "acct_dedup:" + sessionID + ":" + status + ":" +
		strconv.FormatInt(inputOctets, 10) + ":" + strconv.FormatInt(outputOctets, 10)
	if d.redisClient != nil {
		isNew, err := d.redisClient.SetNX(ctx, dedupKey, "1", 30*time.Second).Result()
		if err == nil && !isNew {
			radiusDedupSkipped.Inc()
			return
		}
		// A Redis failure falls through to persist. Double-counting one
		// retransmitted record overwrites a row with the same counters, which is
		// harmless; dropping the record loses the usage outright.
	}

	if d.acctDB == nil {
		radiusAcctProcessed.WithLabelValues(status, "no_store").Inc()
		return
	}

	switch statusType {
	case rfc2866.AcctStatusType_Value_Start:
		d.acctStart(ctx, r, sessionID, status)
	case rfc2866.AcctStatusType_Value_InterimUpdate:
		d.acctUpdate(ctx, sessionID, status, inputOctets, outputOctets)
	case rfc2866.AcctStatusType_Value_Stop:
		d.acctStop(ctx, r, sessionID, status, inputOctets, outputOctets)
	default:
		radiusAcctProcessed.WithLabelValues(status, "ignored").Inc()
	}
}

func (d *RadiusDaemon) acctStart(ctx context.Context, r *radius.Request, sessionID, status string) {
	subscriberID, ok := d.resolveAccountingSubscriber(ctx, r)
	if !ok {
		radiusAcctProcessed.WithLabelValues(status, "unresolved_subscriber").Inc()
		return
	}

	if err := d.acctDB.StartSession(ctx, subscriberID, sessionID,
		accountingNASIP(r), framedIP(r.Packet)); err != nil {
		radiusAcctProcessed.WithLabelValues(status, "error").Inc()
		log.Error().Err(err).Str("session_id", sessionID).Int("subscriber_id", subscriberID).
			Msg("radius: accounting start persist failed")
		return
	}
	radiusAcctProcessed.WithLabelValues(status, "persisted").Inc()
}

func (d *RadiusDaemon) acctUpdate(ctx context.Context, sessionID, status string, in, out int64) {
	matched, err := d.acctDB.UpdateSessionOctets(ctx, sessionID, in, out)
	if err != nil {
		radiusAcctProcessed.WithLabelValues(status, "error").Inc()
		log.Error().Err(err).Str("session_id", sessionID).Msg("radius: interim update persist failed")
		return
	}
	if !matched {
		radiusAcctUnmatched.Inc()
		radiusAcctProcessed.WithLabelValues(status, "unmatched").Inc()
		return
	}
	radiusAcctProcessed.WithLabelValues(status, "persisted").Inc()
}

func (d *RadiusDaemon) acctStop(ctx context.Context, r *radius.Request, sessionID, status string, in, out int64) {
	cause := rfc2866.AcctTerminateCause_Get(r.Packet).String()
	matched, err := d.acctDB.StopSession(ctx, sessionID, in, out, cause)
	if err != nil {
		radiusAcctProcessed.WithLabelValues(status, "error").Inc()
		log.Error().Err(err).Str("session_id", sessionID).Msg("radius: accounting stop persist failed")
		return
	}
	if !matched {
		radiusAcctUnmatched.Inc()
		radiusAcctProcessed.WithLabelValues(status, "unmatched").Inc()
		return
	}
	radiusAcctProcessed.WithLabelValues(status, "persisted").Inc()
}

// resolveAccountingSubscriber maps the record's User-Name to a subscriber id.
//
// Two shapes arrive, matching the two ways a session can have been authorised.
// A MAC-shaped User-Name came from MAC Auth Bypass (FR-HSP-002) and resolves
// through the same hotspot lookup that authorised it; anything else is an
// ordinary subscriber username. Checking MAC first mirrors handleAuth, so a
// session is accounted against the identity that actually authenticated it.
func (d *RadiusDaemon) resolveAccountingSubscriber(ctx context.Context, r *radius.Request) (int, bool) {
	username := rfc2865.UserName_GetString(r.Packet)
	if username == "" {
		return 0, false
	}

	if mac, isMAC := NormaliseMAC(username); isMAC && d.mabDB != nil {
		nasID := 0
		if d.nasResolver != nil {
			nasID = d.nasResolver.ResolveAddr(r.RemoteAddr).ID
		}
		sub, err := d.mabDB.AuthorizeMAC(ctx, mac, nasID)
		if err != nil {
			log.Error().Err(err).Str("mac", mac).Msg("radius: accounting MAC lookup failed")
			return 0, false
		}
		// A voucher-backed grant resolves to id 0 by construction: it has no
		// subscriber row (chk_grant_has_exactly_one_source, migration 034), so
		// there is nothing for session history's foreign key to point at. Those
		// sessions are unrecorded, which is also why a voucher's data cap cannot
		// be enforced yet.
		if sub == nil || sub.ID == 0 {
			return 0, false
		}
		return sub.ID, true
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, username)
	if err != nil || sub == nil {
		if err != nil {
			log.Error().Err(err).Str("username", username).Msg("radius: accounting subscriber lookup failed")
		}
		return 0, false
	}
	return sub.ID, true
}

// acctOctets returns total input and output bytes for the record.
//
// Gigawords are not optional decoration. Acct-Input-Octets is a 32-bit counter
// that wraps every 4 GiB, and the NAS reports each wrap in Acct-Input-Gigawords
// (RFC 2869 §5.1). Reading only the low word makes a subscriber's usage appear
// to reset every 4 GiB — which would break FUP enforcement precisely for the
// heavy users it exists to catch, and in the direction that never triggers it.
func acctOctets(p *radius.Packet) (input, output int64) {
	input = int64(rfc2869.AcctInputGigawords_Get(p))<<32 | int64(rfc2866.AcctInputOctets_Get(p))
	output = int64(rfc2869.AcctOutputGigawords_Get(p))<<32 | int64(rfc2866.AcctOutputOctets_Get(p))
	return input, output
}

// accountingNASIP prefers the NAS-IP-Address the device reports, falling back
// to the source address of the packet.
//
// The attribute is what the NAS calls itself and is what CoA must be sent back
// to; behind NAT the packet's source address may be something else entirely,
// so a CoA aimed at it would arrive nowhere.
func accountingNASIP(r *radius.Request) string {
	if ip := rfc2865.NASIPAddress_Get(r.Packet); ip != nil {
		return ip.String()
	}
	if r.RemoteAddr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr.String()); err == nil {
		return host
	}
	return r.RemoteAddr.String()
}

// framedIP returns the address assigned to the subscriber, or "" when the NAS
// did not supply one. It is what LEA lookups resolve against, so an absent
// value is left empty rather than guessed.
func framedIP(p *radius.Packet) string {
	if ip := rfc2865.FramedIPAddress_Get(p); ip != nil {
		return ip.String()
	}
	return ""
}

// acctStatusLabel names a status type for metrics, keeping the label set
// bounded — an unrecognised type from an exotic NAS must not create a new
// time series per value.
func acctStatusLabel(t rfc2866.AcctStatusType) string {
	switch t {
	case rfc2866.AcctStatusType_Value_Start:
		return "start"
	case rfc2866.AcctStatusType_Value_InterimUpdate:
		return "interim"
	case rfc2866.AcctStatusType_Value_Stop:
		return "stop"
	case rfc2866.AcctStatusType_Value_AccountingOn:
		return "on"
	case rfc2866.AcctStatusType_Value_AccountingOff:
		return "off"
	default:
		return "other"
	}
}
