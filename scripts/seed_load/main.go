// Command seed_load bulk-loads subscribers for the NFR load tests.
//
// Replaces the seed_radius_sessions.py and seed_billing_data.py the tracker
// references, which do not exist in this repository (and Python is not a
// dependency of this project).
//
// Usage:
//
//	go run ./scripts/seed_load -dsn "$DSN" -count 20000 -users-out /src/users.csv
//	go run ./scripts/seed_load -dsn "$DSN" -count 20000 -invoiced-pct 90 -sessions
//
// Every seeded subscriber shares one password, and therefore one bcrypt hash.
// That is deliberate: hashing 20,000 distinct passwords at cost 12 would take
// roughly an hour and measures bcrypt, not the system under test.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	var (
		dsn         = flag.String("dsn", "", "PostgreSQL DSN (required)")
		count       = flag.Int("count", 1000, "number of subscribers to create")
		password    = flag.String("secret", "TestPass1234!", "shared credential for every seeded subscriber")
		bcryptCost  = flag.Int("bcrypt-cost", bcrypt.MinCost, "bcrypt cost for the shared hash")
		invoicedPct = flag.Int("invoiced-pct", 0, "percentage that already have a current-period invoice")
		sessions    = flag.Bool("sessions", false, "also open a live session per subscriber")
		usersOut    = flag.String("users-out", "", "write a username,password CSV here for radload")
		batchSize   = flag.Int("batch", 5000, "rows per COPY batch")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "seed_load: -dsn is required")
		os.Exit(1)
	}
	if err := run(*dsn, *count, *password, *bcryptCost, *invoicedPct, *sessions, *usersOut, *batchSize); err != nil {
		fmt.Fprintf(os.Stderr, "seed_load: %v\n", err)
		os.Exit(1)
	}
}

func run(dsn string, count int, password string, cost, invoicedPct int, sessions bool, usersOut string, batchSize int) error {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	start := time.Now()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	fmt.Printf("seed_load: shared bcrypt hash generated (cost %d)\n", cost)

	if err := seedReferenceData(ctx, pool); err != nil {
		return err
	}

	// COPY is used rather than INSERT: at 20,000 rows the round-trip cost of
	// individual statements dominates, and the point is to have the data, not to
	// measure insertion.
	fmt.Printf("seed_load: loading %d subscribers...\n", count)
	if err := copySubscribers(ctx, pool, count, string(hash), batchSize); err != nil {
		return err
	}

	if invoicedPct > 0 {
		fmt.Printf("seed_load: invoicing %d%% of subscribers for the current period...\n", invoicedPct)
		if err := seedInvoices(ctx, pool, invoicedPct); err != nil {
			return err
		}
	}

	if sessions {
		fmt.Println("seed_load: opening a live session per subscriber...")
		if err := seedSessions(ctx, pool); err != nil {
			return err
		}
	}

	fmt.Println("seed_load: running ANALYZE so the planner sees the new data...")
	if _, err := pool.Exec(ctx, "ANALYZE"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}

	if usersOut != "" {
		if err := writeUsersFile(usersOut, count, password); err != nil {
			return err
		}
		fmt.Printf("seed_load: wrote %s\n", usersOut)
	}

	fmt.Printf("seed_load: done in %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// seedReferenceData inserts the franchise, plan and GST rate the subscribers
// reference. Idempotent so the seeder can be re-run against a live database.
func seedReferenceData(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`INSERT INTO franchises (id, name, owner_name, mobile_number, commission_rate_pct, status)
		 VALUES (1, 'Load Test LCO', 'Owner', '+919000000000', 10.00, 'active')
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes,
		                    fup_throttle_string, price, validity_days, franchise_id)
		 VALUES (1, 'TN_Super_100M', '100M/100M', 3300, 3543348019200, '10M/10M', 799.00, 30, 1)
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO gst_rates (id, cgst_rate, sgst_rate, igst_rate, effective_from)
		 VALUES (1, 9.00, 9.00, 18.00, NOW() - INTERVAL '1 day')
		 ON CONFLICT (id) DO NOTHING`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("seed reference data: %w", err)
		}
	}
	return nil
}

func loadUsername(i int) string { return fmt.Sprintf("load%06d@isp", i) }

func copySubscribers(ctx context.Context, pool *pgxpool.Pool, count int, hash string, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 5000
	}
	columns := []string{
		"id", "caf_number", "username", "password_hash", "mobile_number", "plan_id",
		"franchise_id", "status", "dunning_state", "wallet_balance", "registered_state",
		"plan_expiry", "fup_active", "dnd_opt_out",
	}

	for offset := 0; offset < count; offset += batchSize {
		end := offset + batchSize
		if end > count {
			end = count
		}

		rows := make([][]any, 0, end-offset)
		for i := offset; i < end; i++ {
			id := i + 1
			// Expiries fan out across the next 30 days so the collections
			// forecast has more than one bucket to group by.
			expiry := time.Now().AddDate(0, 0, i%30+1)
			rows = append(rows, []any{
				id,
				fmt.Sprintf("CAF-LOAD-%06d", id),
				loadUsername(id),
				hash,
				fmt.Sprintf("+9190%08d", id),
				1, 1, "active", "active",
				"799.00", "TN",
				expiry, false, false,
			})
		}

		copied, err := pool.CopyFrom(ctx,
			pgx.Identifier{"subscribers"}, columns, pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("copy subscribers at offset %d: %w", offset, err)
		}
		if int(copied) != len(rows) {
			return fmt.Errorf("copy subscribers: wrote %d of %d rows", copied, len(rows))
		}
		fmt.Printf("  %d/%d\n", end, count)
	}

	// COPY with explicit ids leaves the sequence behind, so a later INSERT
	// without an id would collide on the primary key.
	if _, err := pool.Exec(ctx,
		`SELECT setval(pg_get_serial_sequence('subscribers','id'), COALESCE(MAX(id),1)) FROM subscribers`); err != nil {
		return fmt.Errorf("reset subscribers sequence: %w", err)
	}
	return nil
}

// seedInvoices gives a percentage of subscribers a current-period invoice, so
// the unbilled report has a known deficit to find.
func seedInvoices(ctx context.Context, pool *pgxpool.Pool, pct int) error {
	if pct > 100 {
		pct = 100
	}
	const q = `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used, created_at)
		SELECT s.id, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 950.25, NOW()
		FROM subscribers s
		WHERE s.id % 100 < $1`

	tag, err := pool.Exec(ctx, q, pct)
	if err != nil {
		return fmt.Errorf("seed invoices: %w", err)
	}
	fmt.Printf("  %d invoices created\n", tag.RowsAffected())
	return nil
}

// seedSessions opens one live session per subscriber, with usage fanned out so
// some sit in the FUP warning band and some past their quota.
func seedSessions(ctx context.Context, pool *pgxpool.Pool) error {
	const q = `
		INSERT INTO subscriber_session_history (
			subscriber_id, session_id, nas_ip_address, assigned_ipv4,
			start_time, input_octets, output_octets
		)
		SELECT s.id,
		       'load-sess-' || s.id,
		       '10.10.0.1'::inet,
		       ('100.64.' || ((s.id / 254) % 254) || '.' || ((s.id % 254) + 1))::inet,
		       NOW() - INTERVAL '1 hour',
		       (3543348019200::bigint * ((s.id % 110)) / 100),
		       0
		FROM subscribers s`

	tag, err := pool.Exec(ctx, q)
	if err != nil {
		return fmt.Errorf("seed sessions: %w", err)
	}
	fmt.Printf("  %d sessions opened\n", tag.RowsAffected())
	return nil
}

func writeUsersFile(path string, count int, password string) error {
	f, err := os.Create(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return fmt.Errorf("create users file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	w := bufio.NewWriter(f)
	for i := 1; i <= count; i++ {
		if _, err := fmt.Fprintf(w, "%s,%s\n", loadUsername(i), password); err != nil {
			return fmt.Errorf("write users file: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush users file: %w", err)
	}
	return nil
}
