// Package crm implements the pre-subscriber lead pipeline: prospect
// tracking, conversion into a paying subscriber, and funnel reporting.
//
// FR: FR-CRM-001..003 | MDS §4.16 | DBD §6.2 leads
package crm

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Lead pipeline stages. A lead moves forward through these, and ends at
// exactly one of converted or lost.
const (
	StatusNew       = "new"
	StatusContacted = "contacted"
	StatusQualified = "qualified"
	StatusConverted = "converted"
	StatusLost      = "lost"
)

// Lead sources — the dimension FR-CRM-003 reports conversion rate by.
var validSources = map[string]bool{
	"walk_in": true, "referral": true, "website": true,
	"campaign": true, "franchise": true, "other": true,
}

var (
	// ErrAlreadyConverted is returned when a lead has already become a
	// subscriber. Returned rather than silently succeeding because the
	// caller losing this race must not go on to create a second subscriber
	// from the same prospect (MDS §4.16).
	ErrAlreadyConverted = errors.New("crm: this lead has already been converted")
	// ErrLeadLost is returned when converting a lead previously marked lost
	// — recoverable by reopening the lead first, but not something to do
	// silently, since it would quietly resurrect a prospect somebody
	// deliberately closed.
	ErrLeadLost = errors.New("crm: this lead was marked lost and must be reopened before converting")
)

var (
	LeadsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_leads_total",
		Help: "Lead pipeline movement, by the status entered",
	}, []string{"status"})
	// ConversionConflictsTotal counts conversions refused because another
	// caller claimed the lead first. Expected to sit near zero; a rising
	// value means two operators are working the same leads.
	ConversionConflictsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crm_lead_conversion_conflicts_total",
		Help: "Lead conversions refused by the atomic claim because the lead was already converted",
	})
)

// Lead is a pre-subscriber prospect.
type Lead struct {
	ID                    int        `json:"id"`
	FullName              string     `json:"full_name"`
	MobileNumber          string     `json:"mobile_number"`
	Email                 string     `json:"email,omitempty"`
	Source                string     `json:"source"`
	Status                string     `json:"status"`
	FranchiseID           *int       `json:"franchise_id,omitempty"`
	AssignedTo            string     `json:"assigned_to,omitempty"`
	Notes                 string     `json:"notes,omitempty"`
	LostReason            string     `json:"lost_reason,omitempty"`
	ConvertedSubscriberID *int       `json:"converted_subscriber_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	ConvertedAt           *time.Time `json:"converted_at,omitempty"`
}

// ValidSource reports whether s is a source the schema will accept.
// Checked in the handler so a bad value is a readable 422 rather than a raw
// constraint violation surfacing as a 500.
func ValidSource(s string) bool { return validSources[s] }

// ValidStatus reports whether s is a legal pipeline stage for a manual
// update. Deliberately excludes "converted": that status is only ever
// reached through the conversion path, which also has to create a
// subscriber and set converted_subscriber_id — letting a plain status
// update reach it would produce a converted lead pointing at nobody, which
// chk_lead_converted_has_subscriber rejects anyway.
func ValidStatus(s string) bool {
	switch s {
	case StatusNew, StatusContacted, StatusQualified, StatusLost:
		return true
	default:
		return false
	}
}

// FunnelStage is one row of the FR-CRM-003 pipeline report.
type FunnelStage struct {
	Source    string `json:"source"`
	Status    string `json:"status"`
	LeadCount int    `json:"lead_count"`
}

// FunnelReport answers FR-CRM-003: how many leads sit at each stage, broken
// down by where they came from, plus the conversion rate that follows.
type FunnelReport struct {
	Stages         []FunnelStage `json:"stages"`
	TotalLeads     int           `json:"total_leads"`
	ConvertedLeads int           `json:"converted_leads"`
	// ConversionRatePct is a string rather than a float so it renders
	// identically everywhere and never picks up binary-floating-point
	// noise in a number people will quote in a meeting.
	ConversionRatePct string `json:"conversion_rate_pct"`
}
