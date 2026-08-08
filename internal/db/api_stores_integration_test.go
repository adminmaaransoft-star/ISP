//go:build integration

// Integration tests for the persistence added to wire the previously-stub API
// endpoints: wallet ledger listing, invoice listing/detail, admin ticket
// management, admin session control, and LEA lookup.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
)

// ── Wallet ledger listing ────────────────────────────────────────────────────

// TestBillingStore_ListLedgerEntries verifies entries come back newest first
// and that the from/to window filters correctly.
//
// API §7 GET /api/v1/wallets/{subscriber_id}/ledger
func TestBillingStore_ListLedgerEntries(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "ledger@isp"})

	store := database.Billing()
	if _, err := store.RecordRecharge(ctx, posting(1, "500.00", "500.00", "pay_1", nil)); err != nil {
		t.Fatalf("RecordRecharge 1: %v", err)
	}
	if _, err := store.RecordRecharge(ctx, posting(1, "299.00", "799.00", "pay_2", nil)); err != nil {
		t.Fatalf("RecordRecharge 2: %v", err)
	}

	t.Run("returns all legs newest first", func(t *testing.T) {
		entries, err := store.ListLedgerEntries(ctx, 1, nil, nil, 50)
		if err != nil {
			t.Fatalf("ListLedgerEntries: %v", err)
		}
		// Two recharges, two legs each.
		if len(entries) != 4 {
			t.Fatalf("want 4 ledger entries, got %d", len(entries))
		}
		if !entries[0].CreatedAt.After(entries[len(entries)-1].CreatedAt) &&
			!entries[0].CreatedAt.Equal(entries[len(entries)-1].CreatedAt) {
			t.Error("entries must be ordered newest first")
		}
	})

	t.Run("a future from-window excludes everything", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		entries, err := store.ListLedgerEntries(ctx, 1, &future, nil, 50)
		if err != nil {
			t.Fatalf("ListLedgerEntries: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("want 0 entries after a future cutoff, got %d", len(entries))
		}
	})

	t.Run("limit is honoured", func(t *testing.T) {
		entries, err := store.ListLedgerEntries(ctx, 1, nil, nil, 2)
		if err != nil {
			t.Fatalf("ListLedgerEntries: %v", err)
		}
		if len(entries) != 2 {
			t.Errorf("want 2 entries with limit=2, got %d", len(entries))
		}
	})

	t.Run("unknown subscriber returns an empty slice, not an error", func(t *testing.T) {
		entries, err := store.ListLedgerEntries(ctx, 999999, nil, nil, 50)
		if err != nil {
			t.Fatalf("ListLedgerEntries: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("want 0 entries, got %d", len(entries))
		}
	})
}

// ── Invoices ─────────────────────────────────────────────────────────────────

// TestBillingStore_ListAndDetailInvoices verifies list and detail projections, including
// the current-FUP-state speed summary the PDF template consumes.
//
// API §7 GET /api/v1/invoices/{subscriber_id}, GET /api/v1/invoices/{invoice_id}/pdf
func TestBillingStore_ListAndDetailInvoices(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "100 Mbps Unlimited", "100M/100M", 3_543_348_019_200, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "invoicee@isp"})
	seedGstRate(ctx, t, pool, 1)

	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used, created_at)
		VALUES (1, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 950.25, NOW() - INTERVAL '2 days'),
		       (1, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 1200.00, NOW())`); err != nil {
		t.Fatalf("seed invoices: %v", err)
	}

	store := database.Billing()

	t.Run("list returns both invoices newest first", func(t *testing.T) {
		invoices, err := store.ListInvoices(ctx, 1)
		if err != nil {
			t.Fatalf("ListInvoices: %v", err)
		}
		if len(invoices) != 2 {
			t.Fatalf("want 2 invoices, got %d", len(invoices))
		}
		if !invoices[0].CreatedAt.After(invoices[1].CreatedAt) {
			t.Error("invoices must be ordered newest first")
		}
		assertDecimalEqual(t, "total_amount", mustDecimal(t, invoices[0].TotalAmount), "942.82")
	})

	t.Run("detail carries the full-speed summary when not throttled", func(t *testing.T) {
		invoices, _ := store.ListInvoices(ctx, 1)
		detail, err := store.GetInvoiceDetail(ctx, invoices[0].ID)
		if err != nil {
			t.Fatalf("GetInvoiceDetail: %v", err)
		}
		if detail == nil {
			t.Fatal("want a detail row, got nil")
		}
		if detail.SubscriberName != "invoicee@isp" {
			t.Errorf("subscriber_name: got %q", detail.SubscriberName)
		}
		if detail.PlanName != "100 Mbps Unlimited" {
			t.Errorf("plan_name: got %q", detail.PlanName)
		}
		if detail.FUPApplied {
			t.Error("want fup_applied=false for a subscriber who is not throttled")
		}
		if detail.SpeedActive != "100M/100M" {
			t.Errorf("speed_active: want the full plan rate, got %q", detail.SpeedActive)
		}
	})

	t.Run("detail reflects the throttled speed once fup_active is set", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET fup_active = TRUE WHERE id = 1`); err != nil {
			t.Fatalf("set fup_active: %v", err)
		}
		invoices, _ := store.ListInvoices(ctx, 1)
		detail, err := store.GetInvoiceDetail(ctx, invoices[0].ID)
		if err != nil {
			t.Fatalf("GetInvoiceDetail: %v", err)
		}
		if !detail.FUPApplied {
			t.Error("want fup_applied=true once the subscriber is throttled")
		}
		if detail.SpeedActive != "10M/10M" {
			t.Errorf("speed_active: want the FUP throttle rate, got %q", detail.SpeedActive)
		}
	})

	t.Run("unknown invoice returns (nil, nil)", func(t *testing.T) {
		detail, err := store.GetInvoiceDetail(ctx, 999999)
		if err != nil || detail != nil {
			t.Errorf("want (nil, nil), got (%+v, %v)", detail, err)
		}
	})
}

// ── Admin tickets ────────────────────────────────────────────────────────────

// TestTicketStore_AdminLifecycle verifies creation on behalf of an arbitrary
// subscriber and a partial update that leaves untouched fields alone.
//
// API §7 POST /api/v1/tickets, PATCH /api/v1/tickets/{ticket_id}
func TestTicketStore_AdminLifecycle(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "ticketed@isp"})

	store := database.Tickets()

	created, err := store.CreateTicketAdmin(ctx, 1, "connectivity", "No internet since morning")
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}
	if created.SubscriberID != 1 || created.Status != "open" {
		t.Errorf("created ticket: %+v", created)
	}
	if created.AssignedTo != nil {
		t.Error("a freshly created ticket must have no assignee")
	}

	t.Run("partial update touches only the given fields", func(t *testing.T) {
		status := "in_progress"
		updated, err := store.UpdateTicketAdmin(ctx, created.ID, &status, nil)
		if err != nil {
			t.Fatalf("UpdateTicketAdmin status: %v", err)
		}
		if updated.Status != "in_progress" {
			t.Errorf("status: want in_progress, got %q", updated.Status)
		}
		if updated.Description != "No internet since morning" {
			t.Error("description must be untouched by a status-only update")
		}

		tech := 42
		updated, err = store.UpdateTicketAdmin(ctx, created.ID, nil, &tech)
		if err != nil {
			t.Fatalf("UpdateTicketAdmin assignee: %v", err)
		}
		if updated.AssignedTo == nil || *updated.AssignedTo != 42 {
			t.Errorf("assigned_to: want 42, got %v", updated.AssignedTo)
		}
		if updated.Status != "in_progress" {
			t.Error("status must be untouched by an assignee-only update")
		}
	})

	t.Run("updating an unknown ticket returns (nil, nil)", func(t *testing.T) {
		status := "resolved"
		got, err := store.UpdateTicketAdmin(ctx, 999999, &status, nil)
		if err != nil || got != nil {
			t.Errorf("want (nil, nil), got (%+v, %v)", got, err)
		}
	})

	t.Run("an invalid category is rejected by the schema", func(t *testing.T) {
		if _, err := store.CreateTicketAdmin(ctx, 1, "not_a_real_category", "x"); err == nil {
			t.Error("expected the tickets category CHECK to reject an unknown category")
		}
	})
}

// ── Admin session control ───────────────────────────────────────────────────

// TestFUPStore_ResolveSessionSubscriber verifies the session_id -> subscriber
// lookup that DisconnectSession and FUPOverride depend on, and that a closed
// session is not resolvable.
//
// API §7 POST /api/v1/sessions/{session_id}/disconnect
func TestFUPStore_ResolveSessionSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "control@isp"})

	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history (subscriber_id, session_id, nas_ip_address, start_time)
		VALUES (1, 'live-session-1', '10.20.0.5'::inet, NOW() - INTERVAL '10 minutes')`); err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history (subscriber_id, session_id, nas_ip_address, start_time, stop_time)
		VALUES (1, 'closed-session-1', '10.20.0.5'::inet, NOW() - INTERVAL '2 hours', NOW() - INTERVAL '1 hour')`); err != nil {
		t.Fatalf("seed closed session: %v", err)
	}

	store := database.FUP()

	t.Run("a live session resolves to its subscriber and NAS", func(t *testing.T) {
		subscriberID, nasIP, err := store.ResolveSessionSubscriber(ctx, "live-session-1")
		if err != nil {
			t.Fatalf("ResolveSessionSubscriber: %v", err)
		}
		if subscriberID != 1 {
			t.Errorf("subscriber_id: want 1, got %d", subscriberID)
		}
		if nasIP != "10.20.0.5" {
			t.Errorf("nas_ip: want 10.20.0.5, got %q", nasIP)
		}
	})

	t.Run("a closed session is not resolvable", func(t *testing.T) {
		if _, _, err := store.ResolveSessionSubscriber(ctx, "closed-session-1"); err == nil {
			t.Error("expected an error for a session that has already stopped")
		}
	})

	t.Run("an unknown session_id is not resolvable", func(t *testing.T) {
		if _, _, err := store.ResolveSessionSubscriber(ctx, "never-existed"); err == nil {
			t.Error("expected an error for an unknown session_id")
		}
	})
}

// ── LEA lookup ───────────────────────────────────────────────────────────────

// TestFUPStore_LookupByPublicIP_DirectIP verifies the direct-IP path: a match
// within the session's active window, and no match once the session has ended
// or before it started.
//
// FR-OBS-003 | API §7 POST /api/v1/lea/lookup
func TestFUPStore_LookupByPublicIP_DirectIP(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "lea-target@isp"})

	// Relative to now, not a fixed calendar date: subscriber_session_history is
	// partitioned monthly, and migration 010 only pre-creates the current month
	// plus three ahead — an arbitrary historical date would fall in a month with
	// no partition and the insert would fail outright.
	start := time.Now().UTC().Add(-6 * time.Hour)
	stop := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history (subscriber_id, session_id, nas_ip_address, assigned_ipv4, start_time, stop_time)
		VALUES (1, 'lea-sess-1', '10.30.0.1'::inet, '203.0.113.5'::inet, $1, $2)`, start, stop); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	store := database.FUP()

	t.Run("a timestamp inside the session window matches", func(t *testing.T) {
		result, err := store.LookupByPublicIP(ctx, "203.0.113.5", nil, start.Add(1*time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result == nil {
			t.Fatal("want a match, got nil")
		}
		if result.SubscriberID != 1 || result.Username != "lea-target@isp" {
			t.Errorf("result: %+v", result)
		}
		if result.Source != "direct_ip" {
			t.Errorf("source: want direct_ip, got %q", result.Source)
		}
		if result.CAFNumber == "" {
			t.Error("caf_number must be populated for LEA identity")
		}
	})

	t.Run("a timestamp after the session ended does not match", func(t *testing.T) {
		result, err := store.LookupByPublicIP(ctx, "203.0.113.5", nil, stop.Add(time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result != nil {
			t.Errorf("want no match after the session ended, got %+v", result)
		}
	})

	t.Run("a timestamp before the session started does not match", func(t *testing.T) {
		result, err := store.LookupByPublicIP(ctx, "203.0.113.5", nil, start.Add(-time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result != nil {
			t.Errorf("want no match before the session started, got %+v", result)
		}
	})

	t.Run("an unassigned IP does not match", func(t *testing.T) {
		result, err := store.LookupByPublicIP(ctx, "198.51.100.9", nil, start.Add(time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result != nil {
			t.Errorf("want no match for an unused IP, got %+v", result)
		}
	})
}

// TestFUPStore_LookupByPublicIP_CGNAT verifies the CGNAT path: a match
// requires the port to fall inside the allocated block, not just the IP.
//
// FR-OBS-003 | API §7 POST /api/v1/lea/lookup
func TestFUPStore_LookupByPublicIP_CGNAT(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "cgnat-target@isp"})

	// Relative to now for the same reason as the direct-IP test above:
	// cgnat_allocations is partitioned monthly too.
	allocated := time.Now().UTC().Add(-6 * time.Hour)
	released := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cgnat_allocations (subscriber_id, public_ip, port_start, port_end, nas_ip_address, allocated_at, released_at)
		VALUES (1, '198.51.100.20'::inet, 40000, 40099, '10.30.0.1'::inet, $1, $2)`, allocated, released); err != nil {
		t.Fatalf("seed cgnat allocation: %v", err)
	}

	store := database.FUP()

	t.Run("a port inside the block matches", func(t *testing.T) {
		port := 40050
		result, err := store.LookupByPublicIP(ctx, "198.51.100.20", &port, allocated.Add(time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result == nil {
			t.Fatal("want a match, got nil")
		}
		if result.Source != "cgnat" {
			t.Errorf("source: want cgnat, got %q", result.Source)
		}
		if result.SubscriberID != 1 {
			t.Errorf("subscriber_id: want 1, got %d", result.SubscriberID)
		}
	})

	t.Run("a port outside the block does not match", func(t *testing.T) {
		port := 50000
		result, err := store.LookupByPublicIP(ctx, "198.51.100.20", &port, allocated.Add(time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result != nil {
			t.Errorf("want no match for a port outside the allocated range, got %+v", result)
		}
	})

	t.Run("a timestamp after release does not match", func(t *testing.T) {
		port := 40050
		result, err := store.LookupByPublicIP(ctx, "198.51.100.20", &port, released.Add(time.Hour))
		if err != nil {
			t.Fatalf("LookupByPublicIP: %v", err)
		}
		if result != nil {
			t.Errorf("want no match after the block was released, got %+v", result)
		}
	})
}

// TestFUPStore_RecordLEAAudit verifies the append-only audit row is written
// for both a hit and a miss, matching FR-OBS-003's "every invocation" requirement.
func TestFUPStore_RecordLEAAudit(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	store := database.FUP()
	when := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)

	t.Run("a hit records the resolved subscriber and a row count of 1", func(t *testing.T) {
		subID := 7
		if err := store.RecordLEAAudit(ctx, api.LEAAuditEntry{
			AccessorIdentity:   "lea-officer-1",
			AccessorRole:       "noc_engineer",
			QueriedPublicIP:    "203.0.113.5",
			QueriedTimestamp:   when,
			ResultSubscriberID: &subID,
			ResultRowCount:     1,
		}); err != nil {
			t.Fatalf("RecordLEAAudit: %v", err)
		}

		count := countRows(ctx, t, pool, `SELECT COUNT(*) FROM lea_audit_log WHERE accessor_identity = 'lea-officer-1'`)
		if count != 1 {
			t.Fatalf("want 1 audit row, got %d", count)
		}
		resultCount := scanString(ctx, t, pool,
			`SELECT result_row_count::text FROM lea_audit_log WHERE accessor_identity = 'lea-officer-1'`)
		if resultCount != "1" {
			t.Errorf("result_row_count: want 1, got %s", resultCount)
		}
	})

	t.Run("a miss still writes a row, with a null result_subscriber_id", func(t *testing.T) {
		if err := store.RecordLEAAudit(ctx, api.LEAAuditEntry{
			AccessorIdentity:   "lea-officer-2",
			AccessorRole:       "noc_engineer",
			QueriedPublicIP:    "198.51.100.99",
			QueriedTimestamp:   when,
			ResultSubscriberID: nil,
			ResultRowCount:     0,
		}); err != nil {
			t.Fatalf("RecordLEAAudit: %v", err)
		}

		count := countRows(ctx, t, pool,
			`SELECT COUNT(*) FROM lea_audit_log WHERE accessor_identity = 'lea-officer-2' AND result_subscriber_id IS NULL`)
		if count != 1 {
			t.Errorf("want 1 audit row with a null result_subscriber_id, got %d", count)
		}
	})
}

// ── Usage history (portal UI Phase 2) ───────────────────────────────────────

// TestPortalStore_ListSessionHistory verifies newest-first ordering, that
// GB is computed from input+output octets using the same 1024^3 divisor
// internal/cache.SessionStore.PortalSession uses for live sessions, that a
// currently-active session (stop_time NULL) is included with a nil StopTime,
// and that another subscriber's sessions never leak into the result.
//
// FR-SUB-001 | MDS §4.9
func TestPortalStore_ListSessionHistory(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "usage-history@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "other-subscriber@isp"})

	// Relative to now, not a fixed calendar date — see the identical comment
	// on TestFUPStore_LookupByPublicIP_DirectIP above: subscriber_session_history
	// is partitioned monthly and only has partitions for the current month
	// plus three ahead.
	older := time.Now().UTC().Add(-6 * time.Hour)
	olderStop := time.Now().UTC().Add(-5 * time.Hour)
	newer := time.Now().UTC().Add(-2 * time.Hour)
	newerStop := time.Now().UTC().Add(-1 * time.Hour)
	active := time.Now().UTC().Add(-10 * time.Minute)

	seedSession := func(subscriberID int, sessionID string, start time.Time, stop *time.Time, inputOctets, outputOctets int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO subscriber_session_history
				(subscriber_id, session_id, nas_ip_address, start_time, stop_time, input_octets, output_octets, terminate_cause)
			VALUES ($1, $2, '10.40.0.1'::inet, $3, $4, $5, $6, NULLIF($7, ''))`,
			subscriberID, sessionID, start, stop, inputOctets, outputOctets, "user-request"); err != nil {
			t.Fatalf("seed session %s: %v", sessionID, err)
		}
	}

	seedSession(1, "hist-older", older, &olderStop, 1073741824, 1073741824) // 2 GiB total
	seedSession(1, "hist-newer", newer, &newerStop, 0, 0)
	seedSession(1, "hist-active", active, nil, 0, 0)
	seedSession(2, "other-subscribers-session", older, &olderStop, 0, 0)

	store := database.Portal()

	t.Run("returns this subscriber's sessions newest first", func(t *testing.T) {
		entries, err := store.ListSessionHistory(ctx, 1, 50)
		if err != nil {
			t.Fatalf("ListSessionHistory: %v", err)
		}
		if len(entries) != 3 {
			t.Fatalf("want 3 entries, got %d: %+v", len(entries), entries)
		}
		wantOrder := []string{"hist-active", "hist-newer", "hist-older"}
		for i, id := range wantOrder {
			if entries[i].SessionID != id {
				t.Errorf("entry %d: want session_id %q, got %q", i, id, entries[i].SessionID)
			}
		}
	})

	t.Run("an active session has a nil StopTime", func(t *testing.T) {
		entries, err := store.ListSessionHistory(ctx, 1, 50)
		if err != nil {
			t.Fatalf("ListSessionHistory: %v", err)
		}
		if entries[0].SessionID != "hist-active" {
			t.Fatalf("expected hist-active first, got %q", entries[0].SessionID)
		}
		if entries[0].StopTime != nil {
			t.Errorf("want nil StopTime for an active session, got %v", entries[0].StopTime)
		}
	})

	t.Run("GB used is computed from input+output octets", func(t *testing.T) {
		entries, err := store.ListSessionHistory(ctx, 1, 50)
		if err != nil {
			t.Fatalf("ListSessionHistory: %v", err)
		}
		var gbUsed string
		var found bool
		for _, e := range entries {
			if e.SessionID == "hist-older" {
				gbUsed, found = e.GBUsed.String(), true
			}
		}
		if !found {
			t.Fatal("hist-older not found in results")
		}
		if gbUsed != "2" {
			t.Errorf("want 2 GB (2 GiB in, matching internal/cache's 1024^3 divisor), got %s", gbUsed)
		}
	})

	t.Run("another subscriber's sessions never appear", func(t *testing.T) {
		entries, err := store.ListSessionHistory(ctx, 2, 50)
		if err != nil {
			t.Fatalf("ListSessionHistory: %v", err)
		}
		if len(entries) != 1 || entries[0].SessionID != "other-subscribers-session" {
			t.Fatalf("want exactly subscriber 2's own session, got %+v", entries)
		}
		for _, e := range entries {
			if e.SessionID == "hist-older" || e.SessionID == "hist-newer" || e.SessionID == "hist-active" {
				t.Errorf("subscriber 1's session %q leaked into subscriber 2's history", e.SessionID)
			}
		}
	})

	t.Run("a limit of zero falls back to the default", func(t *testing.T) {
		entries, err := store.ListSessionHistory(ctx, 1, 0)
		if err != nil {
			t.Fatalf("ListSessionHistory: %v", err)
		}
		if len(entries) != 3 {
			t.Errorf("want the default limit to still return all 3 seeded sessions, got %d", len(entries))
		}
	})
}
