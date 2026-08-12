// Command pg_failover_drill is a one-off live verification tool, not part
// of the application. It uses this codebase's own internal/db.Connect and
// a real store's write method (TicketStore.CreateTicketAdmin) exactly as
// the API service does, against a real Patroni cluster, while the operator
// kills the primary out from under it — proving internal/db/hapool.go's
// SQLSTATE 25006 detection actually recovers a live write path through the
// wrapped pool, not the raw one. database.Pool() deliberately returns the
// *unwrapped* pool (see its doc comment in internal/db/db.go) — using it
// here would test nothing, so this drills through a real store instead.
//
// Not wired into any cmd/ binary; run directly with `go run` for this
// verification only.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/db"
)

func main() {
	dsn := os.Getenv("DRILL_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DRILL_DSN is required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	database, err := db.Connect(ctx, db.DefaultConfig(dsn))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	// One-time seed via the raw pool — setup, not part of what's under
	// test. plan/subscriber need to exist once for CreateTicketAdmin's FK.
	rawPool := database.Pool()
	var planID int
	if err := rawPool.QueryRow(ctx, `INSERT INTO plans (name, rate_limit_string, volume_gb, price, validity_days)
		VALUES ('drill-plan', '50M/50M', 100, 100.00, 30) RETURNING id`).Scan(&planID); err != nil {
		fmt.Fprintf(os.Stderr, "seed plan: %v\n", err)
		os.Exit(1)
	}
	var subscriberID int
	if err := rawPool.QueryRow(ctx, `INSERT INTO subscribers
		(caf_number, username, password_hash, mobile_number, plan_id, status, registered_state)
		VALUES ('CAF-DRILL', 'drill_subscriber', 'x', '+919999999999', $1, 'active', 'TN') RETURNING id`,
		planID).Scan(&subscriberID); err != nil {
		fmt.Fprintf(os.Stderr, "seed subscriber: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("drill: seeded plan %d, subscriber %d\n", planID, subscriberID)

	// Everything from here goes through database.Tickets() — a real store
	// holding the SQLSTATE-25006-aware wrapped pool (internal/db/hapool.go),
	// the exact same path internal/api's ticket handlers use in production.
	tickets := database.Tickets()

	fmt.Println("drill: starting write loop through database.Tickets() (real store path) — Ctrl+C to stop")

	var (
		attempt, success, failure int
		firstFailureAt            time.Time
		recoveredAt               time.Time
		inFailure                 bool
	)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			printSummary(attempt, success, failure, firstFailureAt, recoveredAt)
			return
		case <-ticker.C:
			attempt++
			writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := tickets.CreateTicketAdmin(writeCtx, subscriberID, "connectivity", fmt.Sprintf("drill write #%d", attempt))
			writeCancel()

			ts := time.Now().Format("15:04:05.000")
			if err != nil {
				failure++
				if !inFailure {
					inFailure = true
					firstFailureAt = time.Now()
					fmt.Printf("[%s] attempt %d: FAIL — %v\n", ts, attempt, err)
				}
				continue
			}
			success++
			if inFailure {
				inFailure = false
				recoveredAt = time.Now()
				fmt.Printf("[%s] attempt %d: OK — recovered after %v\n", ts, attempt, recoveredAt.Sub(firstFailureAt))
			}
		}
	}
}

func printSummary(attempt, success, failure int, firstFailureAt, recoveredAt time.Time) {
	fmt.Println("\n--- drill summary ---")
	fmt.Printf("attempts: %d  success: %d  failure: %d\n", attempt, success, failure)
	if !firstFailureAt.IsZero() {
		fmt.Printf("first failure at: %s\n", firstFailureAt.Format("15:04:05.000"))
	}
	if !recoveredAt.IsZero() {
		fmt.Printf("recovered at:     %s\n", recoveredAt.Format("15:04:05.000"))
		if !firstFailureAt.IsZero() {
			fmt.Printf("outage duration:  %v\n", recoveredAt.Sub(firstFailureAt))
		}
	}
}
