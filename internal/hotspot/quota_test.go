// Voucher data-cap tests — FR-HSP-001 | migration 035.
//
// The behaviour that matters is the order: revoke first, disconnect second. A
// disconnect that lands while the grant is still live buys nothing, because
// the device simply re-authenticates and carries on spending a voucher it has
// already exhausted.
package hotspot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ── Doubles ─────────────────────────────────────────────────────────────────

type fakeQuotaStore struct {
	mu sync.Mutex

	over    []OverCapGrant
	revoked []int64
	// claimed models the conditional UPDATE: only the first caller for a given
	// grant wins, as two scanner replicas would race.
	claimed  map[int64]bool
	listErr  error
	markErr  error
	markMiss bool
}

func newFakeQuotaStore(over ...OverCapGrant) *fakeQuotaStore {
	return &fakeQuotaStore{over: over, claimed: map[int64]bool{}}
}

func (f *fakeQuotaStore) ListGrantsOverCap(_ context.Context, _ int) ([]OverCapGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]OverCapGrant(nil), f.over...), nil
}

func (f *fakeQuotaStore) MarkGrantExhausted(_ context.Context, grantID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return false, f.markErr
	}
	if f.markMiss || f.claimed[grantID] {
		return false, nil
	}
	f.claimed[grantID] = true
	f.revoked = append(f.revoked, grantID)
	return true, nil
}

func (f *fakeQuotaStore) revokedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.revoked...)
}

// recordingPoD captures disconnects and the order they happened in relative to
// revocation.
type recordingPoD struct {
	mu     sync.Mutex
	calls  []string
	err    error
	before func() []int64 // revoked ids observed at the moment of the call
	seen   [][]int64
}

func (p *recordingPoD) Disconnect(_ context.Context, nasIP, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.before != nil {
		p.seen = append(p.seen, p.before())
	}
	if p.err != nil {
		return p.err
	}
	p.calls = append(p.calls, nasIP+"|"+sessionID)
	return nil
}

func (p *recordingPoD) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func overCap(id int64) OverCapGrant {
	return OverCapGrant{
		GrantID: id, MAC: "AA:BB:CC:DD:EE:FF", VoucherID: int(id),
		SessionID: "sess-1", NASIP: "10.10.0.1",
		BytesUsed: 2_000_000_000, CapBytes: 1_073_741_824,
	}
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestFR_HSP_001_ExhaustedVoucherIsRevokedAndDisconnected(t *testing.T) {
	store := newFakeQuotaStore(overCap(1))
	pod := &recordingPoD{}

	ended, err := NewQuotaScanner(store, pod, time.Minute).ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if ended != 1 {
		t.Errorf("want 1 grant ended, got %d", ended)
	}
	if got := store.revokedIDs(); len(got) != 1 || got[0] != 1 {
		t.Errorf("the grant must be revoked, got %v", got)
	}
	if got := pod.snapshot(); len(got) != 1 || got[0] != "10.10.0.1|sess-1" {
		t.Errorf("the live session must be disconnected at its own NAS, got %v", got)
	}
}

// TestFR_HSP_001_RevocationHappensBeforeTheDisconnect is the ordering
// property. Disconnecting a grant that is still live only prompts the device
// to re-authenticate and keep spending an exhausted voucher.
func TestFR_HSP_001_RevocationHappensBeforeTheDisconnect(t *testing.T) {
	store := newFakeQuotaStore(overCap(7))
	pod := &recordingPoD{}
	pod.before = store.revokedIDs

	if _, err := NewQuotaScanner(store, pod, time.Minute).ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	if len(pod.seen) != 1 {
		t.Fatalf("want 1 disconnect, got %d", len(pod.seen))
	}
	if len(pod.seen[0]) != 1 || pod.seen[0][0] != 7 {
		t.Errorf("the grant must already be revoked when the disconnect is issued, "+
			"observed revocations at that moment: %v — otherwise the device just "+
			"reconnects on an exhausted voucher", pod.seen[0])
	}
}

// TestFR_HSP_001_FailedDisconnectStillLeavesTheGrantRevoked — the opposite
// direction from the document purge, and deliberately so. Access is the thing
// that must stop; the live session merely outlives it until the NAS
// re-authenticates.
func TestFR_HSP_001_FailedDisconnectStillLeavesTheGrantRevoked(t *testing.T) {
	store := newFakeQuotaStore(overCap(3))
	pod := &recordingPoD{err: errors.New("nas unreachable")}

	ended, err := NewQuotaScanner(store, pod, time.Minute).ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("a failed disconnect must not fail the scan: %v", err)
	}
	if ended != 1 {
		t.Errorf("the grant still counts as ended, got %d", ended)
	}
	if got := store.revokedIDs(); len(got) != 1 {
		t.Errorf("revocation must survive a failed disconnect, got %v", got)
	}
}

// TestFR_HSP_001_ConcurrentScansEndAGrantOnce — two replicas run this scanner,
// and the conditional revoke is what stops both counting the same exhaustion.
func TestFR_HSP_001_ConcurrentScansEndAGrantOnce(t *testing.T) {
	store := newFakeQuotaStore(overCap(5))
	pod := &recordingPoD{}
	scanner := NewQuotaScanner(store, pod, time.Minute)

	var wg sync.WaitGroup
	results := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _ := scanner.ScanOnce(context.Background()) //nolint:errcheck
			results <- n
		}()
	}
	wg.Wait()
	close(results)

	total := 0
	for n := range results {
		total += n
	}
	if total != 1 {
		t.Errorf("concurrent scans must end one grant exactly once, got %d", total)
	}
	if got := len(pod.snapshot()); got != 1 {
		t.Errorf("want exactly 1 disconnect, got %d — a duplicate disconnect is a second "+
			"Disconnect-Request for a session that is already gone", got)
	}
}

// TestFR_HSP_001_GrantWithNoLiveSessionIsStillRevoked — the grant may have
// been metered before the NAS reported a session id. Access must still stop.
func TestFR_HSP_001_GrantWithNoLiveSessionIsStillRevoked(t *testing.T) {
	g := overCap(9)
	g.SessionID, g.NASIP = "", ""
	store := newFakeQuotaStore(g)
	pod := &recordingPoD{}

	ended, err := NewQuotaScanner(store, pod, time.Minute).ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if ended != 1 || len(store.revokedIDs()) != 1 {
		t.Error("a grant with no known session must still be revoked")
	}
	if got := pod.snapshot(); len(got) != 0 {
		t.Errorf("nothing to disconnect, so no packet should be sent, got %v", got)
	}
}

func TestFR_HSP_001_NoDisconnectorStillRevokes(t *testing.T) {
	store := newFakeQuotaStore(overCap(2))

	ended, err := NewQuotaScanner(store, nil, time.Minute).ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if ended != 1 || len(store.revokedIDs()) != 1 {
		t.Error("with no disconnector configured the grant must still be revoked")
	}
}

func TestFR_HSP_001_ScanSurvivesAListingFailure(t *testing.T) {
	store := newFakeQuotaStore()
	store.listErr = errors.New("database unavailable")

	if _, err := NewQuotaScanner(store, &recordingPoD{}, time.Minute).ScanOnce(context.Background()); err == nil {
		t.Error("a listing failure must be reported so the caller can log it")
	}
}

// TestFR_HSP_001_RevokeFailureSkipsTheDisconnect — if access was not actually
// withdrawn, disconnecting would just prompt a reconnect on a voucher the
// system still considers live.
func TestFR_HSP_001_RevokeFailureSkipsTheDisconnect(t *testing.T) {
	store := newFakeQuotaStore(overCap(4))
	store.markErr = errors.New("write failed")
	pod := &recordingPoD{}

	ended, err := NewQuotaScanner(store, pod, time.Minute).ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("one bad row must not fail the sweep: %v", err)
	}
	if ended != 0 {
		t.Errorf("nothing was ended, got %d", ended)
	}
	if got := pod.snapshot(); len(got) != 0 {
		t.Errorf("no disconnect may be sent for a grant that is still live, got %v", got)
	}
}

func TestFR_HSP_001_CancelledScanStops(t *testing.T) {
	store := newFakeQuotaStore(overCap(1), overCap(2), overCap(3))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewQuotaScanner(store, &recordingPoD{}, time.Minute).ScanOnce(ctx); err == nil {
		t.Error("a cancelled scan must stop rather than working through the batch")
	}
}

// Compile-time proof the double matches the interface the real store
// implements.
var _ QuotaStore = (*fakeQuotaStore)(nil)
var _ Disconnector = (*recordingPoD)(nil)
