package tr069

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Provisioning — FR-CPE-001..002 | MDS §4.19.

// Provisioning states a device moves through.
const (
	StateUnknown          = "unknown"
	StateRegistered       = "registered"
	StateProvisioned      = "provisioned"
	StateNeedsReprovision = "needs_reprovision"
	StateFault            = "fault"
)

// Task lifecycle, matching cpe_tasks.status.
const (
	TaskPending   = "pending"
	TaskSent      = "sent"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskExpired   = "expired"
)

var (
	InformsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tr069_informs_total",
		Help: "CWMP Inform sessions opened, by primary event code",
	}, []string{"event"})
	TasksDeliveredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tr069_tasks_delivered_total",
		Help: "Queued RPCs handed to a device, by RPC type",
	}, []string{"rpc_type"})
	TaskFaultsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tr069_task_faults_total",
		Help: "RPCs a device rejected, by CWMP fault code",
	}, []string{"fault_code"})
	// UnknownDeviceInformsTotal counts devices that Informed without a
	// warehouse record. A steady trickle is normal (field swaps); a spike
	// usually means somebody is pointing third-party CPE at the ACS.
	UnknownDeviceInformsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tr069_unknown_device_informs_total",
		Help: "Informs from devices with no cpe_devices record",
	})
)

// Device is the ACS view of a managed CPE.
type Device struct {
	ID                   int        `json:"id"`
	SerialNumber         string     `json:"serial_number"`
	OUI                  string     `json:"oui,omitempty"`
	ProductClass         string     `json:"product_class,omitempty"`
	DeviceTypeID         int        `json:"device_type_id"`
	SubscriberID         *int       `json:"subscriber_id,omitempty"`
	ConnectionRequestURL string     `json:"connection_request_url,omitempty"`
	SoftwareVersion      string     `json:"software_version,omitempty"`
	HardwareVersion      string     `json:"hardware_version,omitempty"`
	LastInformAt         *time.Time `json:"last_inform_at,omitempty"`
	LastInformEvent      string     `json:"last_inform_event,omitempty"`
	ProvisioningState    string     `json:"provisioning_state"`
	LastFault            string     `json:"last_fault,omitempty"`
	ACSDiscovered        bool       `json:"acs_discovered"`
}

// Task is one queued RPC.
type Task struct {
	ID          int               `json:"id"`
	DeviceID    int               `json:"device_id"`
	RPCType     string            `json:"rpc_type"`
	Params      map[string]string `json:"params"`
	Status      string            `json:"status"`
	Priority    int               `json:"priority"`
	CreatedBy   string            `json:"created_by"`
	FaultCode   string            `json:"fault_code,omitempty"`
	FaultString string            `json:"fault_string,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	SentAt      *time.Time        `json:"sent_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
}

// ProvisioningContext carries the values a template can substitute.
type ProvisioningContext struct {
	PPPoEUsername  string
	PPPoEPassword  string
	SSID           string
	WiFiPassword   string
	RateLimit      string // the plan's rate, e.g. "100M/100M"
	DownstreamKbps string
	UpstreamKbps   string
	PlanName       string
	SerialNumber   string
}

// RenderTemplate substitutes {{placeholders}} in a device type's
// provisioning template.
//
// The template lives in the database rather than in code because TR-069
// parameter paths differ wildly between models — a TP-Link's SSID is not at
// the same path as a Nokia's, and TR-098 and TR-181 devices disagree about
// the root entirely. Keeping paths in data means a new router model is a row
// rather than a release (FR-CPE-002).
//
// A parameter whose value renders empty is DROPPED, not pushed. This is a
// safety property, not tidiness: subscriber passwords are stored as bcrypt,
// so {{pppoe_password}} usually has no value to substitute — and pushing an
// empty PPPoE password would disconnect the subscriber while reporting a
// successful provisioning. Omitting the parameter leaves whatever the device
// already has, which is the safe failure direction.
//
// (Pushing PPPoE credentials therefore needs a separately stored secret, the
// same constraint that defers CHAP in FR-AAA-005. Templates that only set
// SSID and shaping work fully today.)
func RenderTemplate(template map[string]string, ctx ProvisioningContext) map[string]string {
	replacer := strings.NewReplacer(
		"{{pppoe_username}}", ctx.PPPoEUsername,
		"{{pppoe_password}}", ctx.PPPoEPassword,
		"{{ssid}}", ctx.SSID,
		"{{wifi_password}}", ctx.WiFiPassword,
		"{{rate_limit}}", ctx.RateLimit,
		"{{downstream_kbps}}", ctx.DownstreamKbps,
		"{{upstream_kbps}}", ctx.UpstreamKbps,
		"{{plan_name}}", ctx.PlanName,
		"{{serial_number}}", ctx.SerialNumber,
	)

	out := make(map[string]string, len(template))
	for path, valueTemplate := range template {
		if rendered := replacer.Replace(valueTemplate); rendered != "" {
			out[path] = rendered
		}
	}
	return out
}

// RateLimitToKbps splits a MikroTik-style "100M/100M" rate string into
// upstream and downstream kilobits.
//
// The same plan value already drives the RADIUS rate limit (FR-NAS-001), so
// deriving the CPE-side shaping from it is what keeps the two ends of the
// link agreeing — a CPE shaped to a different number than the NAS is the
// classic cause of "my speed test is wrong" tickets.
func RateLimitToKbps(rateLimit string) (upstreamKbps, downstreamKbps string) {
	parts := strings.Split(rateLimit, "/")
	if len(parts) != 2 {
		return "", ""
	}
	return toKbps(parts[0]), toKbps(parts[1])
}

// toKbps converts "100M" / "512k" to a plain kilobit count.
func toKbps(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	multiplier := 1
	switch {
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		multiplier = 1000
		v = v[:len(v)-1]
	case strings.HasSuffix(v, "k"), strings.HasSuffix(v, "K"):
		v = v[:len(v)-1]
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		multiplier = 1000000
		v = v[:len(v)-1]
	}

	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return "" // unparseable: better empty than a wrong shaping value
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return ""
	}
	return itoa(n * multiplier)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
