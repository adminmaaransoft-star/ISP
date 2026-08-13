//go:build integration

package db_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/workflow"
)

// Approval-workflow persistence tests — FR-WFL-001..002 | MDS §4.15.
//
// internal/api/workflow_integration_test.go covers the HTTP layer against a
// stub, which never touches this SQL. These exercise the real queries — in
// particular ClaimApprovalRequest's atomic conditional UPDATE, which is what
// actually stops two approvers from both executing the same money movement,
// and the two CHECK constraints that back the application-level rules.

// TestFR_WFL_001_CreateAndGetApprovalRequest covers the round trip, and that
// amount survives as an exact decimal rather than a float.
func TestFR_WFL_001_CreateAndGetApprovalRequest(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "approval@isp"})

	store := database.Workflow()
	amount := mustDecimal(t, "1500.50")

	created, err := store.CreateApprovalRequest(ctx, workflow.ApprovalRequest{
		ActionType: workflow.ActionWalletCredit, SubscriberID: 1,
		Amount: &amount, Reason: "goodwill credit", RequestedBy: "alice",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}
	if created.Status != workflow.StatusPending {
		t.Errorf("a new request must start pending, got %q", created.Status)
	}
	if created.Amount == nil || !created.Amount.Equal(amount) {
		t.Errorf("amount round trip: want %s, got %v", amount, created.Amount)
	}
	if created.DecidedBy != nil {
		t.Errorf("a new request must have no decider, got %v", *created.DecidedBy)
	}

	got, err := store.GetApprovalRequest(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApprovalRequest: %v", err)
	}
	if got == nil || got.RequestedBy != "alice" {
		t.Errorf("read back: %+v", got)
	}

	t.Run("unknown id returns (nil, nil)", func(t *testing.T) {
		req, err := store.GetApprovalRequest(ctx, 999999)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req != nil {
			t.Errorf("want nil for an unknown request, got %+v", req)
		}
	})
}

// TestFR_WFL_001_ClaimIsAtomicUnderConcurrency is the test the whole atomic
// claim exists for. Ten goroutines race to approve one request; exactly one
// may win, because in production each winner goes on to move money.
func TestFR_WFL_001_ClaimIsAtomicUnderConcurrency(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "race@isp"})

	store := database.Workflow()
	amount := mustDecimal(t, "500.00")
	created, err := store.CreateApprovalRequest(ctx, workflow.ApprovalRequest{
		ActionType: workflow.ActionWalletCredit, SubscriberID: 1,
		Amount: &amount, Reason: "goodwill", RequestedBy: "alice",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}

	const racers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, err := store.ClaimApprovalRequest(ctx, created.ID, "bob")
			if err != nil {
				t.Errorf("ClaimApprovalRequest: %v", err)
				return
			}
			if claimed != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("DOUBLE CLAIM: want exactly 1 winner out of %d concurrent claims, got %d", racers, winners)
	}

	final, err := store.GetApprovalRequest(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApprovalRequest: %v", err)
	}
	if final.Status != workflow.StatusApproved {
		t.Errorf("final status = %q, want approved", final.Status)
	}
}

// TestFR_WFL_001_ClaimOnAnAlreadyDecidedRequestReturnsNil verifies the
// "already decided" signal the API turns into a 409 — and specifically that
// it is (nil, nil), not an error, so the handler can tell it apart from a
// real failure.
func TestFR_WFL_001_ClaimOnAnAlreadyDecidedRequestReturnsNil(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "decided@isp"})

	store := database.Workflow()
	created, err := store.CreateApprovalRequest(ctx, workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 1,
		Reason: "relocated", RequestedBy: "alice",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}

	if _, err := store.RejectApprovalRequest(ctx, created.ID, "bob", "customer disputed"); err != nil {
		t.Fatalf("RejectApprovalRequest: %v", err)
	}

	claimed, err := store.ClaimApprovalRequest(ctx, created.ID, "carol")
	if err != nil {
		t.Fatalf("claiming a rejected request must not error: %v", err)
	}
	if claimed != nil {
		t.Error("a rejected request must not be claimable — it would execute after having been refused")
	}
}

// TestFR_WFL_001_SchemaRejectsSelfApproval is the backstop test: even if
// every application-level check were bypassed, the database itself must
// refuse to record a request decided by the person who filed it.
func TestFR_WFL_001_SchemaRejectsSelfApproval(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "selfapprove@isp"})

	if _, err := pool.Exec(ctx, `
		INSERT INTO approval_requests (action_type, subscriber_id, reason, requested_by_username)
		VALUES ('terminate', 1, 'relocated', 'alice')`); err != nil {
		t.Fatalf("seed approval request: %v", err)
	}

	// Bypassing the store entirely, exactly as a future stray code path or
	// a manual query might.
	_, err := pool.Exec(ctx, `
		UPDATE approval_requests SET status='approved', decided_by_username='alice' WHERE id=1`)
	if err == nil {
		t.Fatal("chk_approval_distinct_approver must reject a request decided by its own requester")
	}
}

// TestFR_WFL_001_SchemaEnforcesAmountByActionType covers the other
// constraint: a terminate carrying an amount, or a money action without one,
// are both nonsense rows that must not be storable.
func TestFR_WFL_001_SchemaEnforcesAmountByActionType(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "amounts@isp"})

	t.Run("terminate with an amount is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO approval_requests (action_type, subscriber_id, amount, reason, requested_by_username)
			VALUES ('terminate', 1, 100.00, 'x', 'alice')`)
		if err == nil {
			t.Error("a terminate request must not carry an amount")
		}
	})

	t.Run("wallet_credit without an amount is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO approval_requests (action_type, subscriber_id, reason, requested_by_username)
			VALUES ('wallet_credit', 1, 'x', 'alice')`)
		if err == nil {
			t.Error("a wallet_credit request must carry an amount")
		}
	})

	t.Run("a zero or negative amount is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO approval_requests (action_type, subscriber_id, amount, reason, requested_by_username)
			VALUES ('refund', 1, 0, 'x', 'alice')`)
		if err == nil {
			t.Error("a refund of zero must be rejected")
		}
	})
}

// TestFR_WFL_001_FinalizeExecutionRecordsOutcome covers the last step of the
// approved path, including the execution_failed branch an operator has to be
// able to find later.
func TestFR_WFL_001_FinalizeExecutionRecordsOutcome(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "finalize@isp"})

	store := database.Workflow()
	created, err := store.CreateApprovalRequest(ctx, workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 1,
		Reason: "relocated", RequestedBy: "alice",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}
	if _, err := store.ClaimApprovalRequest(ctx, created.ID, "bob"); err != nil {
		t.Fatalf("ClaimApprovalRequest: %v", err)
	}

	failure := "subscriber not found"
	if err := store.FinalizeApprovalExecution(ctx, created.ID, workflow.StatusExecutionFailed, &failure, nil); err != nil {
		t.Fatalf("FinalizeApprovalExecution: %v", err)
	}

	final, err := store.GetApprovalRequest(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApprovalRequest: %v", err)
	}
	if final.Status != workflow.StatusExecutionFailed {
		t.Errorf("status = %q, want execution_failed", final.Status)
	}
	if final.ExecutionError == nil || *final.ExecutionError != failure {
		t.Errorf("execution_error = %v, want %q", final.ExecutionError, failure)
	}

	t.Run("finalizing an unknown request reports not found", func(t *testing.T) {
		if err := store.FinalizeApprovalExecution(ctx, 999999, workflow.StatusExecuted, nil, nil); err == nil {
			t.Error("want an error for an unknown request")
		}
	})
}

// TestFR_WFL_001_ListFiltersIndependently verifies the two optional filters
// compose rather than one silently overriding the other.
func TestFR_WFL_001_ListFiltersIndependently(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "list1@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "list2@isp"})

	store := database.Workflow()
	for _, sub := range []int{1, 1, 2} {
		if _, err := store.CreateApprovalRequest(ctx, workflow.ApprovalRequest{
			ActionType: workflow.ActionTerminate, SubscriberID: sub,
			Reason: "x", RequestedBy: "alice",
		}); err != nil {
			t.Fatalf("CreateApprovalRequest: %v", err)
		}
	}
	// Decide one of subscriber 1's, so a status filter has something to exclude.
	if _, err := store.RejectApprovalRequest(ctx, 1, "bob", "no"); err != nil {
		t.Fatalf("RejectApprovalRequest: %v", err)
	}

	all, err := store.ListApprovalRequests(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListApprovalRequests(nil, nil): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered list = %d, want 3", len(all))
	}

	pending := workflow.StatusPending
	pendingOnly, err := store.ListApprovalRequests(ctx, &pending, nil)
	if err != nil {
		t.Fatalf("ListApprovalRequests(pending, nil): %v", err)
	}
	if len(pendingOnly) != 2 {
		t.Errorf("pending list = %d, want 2", len(pendingOnly))
	}

	sub1 := 1
	sub1Pending, err := store.ListApprovalRequests(ctx, &pending, &sub1)
	if err != nil {
		t.Fatalf("ListApprovalRequests(pending, &1): %v", err)
	}
	if len(sub1Pending) != 1 {
		t.Errorf("pending+subscriber filter = %d, want 1 — the two filters must compose", len(sub1Pending))
	}
}

// ── Field tasks (FR-WFL-002) ────────────────────────────────────────────────

func TestFR_WFL_002_FieldTaskLifecycle(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "task@isp"})

	store := database.Workflow()
	subID := 1
	due := time.Now().AddDate(0, 0, 7)

	created, err := store.CreateFieldTask(ctx, workflow.FieldTask{
		Title: "Replace CPE", Description: "Router faulty at flat 3B",
		SubscriberID: &subID, AssignedTo: "tech1", CreatedBy: "csr1", DueDate: &due,
	})
	if err != nil {
		t.Fatalf("CreateFieldTask: %v", err)
	}
	if created.Status != workflow.TaskOpen {
		t.Errorf("a new task must start open, got %q", created.Status)
	}
	if created.CompletedAt != nil {
		t.Error("a new task must not carry a completion time")
	}

	t.Run("completing sets completed_at", func(t *testing.T) {
		status := workflow.TaskCompleted
		updated, err := store.UpdateFieldTask(ctx, created.ID, &status, nil, nil)
		if err != nil {
			t.Fatalf("UpdateFieldTask: %v", err)
		}
		if updated.Status != workflow.TaskCompleted {
			t.Errorf("status = %q, want completed", updated.Status)
		}
		if updated.CompletedAt == nil {
			t.Error("completing a task must stamp completed_at")
		}
	})

	// Reopening must clear the stamp, or a task that was completed once
	// would keep claiming a completion date it no longer has.
	t.Run("reopening clears completed_at", func(t *testing.T) {
		status := workflow.TaskInProgress
		updated, err := store.UpdateFieldTask(ctx, created.ID, &status, nil, nil)
		if err != nil {
			t.Fatalf("UpdateFieldTask: %v", err)
		}
		if updated.CompletedAt != nil {
			t.Errorf("reopening must clear completed_at, got %v", updated.CompletedAt)
		}
	})

	t.Run("a partial update leaves other fields alone", func(t *testing.T) {
		assignee := "tech2"
		updated, err := store.UpdateFieldTask(ctx, created.ID, nil, &assignee, nil)
		if err != nil {
			t.Fatalf("UpdateFieldTask: %v", err)
		}
		if updated.AssignedTo != "tech2" {
			t.Errorf("assigned_to = %q, want tech2", updated.AssignedTo)
		}
		if updated.Status != workflow.TaskInProgress {
			t.Errorf("status must be untouched by an assignee-only update, got %q", updated.Status)
		}
		if updated.Title != "Replace CPE" {
			t.Errorf("title must be untouched, got %q", updated.Title)
		}
	})

	t.Run("unknown id returns (nil, nil)", func(t *testing.T) {
		task, err := store.UpdateFieldTask(ctx, 999999, nil, nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if task != nil {
			t.Errorf("want nil for an unknown task, got %+v", task)
		}
	})
}

// TestFR_WFL_002_FieldTaskIsIndependentOfSubscribers verifies the
// subscriber_id nullability the design depends on — CRD-EXP-002's "ad hoc"
// tasks are not all about one subscriber.
func TestFR_WFL_002_FieldTaskIsIndependentOfSubscribers(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	created, err := database.Workflow().CreateFieldTask(ctx, workflow.FieldTask{
		Title: "Audit the Chennai North POP", AssignedTo: "tech1", CreatedBy: "owner",
	})
	if err != nil {
		t.Fatalf("a task with no subscriber must be storable: %v", err)
	}
	if created.SubscriberID != nil {
		t.Errorf("subscriber_id = %v, want nil", created.SubscriberID)
	}
}

func TestFR_WFL_002_ListFieldTasksFilters(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	store := database.Workflow()
	for _, tc := range []struct{ title, assignee string }{
		{"A", "tech1"}, {"B", "tech1"}, {"C", "tech2"},
	} {
		if _, err := store.CreateFieldTask(ctx, workflow.FieldTask{
			Title: tc.title, AssignedTo: tc.assignee, CreatedBy: "csr1",
		}); err != nil {
			t.Fatalf("CreateFieldTask: %v", err)
		}
	}

	tech1 := "tech1"
	list, err := store.ListFieldTasks(ctx, &tech1, nil)
	if err != nil {
		t.Fatalf("ListFieldTasks: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("tech1's queue = %d tasks, want 2", len(list))
	}

	// A queue view is only useful if completed work drops out of it.
	status := workflow.TaskCompleted
	if _, err := store.UpdateFieldTask(ctx, 1, &status, nil, nil); err != nil {
		t.Fatalf("UpdateFieldTask: %v", err)
	}
	open := workflow.TaskOpen
	openOnly, err := store.ListFieldTasks(ctx, &tech1, &open)
	if err != nil {
		t.Fatalf("ListFieldTasks(open): %v", err)
	}
	if len(openOnly) != 1 {
		t.Errorf("tech1's open queue = %d, want 1 after completing one", len(openOnly))
	}
}
