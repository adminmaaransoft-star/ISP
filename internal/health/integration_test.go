//go:build integration

// Integration tests for the single-call subscriber health endpoint.
//
// Covers INT-HEALTH-001 from the Integration Tests tracker sheet: one request
// must assemble DB state and live Redis session state into a single response,
// well inside the NFR-PERF-002 budget of 200ms.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/health -Tags integration
package health_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/shopspring/decimal"
)

// ── Test doubles ────────────────────────────────────────────────────────────

// itHealthDB serves subscriber rows, optionally with a simulated query latency.
type itHealthDB struct {
	records map[int]*health.SubscriberRecord
	latency time.Duration
	err     error
}

func (db *itHealthDB) GetSubscriberWithMeta(ctx context.Context, subscriberID int) (*health.SubscriberRecord, error) {
	if db.latency > 0 {
		select {
		case <-time.After(db.latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if db.err != nil {
		return nil, db.err
	}
	rec, ok := db.records[subscriberID]
	if !ok {
		return nil, fmt.Errorf("subscriber %d not found", subscriberID)
	}
	return rec, nil
}

// itSessionCache serves live session state, optionally with a latency.
type itSessionCache struct {
	sessions map[int]*health.SessionSummary
	latency  time.Duration
	err      error
}

func (c *itSessionCache) GetActiveSession(ctx context.Context, subscriberID int) (*health.SessionSummary, error) {
	if c.latency > 0 {
		select {
		case <-time.After(c.latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	sess, ok := c.sessions[subscriberID]
	if !ok {
		return nil, nil
	}
	return sess, nil
}

// itRequest builds a request carrying the {id} path value the handler reads.
func itRequest(subscriberID int) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/subscribers/%d/health", subscriberID), nil)
	req.SetPathValue("id", fmt.Sprint(subscriberID))
	return req
}

func itSeedSubscriber() *health.SubscriberRecord {
	expiry := time.Now().Add(12 * 24 * time.Hour)
	return &health.SubscriberRecord{
		ID:            501,
		Username:      "diagnose@isp",
		Status:        "active",
		WalletBalance: decimal.RequireFromString("342.50"),
		PlanExpiry:    &expiry,
		OpenTickets:   2,
	}
}

func itSeedSession() *health.SessionSummary {
	return &health.SessionSummary{
		SessionID:    "sess-health-001",
		NasIP:        "10.10.0.1",
		AssignedIP:   "100.64.3.19",
		BytesUsed:    2_834_678_415_360, // 80% of BytesTotal
		BytesTotal:   3_543_348_019_200,
		PctUsed:      80,
		SpeedProfile: "100M/100M",
		SessionAge:   "3h12m",
	}
}

// ── INT-HEALTH-001 ──────────────────────────────────────────────────────────

// TestFR_OBS_004_SubscriberHealth_SingleCallAssembly verifies one request returns 200 with
// the live session assembled from the cache and the account state from the DB,
// within the 200ms response budget.
//
// INT-HEALTH-001 | FR-OBS-004, NFR-PERF-002
func TestFR_OBS_004_SubscriberHealth_SingleCallAssembly(t *testing.T) {
	db := &itHealthDB{records: map[int]*health.SubscriberRecord{501: itSeedSubscriber()}}
	cache := &itSessionCache{sessions: map[int]*health.SessionSummary{501: itSeedSession()}}

	handler := health.NewHandler(db, cache)
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.GetSubscriberHealth(rec, itRequest(501))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("response took %v, over the 200ms NFR-PERF-002 budget", elapsed)
	}

	var resp health.SubscriberHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	// Live session state comes from the cache.
	if resp.ActiveSession == nil {
		t.Fatal("active_session must not be nil for a subscriber with a live session")
	}
	if resp.ActiveSession.SessionID != "sess-health-001" {
		t.Errorf("session_id: want sess-health-001, got %q", resp.ActiveSession.SessionID)
	}
	if resp.ActiveSession.AssignedIP != "100.64.3.19" {
		t.Errorf("assigned_ip: want 100.64.3.19, got %q", resp.ActiveSession.AssignedIP)
	}

	// Account state comes from the DB.
	if resp.SubscriberID != 501 {
		t.Errorf("subscriber_id: want 501, got %d", resp.SubscriberID)
	}
	if resp.Username != "diagnose@isp" {
		t.Errorf("username: want diagnose@isp, got %q", resp.Username)
	}
	if resp.Status != "active" {
		t.Errorf("status: want active, got %q", resp.Status)
	}
	if resp.WalletBalance != "342.5" && resp.WalletBalance != "342.50" {
		t.Errorf("wallet_balance: want 342.50, got %q", resp.WalletBalance)
	}
	if resp.OpenTickets != 2 {
		t.Errorf("open_tickets: want 2, got %d", resp.OpenTickets)
	}
	if resp.PlanExpiry == nil {
		t.Error("plan_expiry must be populated")
	}

	// 80% consumed is a warning, not yet throttled.
	if resp.FupStatus != "warning" {
		t.Errorf("fup_status at 80%%: want warning, got %q", resp.FupStatus)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}
}

// TestSubscriberHealth_FanOutIsConcurrent verifies the DB and cache lookups run
// in parallel: two 80ms dependencies must not add up to 160ms.
//
// INT-HEALTH-001 | NFR-PERF-002
func TestSubscriberHealth_FanOutIsConcurrent(t *testing.T) {
	const dependencyLatency = 80 * time.Millisecond

	db := &itHealthDB{
		records: map[int]*health.SubscriberRecord{501: itSeedSubscriber()},
		latency: dependencyLatency,
	}
	cache := &itSessionCache{
		sessions: map[int]*health.SessionSummary{501: itSeedSession()},
		latency:  dependencyLatency,
	}

	handler := health.NewHandler(db, cache)
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.GetSubscriberHealth(rec, itRequest(501))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	// Sequential lookups would take at least 160ms; concurrent ones stay near 80ms.
	if elapsed >= 2*dependencyLatency {
		t.Errorf("lookups appear sequential: took %v for two %v dependencies", elapsed, dependencyLatency)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("response took %v, over the 200ms NFR-PERF-002 budget", elapsed)
	}
}

// TestFR_OBS_004_SubscriberHealth_FupStatusThresholds verifies the FUP banding reported to
// support staff.
//
// INT-HEALTH-001 (supporting) | FR-OBS-004
func TestFR_OBS_004_SubscriberHealth_FupStatusThresholds(t *testing.T) {
	cases := []struct {
		pctUsed int
		want    string
	}{
		{0, "below"},
		{79, "below"},
		{80, "warning"},
		{99, "warning"},
		{100, "throttled"},
		{140, "throttled"},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%d%%", c.pctUsed), func(t *testing.T) {
			session := itSeedSession()
			session.PctUsed = c.pctUsed

			handler := health.NewHandler(
				&itHealthDB{records: map[int]*health.SubscriberRecord{501: itSeedSubscriber()}},
				&itSessionCache{sessions: map[int]*health.SessionSummary{501: session}},
			)
			rec := httptest.NewRecorder()
			handler.GetSubscriberHealth(rec, itRequest(501))

			var resp health.SubscriberHealth
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.FupStatus != c.want {
				t.Errorf("pct_used=%d: want fup_status %q, got %q", c.pctUsed, c.want, resp.FupStatus)
			}
		})
	}
}

// TestFR_OBS_004_SubscriberHealth_OfflineSubscriber verifies a subscriber with no live
// session still returns their account state, with a null active_session.
//
// INT-HEALTH-001 (supporting) | FR-OBS-004
func TestFR_OBS_004_SubscriberHealth_OfflineSubscriber(t *testing.T) {
	handler := health.NewHandler(
		&itHealthDB{records: map[int]*health.SubscriberRecord{501: itSeedSubscriber()}},
		&itSessionCache{sessions: map[int]*health.SessionSummary{}}, // nobody online
	)
	rec := httptest.NewRecorder()

	handler.GetSubscriberHealth(rec, itRequest(501))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an offline subscriber, got %d", rec.Code)
	}
	var resp health.SubscriberHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ActiveSession != nil {
		t.Errorf("active_session must be null when offline, got %+v", resp.ActiveSession)
	}
	if resp.Status != "active" {
		t.Errorf("account status must still come from the DB, got %q", resp.Status)
	}
	if resp.FupStatus != "below" {
		t.Errorf("fup_status with no session: want below, got %q", resp.FupStatus)
	}
}

// TestFR_OBS_004_SubscriberHealth_CacheFailureDoesNotFailRequest verifies a Redis outage
// degrades the response rather than breaking the diagnostic endpoint, which is
// exactly when support needs it.
//
// INT-HEALTH-001 (supporting) | FR-OBS-004
func TestFR_OBS_004_SubscriberHealth_CacheFailureDoesNotFailRequest(t *testing.T) {
	handler := health.NewHandler(
		&itHealthDB{records: map[int]*health.SubscriberRecord{501: itSeedSubscriber()}},
		&itSessionCache{err: fmt.Errorf("redis: connection refused")},
	)
	rec := httptest.NewRecorder()

	handler.GetSubscriberHealth(rec, itRequest(501))

	if rec.Code != http.StatusOK {
		t.Fatalf("a cache outage must not fail the request, got %d", rec.Code)
	}
	var resp health.SubscriberHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Username != "diagnose@isp" {
		t.Errorf("DB-sourced fields must still be present, got %+v", resp)
	}
	if resp.ActiveSession != nil {
		t.Error("active_session must be null when the cache is unavailable")
	}
}

// TestFR_OBS_004_SubscriberHealth_UnknownSubscriber verifies a missing subscriber returns
// 404 rather than an empty 200.
//
// INT-HEALTH-001 (supporting) | FR-OBS-004
func TestFR_OBS_004_SubscriberHealth_UnknownSubscriber(t *testing.T) {
	handler := health.NewHandler(
		&itHealthDB{records: map[int]*health.SubscriberRecord{}},
		&itSessionCache{sessions: map[int]*health.SessionSummary{}},
	)
	rec := httptest.NewRecorder()

	handler.GetSubscriberHealth(rec, itRequest(999))

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404 for an unknown subscriber, got %d", rec.Code)
	}
}

// TestFR_OBS_004_SubscriberHealth_InvalidID verifies a non-numeric id is rejected.
//
// INT-HEALTH-001 (supporting) | FR-OBS-004
func TestFR_OBS_004_SubscriberHealth_InvalidID(t *testing.T) {
	handler := health.NewHandler(
		&itHealthDB{records: map[int]*health.SubscriberRecord{}},
		&itSessionCache{sessions: map[int]*health.SessionSummary{}},
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/abc/health", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	handler.GetSubscriberHealth(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric id, got %d", rec.Code)
	}
}
