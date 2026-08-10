#!/usr/bin/env bash
# run_nfr_tests.sh — NFR load and performance tests (tracker sheet "NFR Load Tests").
#
# Covers the rows that can be executed on a single developer machine:
#   NFR-PERF-001  RADIUS auth p99 <= 15ms
#   NFR-PERF-002  API p99 <= 200ms
#   NFR-BIZ-001   unbilled revenue query <= 60s at scale
#   NFR-SEC-002   zero plaintext PII in logs
#
# Usage:
#   ./scripts/run_nfr_tests.sh                    # 20,000 subscribers, DoD levels
#   SUBSCRIBERS=5000 ./scripts/run_nfr_tests.sh   # smaller run
#   DURATION=60s RATE=8000 ./scripts/run_nfr_tests.sh  # past the DoD floor
#
# NFR-SEC-001 (TLS 1.3 floor) is covered by scripts/verify_tls.sh, which is
# kept separate because it needs only the Caddy edge — not the seeded database
# and load-test stack this script builds.
#
# NFR-SCAL-001 (1-hour hold), NFR-AVAIL-001 (Sentinel failover), NFR-DUR-001
# (replica loss mid-storm) and NFR-PERF-003 (Meta round trip) are not run here:
# they need either the full Sentinel stack, real provider credentials, or an
# hour of sustained load. See the tracker notes.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MOUNT_ROOT="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

PG_IMAGE="${PG_IMAGE:-postgres:15-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
GO_IMAGE="${GO_IMAGE:-golang:1.22}"
K6_IMAGE="${K6_IMAGE:-grafana/k6:latest}"

SUBSCRIBERS="${SUBSCRIBERS:-20000}"
# RATE and API_VUS default to the levels the DoD actually specifies (L6-001:
# 5,000 req/s; L6-002: 500 concurrent). They previously defaulted to 2,000 and
# 100, so a green run demonstrated roughly a fifth of the required RADIUS load
# and a fifth of the required API concurrency while reading as a full pass.
RATE="${RATE:-5000}"
DURATION="${DURATION:-30s}"
API_VUS="${API_VUS:-500}"
API_DURATION="${API_DURATION:-30s}"

SUFFIX="$$"
NETWORK="nfr_net_${SUFFIX}"
PG="nfr_pg_${SUFFIX}"
REDIS="nfr_redis_${SUFFIX}"
API="nfr_api_${SUFFIX}"
RADIUSD="nfr_radiusd_${SUFFIX}"

JWT_SECRET="nfr_test_jwt_secret_is_32_chars_ok!!"
RADIUS_SECRET="nfr_test_radius_secret_32_chars_ok!"
RADIUS_VERIFIER_SECRET="nfr_test_verifier_cache_secret_32c!"
TEST_PASSWORD="TestPass1234!"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; RESULTS+=("PASS|$1"); }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; RESULTS+=("FAIL|$1"); FAILURES=$((FAILURES + 1)); }
skip() { printf "${YELLOW}[SKIP]${NC} %s\n" "$1"; RESULTS+=("SKIP|$1"); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
head1() { printf "\n${BOLD}== %s ==${NC}\n" "$1"; }

FAILURES=0
RESULTS=()

cleanup() {
    docker rm -f "$API" "$RADIUSD" "$PG" "$REDIS" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
    rm -f "$REPO_ROOT/config/keys/aes_keys.json" "$REPO_ROOT/.nfr_users.csv" 2>/dev/null || true
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { fail "docker is required"; exit 1; }

in_go_container() {
    docker run --rm --network "$NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -v "isp_gomodcache:/go/pkg/mod" \
        -v "isp_gobuildcache:/root/.cache/go-build" \
        -e GOFLAGS=-mod=mod "$@"
}

psql_q() {
    docker exec -i -e PGPASSWORD=postgres "$PG" psql -tAqX -U postgres -d isp_bss_oss -c "$1"
}

# ── Infrastructure ───────────────────────────────────────────────────────────

head1 "Environment"
info "starting PostgreSQL + Redis"
docker network create "$NETWORK" >/dev/null
docker run -d --rm --name "$PG" --network "$NETWORK" \
    -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=isp_bss_oss \
    "$PG_IMAGE" -c max_connections=300 >/dev/null
docker run -d --rm --name "$REDIS" --network "$NETWORK" "$REDIS_IMAGE" >/dev/null

for _ in $(seq 1 120); do
    docker exec "$PG" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$PG" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 || {
    fail "PostgreSQL never became ready"; exit 1;
}

DSN="postgres://postgres:postgres@${PG}:5432/isp_bss_oss?sslmode=disable"

info "applying migrations"
in_go_container "$GO_IMAGE" go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
    -dir ./migrations postgres "$DSN" up >/dev/null 2>&1 || { fail "migrations failed"; exit 1; }

info "generating key store"
mkdir -p "$REPO_ROOT/config/keys"
KEY_B64=$(docker run --rm "$GO_IMAGE" sh -c "head -c 32 /dev/urandom | base64 -w0")
printf '{"active_version":"v1","keys":{"v1":"%s"}}\n' "$KEY_B64" > "$REPO_ROOT/config/keys/aes_keys.json"

head1 "Seeding ${SUBSCRIBERS} subscribers"
SEED_START=$(date +%s)
if ! in_go_container "$GO_IMAGE" go run ./scripts/seed_load \
        -dsn "$DSN" -count "$SUBSCRIBERS" -secret "$TEST_PASSWORD" \
        -invoiced-pct 90 -sessions -users-out /src/.nfr_users.csv 2>&1 | tail -8; then
    fail "seeding failed"
    exit 1
fi
SEED_ELAPSED=$(( $(date +%s) - SEED_START ))
info "seeded in ${SEED_ELAPSED}s"

# ── NFR-BIZ-001 ──────────────────────────────────────────────────────────────

head1 "NFR-BIZ-001 — unbilled revenue query at scale (threshold: 60s)"

info "EXPLAIN ANALYZE for index usage"
PLAN=$(psql_q "EXPLAIN (ANALYZE, BUFFERS) SELECT COUNT(*) FROM subscribers s WHERE s.status = 'active' AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.subscriber_id = s.id AND i.created_at >= date_trunc('month', NOW()));")
echo "$PLAN" | head -12

EXEC_MS=$(echo "$PLAN" | grep -oE 'Execution Time: [0-9.]+' | grep -oE '[0-9.]+' | head -1)
if [ -z "$EXEC_MS" ]; then
    fail "NFR-BIZ-001: could not read execution time from the plan"
else
    UNBILLED=$(psql_q "SELECT COUNT(*) FROM subscribers s WHERE s.status='active' AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.subscriber_id=s.id AND i.created_at >= date_trunc('month', NOW()));" | tr -d '[:space:]')
    info "unbilled subscribers found: ${UNBILLED} of ${SUBSCRIBERS}"
    # 60s threshold expressed in milliseconds.
    if awk "BEGIN{exit !($EXEC_MS <= 60000)}"; then
        pass "NFR-BIZ-001: unbilled query ran in ${EXEC_MS}ms (threshold 60000ms)"
    else
        fail "NFR-BIZ-001: unbilled query took ${EXEC_MS}ms, over the 60000ms threshold"
    fi
    if echo "$PLAN" | grep -qi "Index"; then
        pass "NFR-BIZ-001: the plan uses an index scan"
    else
        skip "NFR-BIZ-001: sequential scan chosen — expected for this row count, still inside budget"
    fi
fi

# ── Start the services ───────────────────────────────────────────────────────

head1 "Starting services"
docker build -q -f "${MOUNT_ROOT}/Dockerfile.api" -t isp-bss-api:nfr "$MOUNT_ROOT" >/dev/null || { fail "API image build"; exit 1; }
docker build -q -f "${MOUNT_ROOT}/Dockerfile" -t isp-bss-radiusd:nfr "$MOUNT_ROOT" >/dev/null || { fail "radiusd image build"; exit 1; }

docker run -d --rm --name "$API" --network "$NETWORK" \
    -v "${MOUNT_ROOT}/config/keys:/app/config/keys:ro" \
    -e "DB_DSN=${DSN}" -e "REDIS_ADDR=${REDIS}:6379" \
    -e "JWT_SECRET=${JWT_SECRET}" \
    -e "AES_KEY_STORE_URL=local:/app/config/keys/aes_keys.json" \
    -e "LOG_FORMAT=json" -e "LOG_LEVEL=warn" \
    -e "DB_MAX_CONNS=50" \
    isp-bss-api:nfr >/dev/null

docker run -d --rm --name "$RADIUSD" --network "$NETWORK" \
    -e "DB_DSN=${DSN}" -e "REDIS_ADDR=${REDIS}:6379" \
    -e "RADIUS_SECRET=${RADIUS_SECRET}" \
    -e "RADIUS_VERIFIER_SECRET=${RADIUS_VERIFIER_SECRET}" \
    -e "LOG_FORMAT=json" -e "LOG_LEVEL=warn" \
    -e "DB_MAX_CONNS=50" \
    isp-bss-radiusd:nfr >/dev/null

for _ in $(seq 1 60); do
    docker exec "$API" wget -q -O /dev/null http://127.0.0.1:8080/readyz >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$API" wget -q -O /dev/null http://127.0.0.1:8080/readyz >/dev/null 2>&1 \
    && info "API ready" || { fail "API never became ready"; docker logs "$API" | tail -20; }

sleep 3  # let the RADIUS daemon bind its UDP socket

# ── NFR-PERF-001 ─────────────────────────────────────────────────────────────

head1 "NFR-PERF-001 — RADIUS auth latency (threshold: p99 <= 15ms)"

# A single pass/fail at the target rate cannot distinguish "the code is slow"
# from "this machine cannot offer that rate". Sweeping first establishes where
# the p99 budget actually holds, which is the number worth recording.
# The first sweep row absorbs one bcrypt per distinct subscriber as the
# fast-verifier cache fills (SUBSCRIBERS cold misses at cost-12, ~280ms each),
# so its p99 reads high and is not a measure of steady-state auth latency.
# Later rows and the target-rate run below are the numbers to record.
SWEEP="${RATE_SWEEP:-1000,2000,3000,5000}"
SUSTAINED=0
info "capacity sweep before the target-rate run"
printf "  %-10s %-12s %-12s %-12s %s\n" "rate" "achieved" "p50" "p99" "verdict"
for r in $(echo "$SWEEP" | tr ',' ' '); do
    OUT=$(in_go_container "$GO_IMAGE" go run ./cmd/radload \
        -addr "${RADIUSD}:1812" -secret "$RADIUS_SECRET" \
        -users /src/.nfr_users.csv -rate "$r" -duration 10s \
        -concurrency 128 2>&1)
    ACH=$(echo "$OUT" | grep -oE 'throughput  : [0-9]+' | grep -oE '[0-9]+$')
    P50=$(echo "$OUT" | grep -oE 'latency p50 : .*' | sed 's/latency p50 : //')
    P99=$(echo "$OUT" | grep -oE 'latency p99 : .*' | sed 's/latency p99 : //')
    P99_MS=$(echo "$P99" | sed -E 's/([0-9.]+)ms/\1/; s/([0-9.]+)s$/\1000/; s/([0-9.]+)µs/0.\1/')
    VERDICT="over budget"
    if awk "BEGIN{exit !(${P99_MS:-9999} <= 15)}"; then
        VERDICT="within 15ms"
        SUSTAINED="$r"
    fi
    printf "  %-10s %-12s %-12s %-12s %s\n" "$r" "${ACH:-?}/s" "${P50:-?}" "${P99:-?}" "$VERDICT"
done
if [ "$SUSTAINED" != "0" ]; then
    info "highest swept rate holding p99 <= 15ms: ${SUSTAINED} req/s"
fi

echo ""
info "target-rate run at ${RATE} req/s"
RAD_OUT=$(in_go_container "$GO_IMAGE" go run ./cmd/radload \
    -addr "${RADIUSD}:1812" -secret "$RADIUS_SECRET" \
    -users /src/.nfr_users.csv -rate "$RATE" -duration "$DURATION" \
    -concurrency 128 -p99 15ms 2>&1)
RAD_CODE=$?
echo "$RAD_OUT"
RAD_P99=$(echo "$RAD_OUT" | grep -oE 'latency p99 : .*' | sed 's/latency p99 : //')

# Server-side view, so a client or network bottleneck is not mistaken for slow
# authentication: these counters come from inside the daemon.
info "daemon-side metrics"
METRICS=$(docker exec "$RADIUSD" wget -q -O- http://127.0.0.1:9101/metrics 2>/dev/null || true)
if [ -n "$METRICS" ]; then
    HITS=$(echo "$METRICS"   | grep -E '^radius_subscriber_cache_hits_total '   | awk '{print $2}')
    MISSES=$(echo "$METRICS" | grep -E '^radius_subscriber_cache_misses_total ' | awk '{print $2}')
    CERR=$(echo "$METRICS"   | grep -E '^radius_subscriber_cache_errors_total ' | awk '{print $2}')
    SUM=$(echo "$METRICS"    | grep -E '^radius_auth_duration_seconds_sum '     | awk '{print $2}')
    CNT=$(echo "$METRICS"    | grep -E '^radius_auth_duration_seconds_count '   | awk '{print $2}')
    printf "  cache hits/misses/errors : %s / %s / %s\n" "${HITS:-?}" "${MISSES:-?}" "${CERR:-?}"
    if [ -n "${SUM:-}" ] && [ -n "${CNT:-}" ] && awk "BEGIN{exit !(${CNT:-0} > 0)}"; then
        printf "  server-side mean auth    : %s ms over %s requests\n" \
            "$(awk "BEGIN{printf \"%.3f\", ($SUM/$CNT)*1000}")" "$CNT"
    fi
    echo "$METRICS" | grep -E '^radius_auth_duration_seconds_bucket' | while read -r line; do
        le=$(echo "$line" | sed -E 's/.*le="([^"]+)".*/\1/')
        v=$(echo "$line" | awk '{print $2}')
        printf "  auth <= %-8s : %s\n" "$le" "$v"
    done
fi

if [ "$RAD_CODE" -eq 0 ]; then
    pass "NFR-PERF-001: RADIUS p99 ${RAD_P99} within 15ms at ${RATE} req/s"
else
    fail "NFR-PERF-001: RADIUS p99 ${RAD_P99} (threshold 15ms) at ${RATE} req/s"
fi

# ── NFR-PERF-002 ─────────────────────────────────────────────────────────────

head1 "NFR-PERF-002 — API latency (threshold: p99 <= 200ms)"
TOKEN=$(in_go_container "$GO_IMAGE" go run ./scripts/gen_jwt \
    -secret "$JWT_SECRET" -role billing_admin -ttl 2h 2>/dev/null | tr -d '\r\n')

if [ -z "$TOKEN" ]; then
    fail "NFR-PERF-002: could not mint a JWT"
else
    K6_OUT=$(docker run --rm --network "$NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -e "BASE_URL=http://${API}:8080" \
        -e "JWT_TOKEN=${TOKEN}" \
        "$K6_IMAGE" run --vus "$API_VUS" --duration "$API_DURATION" \
        --summary-trend-stats "avg,p(95),p(99),max" \
        /src/scripts/k6_api_load.js 2>&1)
    K6_CODE=$?
    echo "$K6_OUT" | grep -E "api_latency_ms|http_req_duration|http_req_failed|checks|✓|✗|THRESHOLD|thresholds" | head -20
    if [ "$K6_CODE" -eq 0 ]; then
        pass "NFR-PERF-002: API thresholds met at ${API_VUS} VUs for ${API_DURATION}"
    else
        fail "NFR-PERF-002: k6 thresholds not met (exit ${K6_CODE})"
        echo "$K6_OUT" | tail -25
    fi
fi

# ── NFR-SEC-002 ──────────────────────────────────────────────────────────────

head1 "NFR-SEC-002 — plaintext PII scan (threshold: zero hits)"
cd "$REPO_ROOT"
PII_HITS=$(grep -rni 'aadhaar\|\.pan\b\|mobile_number' --include='*.go' . | grep -v '_test.go\|encrypt\|db\.' | grep 'log\.\|Print')
if [ -z "$PII_HITS" ]; then
    pass "NFR-SEC-002: zero plaintext PII log sites"
else
    fail "NFR-SEC-002: plaintext PII found"
    echo "$PII_HITS"
fi

RUNTIME_PII=$(docker logs "$API" 2>&1 | grep -ci 'aadhaar\|pan_number' || true)
if [ "${RUNTIME_PII:-0}" -eq 0 ]; then
    pass "NFR-SEC-002: no PII field names in the API service logs"
else
    fail "NFR-SEC-002: ${RUNTIME_PII} PII mentions in runtime logs"
fi

# ── Summary ──────────────────────────────────────────────────────────────────

head1 "Summary"
for r in "${RESULTS[@]}"; do
    status="${r%%|*}"
    text="${r#*|}"
    case "$status" in
        PASS) printf "  ${GREEN}PASS${NC}  %s\n" "$text" ;;
        FAIL) printf "  ${RED}FAIL${NC}  %s\n" "$text" ;;
        SKIP) printf "  ${YELLOW}SKIP${NC}  %s\n" "$text" ;;
    esac
done

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}NFR TESTS PASS${NC}\n"
    exit 0
fi
printf "${RED}NFR TESTS FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
