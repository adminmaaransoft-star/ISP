#!/usr/bin/env bash
# verify_migrations.sh — DoD Level 1 migration checks against a real PostgreSQL.
#
# Covers DoD L1-001 (applies cleanly), L1-002 (down then up again), L1-005
# (INCLUDE clauses present), L1-007 (>=3 future partitions) and L1-008 (FK
# enforcement), using goose exactly as the DoD specifies.
#
# Usage:
#   ./scripts/verify_migrations.sh              # throwaway container
#   TEST_DB_DSN=postgres://... ./scripts/verify_migrations.sh
#
# Exit 0 = every check passed.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Under Git Bash, `pwd` yields an MSYS path (/d/repo) that Docker Desktop cannot
# mount, and MSYS rewrites in-container paths like /src into C:/Program Files/...
# unless conversion is disabled. `pwd -W` gives the Windows form; on Linux it
# fails and the plain path is already correct.
MOUNT_ROOT="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

PG_IMAGE="${PG_IMAGE:-postgres:15-alpine}"
GO_IMAGE="${GO_IMAGE:-golang:1.22}"
NETWORK="verify_migrations_net_$$"
CONTAINER="verify_migrations_pg_$$"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }

FAILURES=0

cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { fail "docker is required"; exit 1; }

info "starting PostgreSQL ($PG_IMAGE)"
docker network create "$NETWORK" >/dev/null
docker run -d --rm --name "$CONTAINER" --network "$NETWORK" \
    -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=isp_bss_oss \
    "$PG_IMAGE" >/dev/null

for _ in $(seq 1 60); do
    docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$CONTAINER" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 || {
    fail "PostgreSQL did not become ready within 60s"; exit 1;
}

DSN="postgres://postgres:postgres@${CONTAINER}:5432/isp_bss_oss?sslmode=disable"

psql_q() {
    docker exec -i -e PGPASSWORD=postgres "$CONTAINER" \
        psql -tAqX -U postgres -d isp_bss_oss -c "$1"
}

# goose runs in a Go container on the same network so it can reach the database.
goose() {
    docker run --rm --network "$NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -v "isp_gomodcache:/go/pkg/mod" \
        -v "isp_gobuildcache:/root/.cache/go-build" \
        -e GOFLAGS=-mod=mod \
        "$GO_IMAGE" \
        go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
        -dir ./migrations postgres "$DSN" "$@"
}

# ── L1-001: migrations apply cleanly ─────────────────────────────────────────

info "goose up (DoD L1-001)"
if UP_OUT=$(goose up 2>&1); then
    pass "L1-001: all migrations applied"
else
    fail "L1-001: goose up failed"
    echo "$UP_OUT" | tail -20
    exit 1
fi

EXPECTED_TABLES="franchises plans subscribers encryption_keys kyc_verifications gst_rates invoices wallet_ledgers notification_templates notification_log tickets subscriber_session_history cgnat_allocations lco_ledger revenue_snapshots collections_forecast lea_audit_log"
TABLE_COUNT=0
for t in $EXPECTED_TABLES; do
    TABLE_COUNT=$((TABLE_COUNT + 1))
    COUNT=$(psql_q "SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relname='$t' AND n.nspname='public';" | tr -d '[:space:]')
    if [ "$COUNT" != "1" ]; then
        fail "L1-001: table $t missing after migration"
    fi
done
pass "L1-001: all $TABLE_COUNT expected tables present"

# ── L1-007: partitioned tables have >= 3 future partitions ───────────────────

info "partition coverage (DoD L1-007)"
for parent in subscriber_session_history cgnat_allocations; do
    PARTS=$(psql_q "SELECT COUNT(*) FROM pg_inherits i JOIN pg_class p ON p.oid=i.inhparent WHERE p.relname='$parent';" | tr -d '[:space:]')
    if [ "${PARTS:-0}" -ge 4 ]; then
        pass "L1-007: $parent has $PARTS monthly partitions (current + 3 ahead)"
    else
        fail "L1-007: $parent has only ${PARTS:-0} partitions, want >= 4"
    fi
done

# Partition routing must actually work for a current-month insert.
psql_q "INSERT INTO franchises (id,name,owner_name,mobile_number,commission_rate_pct,status) VALUES (1,'F','O','+910000000000',10.00,'active');" >/dev/null
psql_q "INSERT INTO plans (id,name,rate_limit_string,volume_gb,fup_threshold_bytes,price,validity_days) VALUES (1,'P','100M/100M',3300,3543348019200,799.00,30);" >/dev/null
psql_q "INSERT INTO subscribers (id,caf_number,username,password_hash,mobile_number,plan_id,status,registered_state) VALUES (1,'CAF-1','u@isp','x','+910000000000',1,'active','TN');" >/dev/null

if psql_q "INSERT INTO subscriber_session_history (subscriber_id,session_id,nas_ip_address,start_time) VALUES (1,'s-1','10.0.0.1',NOW());" >/dev/null 2>&1; then
    pass "L1-007: current-month insert routes to a partition"
else
    fail "L1-007: insert into partitioned table failed — no partition covers today"
fi

# ── L1-005: INCLUDE clauses on the LEA indexes ───────────────────────────────

info "INCLUDE clauses on LEA indexes (DoD L1-005)"
LEA_DEF=$(psql_q "SELECT indexdef FROM pg_indexes WHERE indexname='idx_lea_ipv4_time';")
if echo "$LEA_DEF" | grep -q "INCLUDE (subscriber_id, stop_time)"; then
    pass "L1-005: idx_lea_ipv4_time carries INCLUDE (subscriber_id, stop_time)"
else
    fail "L1-005: idx_lea_ipv4_time INCLUDE clause wrong: $LEA_DEF"
fi

CGNAT_DEF=$(psql_q "SELECT indexdef FROM pg_indexes WHERE indexname='idx_cgnat_lea';")
if echo "$CGNAT_DEF" | grep -q "INCLUDE"; then
    pass "L1-005: idx_cgnat_lea carries an INCLUDE clause"
else
    fail "L1-005: idx_cgnat_lea INCLUDE clause missing: $CGNAT_DEF"
fi

# ── L1-008: FK enforcement ───────────────────────────────────────────────────

info "foreign key enforcement (DoD L1-008)"
# An explicit id is needed: the seed rows above set ids directly, which leaves
# the SERIAL sequence at 1, so an implicit id would collide on the primary key
# before PostgreSQL ever evaluated the foreign key.
FK_OUT=$(psql_q "INSERT INTO subscribers (id,caf_number,username,password_hash,mobile_number,plan_id,status,registered_state) VALUES (9001,'CAF-X','orphan@isp','x','+910000000001',9999999,'active','TN');" 2>&1)
if echo "$FK_OUT" | grep -q "violates foreign key constraint"; then
    pass "L1-008: orphan plan_id rejected by FK"
else
    fail "L1-008: orphan FK was not rejected: $FK_OUT"
fi

# ── L1-006: GST check constraint (same assertion as INT-BIL-006) ─────────────

info "GST check constraint (DoD L1-006)"
psql_q "INSERT INTO gst_rates (id,cgst_rate,sgst_rate,igst_rate) VALUES (1,9.00,9.00,18.00);" >/dev/null
GST_OUT=$(psql_q "INSERT INTO invoices (subscriber_id,base_amount,cgst_amount,sgst_amount,igst_amount,total_amount,gst_rate_id,gb_included,gb_used) VALUES (1,799.00,71.91,71.91,143.82,1086.64,1,3300,950.25);" 2>&1)
if echo "$GST_OUT" | grep -q "chk_gst_logic"; then
    pass "L1-006: dual-tax invoice rejected by chk_gst_logic"
else
    fail "L1-006: dual-tax invoice was not rejected: $GST_OUT"
fi

# ── L1-002: down then up again ───────────────────────────────────────────────

info "goose down-to-zero then up again (DoD L1-002)"
if DOWN_OUT=$(goose down-to 0 2>&1); then
    REMAINING=$(psql_q "SELECT COUNT(*) FROM pg_tables WHERE schemaname='public' AND tablename <> 'goose_db_version';" | tr -d '[:space:]')
    if [ "$REMAINING" = "0" ]; then
        pass "L1-002: rollback removed every table"
    else
        fail "L1-002: $REMAINING table(s) survived rollback"
        psql_q "SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename <> 'goose_db_version';"
    fi
else
    fail "L1-002: goose down-to 0 failed"
    echo "$DOWN_OUT" | tail -20
fi

if REUP_OUT=$(goose up 2>&1); then
    pass "L1-002: re-applied cleanly after rollback"
else
    fail "L1-002: goose up after rollback failed"
    echo "$REUP_OUT" | tail -20
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}MIGRATIONS PASS${NC} — DoD L1 checks satisfied\n"
    exit 0
fi
printf "${RED}MIGRATIONS FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
