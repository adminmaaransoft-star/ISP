#!/usr/bin/env bash
# INT-BIL-006 — GST constraint blocks dual tax
#
# FR-BIL-001 | DB-006 | DBD §6.2 invoices.chk_gst_logic
#
# An invoice may carry intrastate tax (CGST+SGST) or interstate tax (IGST), never
# both. The rule is enforced in the schema so a bug in any writer — not just
# billing.CalculateGstInvoice — is rejected by the database.
#
# This assertion is schema-level, so unlike the other INT-* cases it cannot run
# against an in-process fake; it needs a real PostgreSQL.
#
# Usage:
#   ./scripts/int_bil_006_gst_constraint.sh              # starts a throwaway container
#   TEST_DB_DSN=postgres://user:pw@host:5432/db ./scripts/int_bil_006_gst_constraint.sh
#
# Exit 0 = constraint enforced. Exit 1 = constraint missing or wrong.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PG_IMAGE="${PG_IMAGE:-postgres:15-alpine}"
CONTAINER="int_bil_006_pg_$$"
OWNED_CONTAINER=0

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }

cleanup() {
    if [ "$OWNED_CONTAINER" = "1" ]; then
        docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# psql runs inside the container when we own it, otherwise against TEST_DB_DSN.
psql_run() {
    if [ "$OWNED_CONTAINER" = "1" ]; then
        docker exec -i -e PGPASSWORD=postgres "$CONTAINER" \
            psql -v ON_ERROR_STOP=1 -U postgres -d isp_bss_oss "$@"
    else
        psql -v ON_ERROR_STOP=1 "$TEST_DB_DSN" "$@"
    fi
}

# ── Bring up a database ──────────────────────────────────────────────────────

if [ -n "${TEST_DB_DSN:-}" ]; then
    info "using TEST_DB_DSN"
    command -v psql >/dev/null 2>&1 || { fail "psql not on PATH (needed with TEST_DB_DSN)"; exit 1; }
else
    command -v docker >/dev/null 2>&1 || {
        fail "docker not available and TEST_DB_DSN unset — cannot reach a PostgreSQL"
        exit 1
    }
    info "starting throwaway PostgreSQL ($PG_IMAGE)"
    docker run -d --rm --name "$CONTAINER" \
        -e POSTGRES_PASSWORD=postgres \
        -e POSTGRES_DB=isp_bss_oss \
        "$PG_IMAGE" >/dev/null
    OWNED_CONTAINER=1

    for _ in $(seq 1 60); do
        if docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 || {
        fail "PostgreSQL did not become ready within 60s"
        exit 1
    }
fi

# ── Apply the Up half of every migration, in order ───────────────────────────

# Only 001..006 are needed to reach the invoices table, and stopping there keeps
# the check runnable on a stock postgres image: migrations 010 and 011 require
# the pg_partman extension, which is not bundled with postgres:15-alpine.
MIGRATE_THROUGH="${MIGRATE_THROUGH:-006}"

info "applying migrations 001..$MIGRATE_THROUGH"
for migration in "$REPO_ROOT"/migrations/*.sql; do
    number="$(basename "$migration" | cut -d_ -f1)"
    if [ "$number" \> "$MIGRATE_THROUGH" ]; then
        continue
    fi
    # Take everything between "-- +goose Up" and "-- +goose Down".
    if ! sed -n '/^-- +goose Up/,/^-- +goose Down/p' "$migration" \
        | sed '/^-- +goose Down/d' \
        | psql_run >/dev/null; then
        fail "migration failed: $(basename "$migration")"
        exit 1
    fi
done
pass "migrations applied"

# ── Seed the FK chain an invoice needs ───────────────────────────────────────

psql_run >/dev/null <<'SQL'
INSERT INTO franchises (id, name, owner_name, mobile_number, commission_rate_pct, status)
VALUES (1, 'Chennai LCO', 'A. Kumar', '+919876543210', 10.00, 'active');

INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes,
                   fup_throttle_string, price, validity_days, franchise_id)
VALUES (1, '100 Mbps Unlimited', '100M/100M', 3300, 3543348019200, '10M/10M', 799.00, 30, 1);

INSERT INTO subscribers (id, caf_number, username, password_hash, mobile_number,
                         plan_id, franchise_id, status, registered_state)
VALUES (1, 'CAF-2026-0001', 'gst@isp', '$2a$12$notarealhash', '+919876543210',
        1, 1, 'active', 'TN');

INSERT INTO gst_rates (id, cgst_rate, sgst_rate, igst_rate)
VALUES (1, 9.00, 9.00, 18.00);
SQL
pass "seed data inserted"

FAILURES=0

# ── The assertion: dual tax must be rejected ─────────────────────────────────

info "attempting invoice with both CGST and IGST (must be rejected)"
DUAL_TAX_OUT=$(psql_run <<'SQL' 2>&1 || true
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
                      total_amount, gst_rate_id, gb_included, gb_used)
VALUES (1, 799.00, 71.91, 71.91, 143.82, 1086.64, 1, 3300, 950.25);
SQL
)

if echo "$DUAL_TAX_OUT" | grep -q "chk_gst_logic"; then
    pass "dual-tax insert rejected by chk_gst_logic"
else
    fail "dual-tax insert was NOT rejected by chk_gst_logic"
    echo "  psql said: $DUAL_TAX_OUT"
    FAILURES=$((FAILURES + 1))
fi

# PostgreSQL reports a CHECK violation as SQLSTATE 23514.
if echo "$DUAL_TAX_OUT" | grep -q "23514"; then
    pass "SQLSTATE 23514 (check_violation) returned"
else
    # psql only prints the code with VERBOSITY=verbose; re-run to confirm.
    SQLSTATE_OUT=$(psql_run -v VERBOSITY=verbose <<'SQL' 2>&1 || true
\set VERBOSITY verbose
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
                      total_amount, gst_rate_id, gb_included, gb_used)
VALUES (1, 799.00, 71.91, 71.91, 143.82, 1086.64, 1, 3300, 950.25);
SQL
)
    if echo "$SQLSTATE_OUT" | grep -q "23514"; then
        pass "SQLSTATE 23514 (check_violation) returned"
    else
        fail "expected SQLSTATE 23514, got: $SQLSTATE_OUT"
        FAILURES=$((FAILURES + 1))
    fi
fi

# ── No row may have been written ─────────────────────────────────────────────

ROW_COUNT=$(psql_run -tAc "SELECT COUNT(*) FROM invoices;" | tr -d '[:space:]')
if [ "$ROW_COUNT" = "0" ]; then
    pass "no invoice row inserted"
else
    fail "expected 0 invoice rows after the rejected insert, found $ROW_COUNT"
    FAILURES=$((FAILURES + 1))
fi

# ── The valid shapes must still be accepted ──────────────────────────────────

info "checking valid intrastate and interstate invoices are accepted"
if psql_run >/dev/null 2>&1 <<'SQL'
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
                      total_amount, gst_rate_id, gb_included, gb_used)
VALUES (1, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 950.25);
SQL
then
    pass "intrastate invoice (CGST+SGST, IGST=0) accepted"
else
    fail "valid intrastate invoice was rejected"
    FAILURES=$((FAILURES + 1))
fi

if psql_run >/dev/null 2>&1 <<'SQL'
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
                      total_amount, gst_rate_id, gb_included, gb_used)
VALUES (1, 799.00, 0.00, 0.00, 143.82, 942.82, 1, 3300, 950.25);
SQL
then
    pass "interstate invoice (IGST only) accepted"
else
    fail "valid interstate invoice was rejected"
    FAILURES=$((FAILURES + 1))
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}INT-BIL-006 PASS${NC} — chk_gst_logic blocks dual tax and permits both valid shapes\n"
    exit 0
fi
printf "${RED}INT-BIL-006 FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
