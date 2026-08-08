//go:build integration

// Proves migration 019's least-privilege bss_app role is what actually
// enforces lea_audit_log's append-only guarantee. Every other test in this
// package connects as the postgres superuser (via TEST_DB_DSN), which
// PostgreSQL's row-level security always exempts regardless of policy —
// so this is the one test that must connect as bss_app specifically, or it
// proves nothing.
package db_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLEAAuditLog_AppRoleBlockedFromUpdateDelete | DoD Phase 1 Step 2 |
// SecD §9.7, DBD §6.5
func TestLEAAuditLog_AppRoleBlockedFromUpdateDelete(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	const appPassword = "integration-test-app-role-password"
	// Not parameterized: ALTER ROLE ... PASSWORD expects a string literal in
	// PostgreSQL's grammar, not a bind parameter — appPassword is a fixed
	// constant this test controls, not external input, so direct
	// interpolation here is safe.
	if _, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE bss_app WITH PASSWORD '%s'", appPassword)); err != nil {
		t.Fatalf("set bss_app password: %v", err)
	}

	appDSN, err := dsnAsRole(os.Getenv("TEST_DB_DSN"), "bss_app", appPassword)
	if err != nil {
		t.Fatalf("build bss_app DSN: %v", err)
	}
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as bss_app: %v", err)
	}
	defer appPool.Close()

	// Seed one row as the superuser — bss_app can only INSERT, which is
	// exercised in its own subtest below rather than relied on here.
	if _, err := pool.Exec(ctx, `
		INSERT INTO lea_audit_log (accessor_identity, accessor_role, queried_public_ip, queried_timestamp, result_row_count)
		VALUES ('rls-test-officer', 'noc_engineer', '203.0.113.9', NOW(), 1)`); err != nil {
		t.Fatalf("seed lea_audit_log row: %v", err)
	}

	t.Run("UPDATE is denied", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `UPDATE lea_audit_log SET accessor_identity = 'tampered' WHERE accessor_identity = 'rls-test-officer'`)
		if err == nil {
			t.Fatal("expected UPDATE to be denied for bss_app, it succeeded")
		}
	})

	t.Run("DELETE is denied", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `DELETE FROM lea_audit_log WHERE accessor_identity = 'rls-test-officer'`)
		if err == nil {
			t.Fatal("expected DELETE to be denied for bss_app, it succeeded")
		}
	})

	t.Run("the seeded row survived untouched", func(t *testing.T) {
		var identity string
		err := pool.QueryRow(ctx, `
			SELECT accessor_identity FROM lea_audit_log
			WHERE accessor_identity IN ('rls-test-officer', 'tampered')
			ORDER BY id DESC LIMIT 1`).Scan(&identity)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if identity != "rls-test-officer" {
			t.Errorf("row was modified despite a denied UPDATE/DELETE: accessor_identity = %q", identity)
		}
	})

	t.Run("SELECT is denied (the app never reads this table back)", func(t *testing.T) {
		var count int
		err := appPool.QueryRow(ctx, `SELECT COUNT(*) FROM lea_audit_log`).Scan(&count)
		if err == nil {
			t.Fatal("expected SELECT to be denied for bss_app, it succeeded")
		}
	})

	t.Run("INSERT still works — the one thing bss_app needs to do", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO lea_audit_log (accessor_identity, accessor_role, queried_public_ip, queried_timestamp, result_row_count)
			VALUES ('rls-test-officer-2', 'noc_engineer', '203.0.113.10', NOW(), 0)`)
		if err != nil {
			t.Fatalf("bss_app should be able to INSERT: %v", err)
		}
	})
}

// dsnAsRole rewrites a postgres:// DSN's user/password, keeping host, port,
// dbname, and query params (e.g. sslmode) unchanged.
func dsnAsRole(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
