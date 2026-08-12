//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"
)

// SLA engine persistence tests — FR-SUP-001..003 | MDS §4.13, migration 023.
//
// These exercise the SQL, which is where the SLA logic actually lives: the
// category → priority → policy resolution chain, the routing-rule match, and
// the claim-once behaviour the breach scanner depends on. A stub would test
// none of it.

// TestFR_SUP_001_TicketCreation_AppliesCategoryDefaultSLA verifies a ticket
// created without an explicit priority takes its category's default and gets
// both deadlines computed from the seeded policy.
func TestFR_SUP_001_TicketCreation_AppliesCategoryDefaultSLA(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_default"})

	// connectivity defaults to 'high' (migration 023), whose policy is
	// 60-minute response / 480-minute resolution.
	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "No link light", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	if created.Priority != "high" {
		t.Errorf("priority = %q, want %q (the connectivity default)", created.Priority, "high")
	}
	if created.SLAResponseDueAt == nil || created.SLAResolutionDueAt == nil {
		t.Fatal("both SLA deadlines must be set at creation")
	}

	gotResponse := created.SLAResponseDueAt.Sub(created.CreatedAt).Round(time.Minute)
	if gotResponse != 60*time.Minute {
		t.Errorf("response window = %v, want 60m", gotResponse)
	}
	gotResolution := created.SLAResolutionDueAt.Sub(created.CreatedAt).Round(time.Minute)
	if gotResolution != 480*time.Minute {
		t.Errorf("resolution window = %v, want 480m", gotResolution)
	}
}

// TestFR_SUP_001_StaffPriorityOverride_UsesThatPolicy verifies an explicit
// priority wins over the category default and selects a different policy row.
func TestFR_SUP_001_StaffPriorityOverride_UsesThatPolicy(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_override"})

	critical := "critical"
	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Whole street down", &critical)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	if created.Priority != "critical" {
		t.Errorf("priority = %q, want the override %q", created.Priority, "critical")
	}
	// critical connectivity is 15m / 240m, tighter than high's 60m / 480m.
	if got := created.SLAResponseDueAt.Sub(created.CreatedAt).Round(time.Minute); got != 15*time.Minute {
		t.Errorf("response window = %v, want 15m", got)
	}
}

// TestFR_SUP_003_TicketCreation_ResolvesRoutingRole verifies the routing rule
// seeded for each category is applied at creation.
func TestFR_SUP_003_TicketCreation_ResolvesRoutingRole(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_routing"})

	// Migration 023 routes connectivity → technician, billing → csr.
	for category, wantRole := range map[string]string{
		"connectivity": "technician",
		"billing":      "csr",
	} {
		created, err := database.Tickets().CreateTicketAdmin(ctx, 1, category, "routing check", nil)
		if err != nil {
			t.Fatalf("CreateTicketAdmin(%s): %v", category, err)
		}
		if created.RoutedRole == nil {
			t.Fatalf("%s ticket has no routed_role", category)
		}
		if *created.RoutedRole != wantRole {
			t.Errorf("%s routed to %q, want %q", category, *created.RoutedRole, wantRole)
		}
	}
}

// TestFR_SUP_001_PriorityChange_ReanchorsToOriginalCreatedAt is the rule that
// keeps an SLA clock meaningful: re-triage recomputes the window, but from
// the ticket's original creation time, so repeated updates cannot push a
// deadline out indefinitely (MDS §4.13).
func TestFR_SUP_001_PriorityChange_ReanchorsToOriginalCreatedAt(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_reanchor"})

	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Slow", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	// Backdate creation by an hour so "anchored to created_at" and "anchored
	// to now" produce visibly different answers.
	if _, err := pool.Exec(ctx,
		`UPDATE tickets SET created_at = created_at - INTERVAL '1 hour' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("backdate ticket: %v", err)
	}

	critical := "critical"
	updated, err := database.Tickets().UpdateTicketAdmin(ctx, created.ID, nil, nil, &critical)
	if err != nil {
		t.Fatalf("UpdateTicketAdmin: %v", err)
	}

	// critical connectivity response is 15m from creation. Creation is now an
	// hour ago, so the deadline must be in the past — 45 minutes ago.
	// Anchoring to now() instead would put it 15 minutes in the future.
	if !updated.SLAResponseDueAt.Before(time.Now()) {
		t.Errorf("response deadline %v is in the future; it was re-anchored to now() rather than created_at",
			updated.SLAResponseDueAt)
	}
	if got := updated.SLAResponseDueAt.Sub(updated.CreatedAt).Round(time.Minute); got != 15*time.Minute {
		t.Errorf("response window = %v from created_at, want 15m", got)
	}
}

// TestFR_SUP_002_ClaimSLAEvents_RecordsBreachesExactlyOnce covers the
// idempotency the whole alerting design rests on: the scanner runs every 5
// minutes, and a breached ticket must produce one alert, not one per scan.
func TestFR_SUP_002_ClaimSLAEvents_RecordsBreachesExactlyOnce(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_breach"})

	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Down", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	// Push both deadlines into the past, leaving the ticket open.
	if _, err := pool.Exec(ctx, `
		UPDATE tickets
		SET created_at            = NOW() - INTERVAL '10 hours',
		    sla_response_due_at   = NOW() - INTERVAL '9 hours',
		    sla_resolution_due_at = NOW() - INTERVAL '2 hours'
		WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("age the ticket: %v", err)
	}

	first, err := database.SLA().ClaimSLAEvents(ctx, 0.8)
	if err != nil {
		t.Fatalf("first ClaimSLAEvents: %v", err)
	}
	// All four thresholds are behind us: both warnings and both breaches.
	if len(first) != 4 {
		t.Fatalf("first scan claimed %d events (%+v), want 4", len(first), first)
	}
	for _, e := range first {
		if e.TicketID != created.ID {
			t.Errorf("event for ticket %d, want %d", e.TicketID, created.ID)
		}
		if e.SubscriberID != 1 {
			t.Errorf("event subscriber = %d, want 1", e.SubscriberID)
		}
	}

	second, err := database.SLA().ClaimSLAEvents(ctx, 0.8)
	if err != nil {
		t.Fatalf("second ClaimSLAEvents: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second scan claimed %d events, want 0 — an alert would fire again every 5 minutes forever", len(second))
	}
}

// TestFR_SUP_002_ResolvedTicketStopsAccruingEvents verifies the resolution
// clock stops when the ticket actually closes.
func TestFR_SUP_002_ResolvedTicketStopsAccruingEvents(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_resolved"})

	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Down", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tickets
		SET status                = 'resolved',
		    created_at            = NOW() - INTERVAL '10 hours',
		    sla_response_due_at   = NOW() - INTERVAL '9 hours',
		    sla_resolution_due_at = NOW() - INTERVAL '2 hours'
		WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("resolve and age the ticket: %v", err)
	}

	events, err := database.SLA().ClaimSLAEvents(ctx, 0.8)
	if err != nil {
		t.Fatalf("ClaimSLAEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("a resolved ticket produced %d SLA events (%+v), want 0", len(events), events)
	}
}

// TestFR_SUP_002_WarningFiresBeforeBreach verifies the 80% warning is a
// genuinely earlier signal, not one that only appears once the deadline has
// already passed.
func TestFR_SUP_002_WarningFiresBeforeBreach(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_warning"})

	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Flaky", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	// 100-minute window, 90 minutes elapsed: past the 80-minute warning
	// point, short of the deadline itself.
	if _, err := pool.Exec(ctx, `
		UPDATE tickets
		SET created_at            = NOW() - INTERVAL '90 minutes',
		    sla_response_due_at   = NOW() + INTERVAL '10 minutes',
		    sla_resolution_due_at = NOW() + INTERVAL '10 hours'
		WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("age the ticket: %v", err)
	}

	events, err := database.SLA().ClaimSLAEvents(ctx, 0.8)
	if err != nil {
		t.Fatalf("ClaimSLAEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("claimed %d events (%+v), want exactly 1 (the response warning)", len(events), events)
	}
	if events[0].EventType != "response_warning" {
		t.Errorf("event type = %q, want %q", events[0].EventType, "response_warning")
	}
}

// TestFR_SUP_001_UnknownCategoryFailsLoudly verifies a category with no
// priority default is a hard error rather than a ticket created with no SLA
// at all — the failure mode MDS §4.13 explicitly rejects.
func TestFR_SUP_001_UnknownCategoryFailsLoudly(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_badcat"})

	if _, err := database.Tickets().CreateTicketAdmin(ctx, 1, "not_a_real_category", "x", nil); err == nil {
		t.Error("want an error for a category with no category_priority_defaults row")
	}
}

// TestFR_SUP_002_CountOpenSLABreaches_CountsLiveTicketsNotHistory verifies the
// dashboard counter reflects current trouble, so resolving a breached ticket
// clears it rather than leaving a permanent number.
func TestFR_SUP_002_CountOpenSLABreaches_CountsLiveTicketsNotHistory(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sla_counter"})

	created, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "Down", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tickets
		SET sla_response_due_at   = NOW() - INTERVAL '1 hour',
		    sla_resolution_due_at = NOW() - INTERVAL '1 hour'
		WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("age the ticket: %v", err)
	}

	response, resolution, err := database.SLA().CountOpenSLABreaches(ctx)
	if err != nil {
		t.Fatalf("CountOpenSLABreaches: %v", err)
	}
	if response != 1 || resolution != 1 {
		t.Errorf("breach counts = (%d response, %d resolution), want (1, 1)", response, resolution)
	}

	resolved := "resolved"
	if _, err := database.Tickets().UpdateTicketAdmin(ctx, created.ID, &resolved, nil, nil); err != nil {
		t.Fatalf("resolve ticket: %v", err)
	}

	response, resolution, err = database.SLA().CountOpenSLABreaches(ctx)
	if err != nil {
		t.Fatalf("CountOpenSLABreaches after resolve: %v", err)
	}
	if response != 0 || resolution != 0 {
		t.Errorf("after resolving, breach counts = (%d, %d), want (0, 0)", response, resolution)
	}
}
