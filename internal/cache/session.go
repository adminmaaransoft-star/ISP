// Package cache holds the Redis-backed live session state that the RADIUS
// daemon writes on accounting and that the health endpoint and subscriber
// portal read.
//
// Live session state lives in Redis rather than PostgreSQL because it is read
// on every health check and dashboard load but is worthless once the session
// ends. Redis is treated as a cache throughout: every reader degrades to
// "no active session" when it is unavailable, so a Redis outage never takes down
// the diagnostic endpoint that support needs during precisely that outage.
//
// DDS §5.9 | IDD §8.4
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/shopspring/decimal"
)

// SessionTTL bounds how long a session record survives without an accounting
// update. Interim-Updates arrive every 5 minutes, so a record older than this
// belongs to a session whose Accounting-Stop was lost.
const SessionTTL = 30 * time.Minute

// SessionKey returns the Redis key holding a subscriber's live session.
func SessionKey(subscriberID int) string {
	return "session:active:" + strconv.Itoa(subscriberID)
}

// Session is the stored representation of a live RADIUS session.
type Session struct {
	SessionID    string    `json:"session_id"`
	SubscriberID int       `json:"subscriber_id"`
	NasIP        string    `json:"nas_ip"`
	AssignedIP   string    `json:"assigned_ip"`
	BytesIn      int64     `json:"bytes_in"`
	BytesOut     int64     `json:"bytes_out"`
	BytesTotal   int64     `json:"bytes_total"` // plan quota; 0 = unlimited
	SpeedProfile string    `json:"speed_profile"`
	FUPThrottled bool      `json:"fup_throttled"`
	StartedAt    time.Time `json:"started_at"`
}

// BytesUsed is the combined upstream and downstream usage.
func (s *Session) BytesUsed() int64 { return s.BytesIn + s.BytesOut }

// PctUsed is the percentage of quota consumed, 0 for an unlimited plan.
//
// Computed with decimal rather than float division: this value drives the FUP
// banding, and a subscriber must not be reported as throttled because of a
// floating-point rounding artefact at the boundary.
func (s *Session) PctUsed() int {
	if s.BytesTotal <= 0 {
		return 0
	}
	return int(decimal.NewFromInt(s.BytesUsed()).
		Mul(decimal.NewFromInt(100)).
		Div(decimal.NewFromInt(s.BytesTotal)).
		IntPart())
}

// SessionStore reads and writes live session state.
type SessionStore struct {
	rc redis.UniversalClient
}

// NewSessionStore constructs a SessionStore.
func NewSessionStore(rc redis.UniversalClient) *SessionStore {
	return &SessionStore{rc: rc}
}

var (
	_ health.RedisQuerier = (*SessionStore)(nil)
	_ api.SessionReader   = (*SessionStore)(nil)
)

// PortalView adapts SessionStore to the portal's session interface.
//
// health.RedisQuerier and portal.PortalSessionQuerier both declare
// GetActiveSession but return different types, which no single Go type can
// satisfy — hence the adapter rather than a second method name.
type PortalView struct{ store *SessionStore }

var _ portal.PortalSessionQuerier = (*PortalView)(nil)

// Portal returns the portal-facing view of live session state.
func (s *SessionStore) Portal() *PortalView { return &PortalView{store: s} }

// GetActiveSession implements portal.PortalSessionQuerier.
func (p *PortalView) GetActiveSession(ctx context.Context, subscriberID int) (*portal.ActiveSession, error) {
	return p.store.PortalSession(ctx, subscriberID)
}

// Put stores a session and refreshes its TTL.
func (s *SessionStore) Put(ctx context.Context, sess Session) error {
	payload, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("cache: marshal session: %w", err)
	}
	if err := s.rc.Set(ctx, SessionKey(sess.SubscriberID), payload, SessionTTL).Err(); err != nil {
		return fmt.Errorf("cache: store session for subscriber %d: %w", sess.SubscriberID, err)
	}
	return nil
}

// Delete removes a session at Accounting-Stop.
func (s *SessionStore) Delete(ctx context.Context, subscriberID int) error {
	if err := s.rc.Del(ctx, SessionKey(subscriberID)).Err(); err != nil {
		return fmt.Errorf("cache: delete session for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// get loads the raw session, returning (nil, nil) when absent.
func (s *SessionStore) get(ctx context.Context, subscriberID int) (*Session, error) {
	raw, err := s.rc.Get(ctx, SessionKey(subscriberID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil // offline, which is a normal state rather than an error
	}
	if err != nil {
		return nil, fmt.Errorf("cache: read session for subscriber %d: %w", subscriberID, err)
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("cache: unmarshal session for subscriber %d: %w", subscriberID, err)
	}
	return &sess, nil
}

// Get returns the live session for a subscriber, or nil when offline.
func (s *SessionStore) Get(ctx context.Context, subscriberID int) (*Session, error) {
	return s.get(ctx, subscriberID)
}

// GetActiveSession implements health.RedisQuerier.
func (s *SessionStore) GetActiveSession(ctx context.Context, subscriberID int) (*health.SessionSummary, error) {
	sess, err := s.get(ctx, subscriberID)
	if err != nil || sess == nil {
		return nil, err
	}
	return &health.SessionSummary{
		SessionID:    sess.SessionID,
		NasIP:        sess.NasIP,
		AssignedIP:   sess.AssignedIP,
		BytesUsed:    sess.BytesUsed(),
		BytesTotal:   sess.BytesTotal,
		PctUsed:      sess.PctUsed(),
		SpeedProfile: sess.SpeedProfile,
		SessionAge:   formatAge(time.Since(sess.StartedAt)),
	}, nil
}

// PortalSession adapts the stored session to the portal's view.
//
// The portal reports usage in GB because that is the unit a subscriber's plan is
// sold in; the health endpoint keeps raw octets for support diagnostics.
func (s *SessionStore) PortalSession(ctx context.Context, subscriberID int) (*portal.ActiveSession, error) {
	sess, err := s.get(ctx, subscriberID)
	if err != nil || sess == nil {
		return nil, err
	}
	const bytesPerGB = 1024 * 1024 * 1024
	gb := func(b int64) decimal.Decimal {
		return decimal.NewFromInt(b).Div(decimal.NewFromInt(bytesPerGB)).Round(2)
	}
	pct := 0.0
	if sess.BytesTotal > 0 {
		pct, _ = decimal.NewFromInt(sess.BytesUsed()).
			Mul(decimal.NewFromInt(100)).
			Div(decimal.NewFromInt(sess.BytesTotal)).
			Round(2).Float64()
	}
	return &portal.ActiveSession{
		SessionID:    sess.SessionID,
		NASIP:        sess.NasIP,
		AssignedIP:   sess.AssignedIP,
		BytesIn:      sess.BytesIn,
		BytesOut:     sess.BytesOut,
		GBUsed:       gb(sess.BytesUsed()),
		GBIncluded:   gb(sess.BytesTotal),
		PctUsed:      pct,
		FUPThrottled: sess.FUPThrottled,
		StartedAt:    sess.StartedAt,
	}, nil
}

// formatAge renders a session duration as a compact human string.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
