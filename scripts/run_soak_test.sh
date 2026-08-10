#!/usr/bin/env bash
# run_soak_test.sh — NFR-SCAL-001 / DoD L6-005.
#
# Holds sustained RADIUS load against 20,000 seeded subscribers for an hour and
# samples the daemon's runtime metrics every 30s, to show that goroutines, file
# descriptors and heap do not grow without bound.
#
#   Budget: no goroutine leak, no OOM, over a 1-hour hold.
#
# ── What this measures, and what it cannot ───────────────────────────────────
#
# Detection needs no pprof. The daemon already exposes everything required on
# its Prometheus endpoint via the default Go collector — verified present on a
# running daemon:
#
#     go_goroutines, go_threads, go_memstats_heap_alloc_bytes, process_open_fds
#
# A leak shows up as monotonic growth in go_goroutines or process_open_fds
# across the hold. What these counters cannot tell you is *where* the leak is;
# that needs net/http/pprof, which is NOT imported anywhere in this codebase.
# If this script reports a leak, adding pprof to cmd/radiusd is the next step,
# not a prerequisite for running it.
#
# ── A note on the tracker's acceptance criteria ──────────────────────────────
#
# The NFR sheet lists "radius_worker_queue_depth < 50" as a pass condition for
# this row. That metric does not exist — the only queue-depth gauge in the
# codebase is fup_dead_letter_queue_depth, which is unrelated. This script
# therefore does not evaluate it, and reports it as SKIP rather than silently
# treating a missing metric as satisfied. Either add the gauge to the RADIUS
# worker pool or amend the row; as written it cannot be graded.
#
# ── Usage ────────────────────────────────────────────────────────────────────
#
#   ./scripts/run_soak_test.sh                       # the full 1-hour hold
#   DURATION=300s ./scripts/run_soak_test.sh         # 5-minute smoke run
#   SAMPLE_INTERVAL=10 DURATION=120s ./scripts/run_soak_test.sh
#
# Output: a TSV of every sample at soak_metrics_<timestamp>.tsv, so the run can
# be re-analysed or plotted without repeating an hour of load.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
MOUNT_ROOT="$(pwd -W 2>/dev/null || pwd)"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

DURATION="${DURATION:-3600s}"
RATE="${RATE:-333}"
SUBSCRIBERS="${SUBSCRIBERS:-20000}"
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-30}"
METRICS_URL="${METRICS_URL:-http://127.0.0.1:9100/metrics}"
# Fraction of growth tolerated between the settle point and the end of the hold.
GROWTH_TOLERANCE_PCT="${GROWTH_TOLERANCE_PCT:-10}"
# Samples ignored at the start, while caches fill and pools reach steady state.
# Comparing against t=0 would flag normal warm-up as a leak.
SETTLE_SAMPLES="${SETTLE_SAMPLES:-20}"

# Must match the project name scripts/demo_up.sh exports, or every container
# and the compose network resolve under a different prefix.
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-isp_bss_demo}"
export COMPOSE_PROJECT_NAME
COMPOSE_NETWORK="${COMPOSE_PROJECT_NAME}_bss_internal"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; RESULTS+=("PASS|$1"); }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; RESULTS+=("FAIL|$1"); FAILURES=$((FAILURES + 1)); }
skip() { printf "${YELLOW}[SKIP]${NC} %s\n" "$1"; RESULTS+=("SKIP|$1"); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
head1() { printf "\n${BOLD}== %s ==${NC}\n" "$1"; }

FAILURES=0
RESULTS=()
SAMPLES="$REPO_ROOT/soak_metrics_$(date +%Y%m%d_%H%M%S).tsv"
LOAD_PID=""

cleanup() {
    [ -n "$LOAD_PID" ] && kill "$LOAD_PID" 2>/dev/null
    rm -f "$REPO_ROOT/.soak_users.csv" 2>/dev/null
    info "stack left running — 'docker compose down -v' to tear it down"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { echo "docker is required"; exit 1; }

# duration_seconds converts a Go-style duration (3600s, 60m, 1h) to seconds.
duration_seconds() {
    local d="$1"
    case "$d" in
        *h) echo $(( ${d%h} * 3600 )) ;;
        *m) echo $(( ${d%m} * 60 )) ;;
        *s) echo "${d%s}" ;;
        *)  echo "$d" ;;
    esac
}

# metric reads one bare Prometheus gauge from the daemon's endpoint.
metric() {
    curl -s --max-time 5 "$METRICS_URL" 2>/dev/null \
        | grep -E "^$1 " | awk '{print $2}' | head -1
}

DURATION_S=$(duration_seconds "$DURATION")

head1 "Environment"
info "duration ${DURATION} (${DURATION_S}s) | rate ${RATE} req/s | ${SUBSCRIBERS} subscribers"
info "sampling every ${SAMPLE_INTERVAL}s -> $(basename "$SAMPLES")"

info "bringing up the stack via scripts/demo_up.sh"
if ! bash scripts/demo_up.sh >/dev/null 2>&1; then
    fail "demo_up.sh failed — run it directly to see why"
    exit 1
fi

# demo_up.sh generates any missing secrets into .env, so this must come after
# it. Without it the DB password and RADIUS secret fall back to defaults that
# do not match the running stack, and every connection is refused.
if [ -f .env ]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
fi

if [ -z "$(metric go_goroutines)" ]; then
    fail "no go_goroutines at ${METRICS_URL} — is aaa_core_daemon up and its metrics port published?"
    docker compose ps aaa_core_daemon
    exit 1
fi
pass "daemon metrics reachable at ${METRICS_URL}"

head1 "Seeding ${SUBSCRIBERS} subscribers"
DSN="postgres://postgres:${DB_SECURE_PASSWORD:-postgres}@postgres_primary:5432/isp_bss_oss?sslmode=disable"

# demo_up.sh seeds demo subscribers from scripts/seed_local.sql, and seed_load
# starts its IDs at 1, so the two collide on subscribers_pkey. The soak test
# needs every subscriber to share a known password (radload has to authenticate
# as them), which the demo rows do not, so reusing them is not an option —
# this run owns the dataset. Set RESET_DB=0 to seed on top of whatever is
# already there, which will fail unless the table is empty.
if [ "${RESET_DB:-1}" = "1" ]; then
    info "clearing existing subscriber data (RESET_DB=0 to skip)"
    docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss -q \
        -c "TRUNCATE subscribers RESTART IDENTITY CASCADE;" >/dev/null 2>&1 || {
        fail "could not clear subscriber data"; exit 1;
    }
fi

if ! docker run --rm --network "$COMPOSE_NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod golang:1.22 \
        go run ./scripts/seed_load -dsn "$DSN" -count "$SUBSCRIBERS" \
        -secret "${TEST_PASSWORD:-TestPass1234!}" -sessions \
        -users-out /src/.soak_users.csv 2>&1 | tail -3; then
    fail "seeding failed"
    exit 1
fi

head1 "NFR-SCAL-001 — ${DURATION} hold at ${RATE} req/s"

docker run --rm --network "$COMPOSE_NETWORK" \
    -v "${MOUNT_ROOT}:/src" -w /src \
    -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
    -e GOFLAGS=-mod=mod golang:1.22 \
    go run ./cmd/radload -addr "bss_aaa_core_daemon:1812" \
    -secret "${RADIUS_SECRET:-testing123}" -users /src/.soak_users.csv \
    -rate "$RATE" -duration "$DURATION" -timeout 10s \
    > "$REPO_ROOT/.soak_radload.out" 2>&1 &
LOAD_PID=$!

printf 'elapsed_s\tgoroutines\tthreads\theap_bytes\topen_fds\n' > "$SAMPLES"

START=$(date +%s)
SAMPLE_N=0
printf "  %-9s %-12s %-9s %-14s %s\n" "elapsed" "goroutines" "threads" "heap_MB" "open_fds"
while :; do
    NOW=$(date +%s)
    ELAPSED=$(( NOW - START ))
    [ "$ELAPSED" -ge "$DURATION_S" ] && break

    G=$(metric go_goroutines)
    TH=$(metric go_threads)
    HEAP=$(metric go_memstats_heap_alloc_bytes)
    FDS=$(metric process_open_fds)

    if [ -n "$G" ]; then
        printf '%s\t%s\t%s\t%s\t%s\n' "$ELAPSED" "$G" "$TH" "$HEAP" "$FDS" >> "$SAMPLES"
        SAMPLE_N=$((SAMPLE_N + 1))
        # Print every 10th sample so an hour-long run does not emit 120 lines.
        if [ $(( SAMPLE_N % 10 )) -eq 1 ]; then
            HEAP_MB=$(awk "BEGIN{printf \"%.1f\", ${HEAP:-0}/1048576}")
            printf "  %-9s %-12s %-9s %-14s %s\n" "${ELAPSED}s" "$G" "${TH:-?}" "$HEAP_MB" "${FDS:-?}"
        fi
    else
        # A scrape failure mid-hold is itself a finding — the daemon may have
        # died. Recorded rather than skipped silently.
        printf '%s\tSCRAPE_FAILED\t\t\t\n' "$ELAPSED" >> "$SAMPLES"
        info "metrics scrape failed at ${ELAPSED}s"
    fi

    sleep "$SAMPLE_INTERVAL"
done

wait "$LOAD_PID" 2>/dev/null
LOAD_PID=""

# ── Analysis ─────────────────────────────────────────────────────────────────

head1 "Analysis"

TOTAL=$(grep -c . "$SAMPLES")
TOTAL=$(( TOTAL - 1 )) # header
info "collected ${TOTAL} samples"

SCRAPE_FAILS=$(grep -c 'SCRAPE_FAILED' "$SAMPLES")
if [ "$SCRAPE_FAILS" -gt 0 ]; then
    fail "NFR-SCAL-001: ${SCRAPE_FAILS} metrics scrape(s) failed during the hold — the daemon may have restarted"
fi

if [ "$TOTAL" -le "$SETTLE_SAMPLES" ]; then
    skip "NFR-SCAL-001: only ${TOTAL} samples, need more than ${SETTLE_SAMPLES} to judge a trend (raise DURATION)"
else
    # Compare the post-settle baseline against the tail. Growth during warm-up
    # is expected — worker pools spin up, caches fill — and comparing against
    # t=0 would report that as a leak.
    analyse() {
        local col="$1" label="$2"
        local base tail_
        base=$(awk -F'\t' -v c="$col" -v s="$SETTLE_SAMPLES" 'NR>1 && $2!="SCRAPE_FAILED" && NR<=s+1 {v=$c; n++} END{if(n)print v}' "$SAMPLES")
        tail_=$(awk -F'\t' -v c="$col" '$2!="SCRAPE_FAILED" {v=$c} END{print v}' "$SAMPLES")
        if [ -z "$base" ] || [ -z "$tail_" ] || [ "$base" = "0" ]; then
            skip "NFR-SCAL-001: ${label} — not enough data to compare"
            return
        fi
        local growth
        growth=$(awk "BEGIN{printf \"%.1f\", (($tail_ - $base)/$base)*100}")
        info "${label}: ${base} at settle -> ${tail_} at end (${growth}%)"
        if awk "BEGIN{exit !($growth <= $GROWTH_TOLERANCE_PCT)}"; then
            pass "NFR-SCAL-001: ${label} stable across the hold (${growth}%, tolerance ${GROWTH_TOLERANCE_PCT}%)"
        else
            fail "NFR-SCAL-001: ${label} grew ${growth}% across the hold (tolerance ${GROWTH_TOLERANCE_PCT}%) — probable leak"
        fi
    }

    analyse 2 "goroutines"
    analyse 5 "open file descriptors"

    # Heap is reported but not gated: Go's heap sawtooths with GC, so a single
    # end-of-run reading is not evidence either way. The TSV is there to plot.
    HEAP_MAX=$(awk -F'\t' 'NR>1 && $2!="SCRAPE_FAILED" && $4>m {m=$4} END{printf "%.1f", m/1048576}' "$SAMPLES")
    info "peak heap: ${HEAP_MAX} MB (reported, not gated — GC sawtooth makes a single reading meaningless)"
fi

skip "NFR-SCAL-001: radius_worker_queue_depth not evaluated — the metric does not exist (see this script's header)"

if [ -f "$REPO_ROOT/.soak_radload.out" ]; then
    info "load generator summary:"
    grep -E 'requests|error rate|latency p99|throughput' "$REPO_ROOT/.soak_radload.out" | sed 's/^/  /'
    rm -f "$REPO_ROOT/.soak_radload.out"
fi

# ── Summary ──────────────────────────────────────────────────────────────────

head1 "Summary"
for r in "${RESULTS[@]}"; do
    status="${r%%|*}"; text="${r#*|}"
    case "$status" in
        PASS) printf "  ${GREEN}PASS${NC}  %s\n" "$text" ;;
        FAIL) printf "  ${RED}FAIL${NC}  %s\n" "$text" ;;
        SKIP) printf "  ${YELLOW}SKIP${NC}  %s\n" "$text" ;;
    esac
done

echo ""
info "samples retained at $(basename "$SAMPLES")"
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}SOAK TEST PASS${NC}\n"
    exit 0
fi
printf "${RED}SOAK TEST FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
