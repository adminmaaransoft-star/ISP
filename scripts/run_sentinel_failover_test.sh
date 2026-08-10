#!/usr/bin/env bash
# run_sentinel_failover_test.sh — NFR-AVAIL-001 / DoD L6-004.
#
# Kills the Redis master under live RADIUS load and measures how long Sentinel
# takes to promote a replica, and how long authentication takes to recover.
#
#   Budget: new master elected within 3s, RADIUS auth resumes within 5s.
#
# ── Why this script edits sentinel.conf ──────────────────────────────────────
#
# The committed config/redis/sentinel.conf sets:
#
#     sentinel down-after-milliseconds bss_master 3000
#
# That is the time Sentinel waits before it even *considers* the master down.
# Election cannot begin until it elapses, so with 3000ms the detection step
# alone consumes the entire 3-second budget and the NFR cannot pass however
# fast the election itself is. This is a configuration conflict, not a
# performance problem — the same shape as the bcrypt/NFR-PERF-001 conflict.
#
# So this script runs against a temporary copy of sentinel.conf with
# down-after-milliseconds lowered to 1000ms (override with DOWN_AFTER_MS),
# leaving roughly 2s of the budget for the election itself. The committed
# config is never modified: the copy lives under a scratch directory and is
# removed on exit.
#
# If this test passes at 1000ms, the follow-up decision is a product one —
# lower the committed value to match, or renegotiate the 3s target. Running it
# against the committed 3000ms only ever measures the threshold.
#
# ── How the master is taken down, and why it matters ─────────────────────────
#
# CHAOS_MODE=pause (the default) pauses the master's container: it stops
# answering, but stays on the Docker network and keeps resolving in DNS. That
# is what a crashed or hung redis-server on a surviving host looks like, and it
# is the scenario NFR-AVAIL-001 is about.
#
# CHAOS_MODE=kill removes the container outright. That also removes it from
# Docker's DNS — and because sentinel.conf monitors the master by *hostname*
# (`sentinel monitor bss_master redis_primary`, with resolve-hostnames yes,
# which the config comments note is mandatory under compose), Sentinel can no
# longer resolve the name at all. Observed result: it logs
#
#     # Failed to resolve hostname 'redis_primary'
#     # +tilt #tilt mode entered
#
# and never promotes a replica. Sixty seconds after the kill there is still no
# new master and authentication never recovers.
#
# That is worth knowing rather than hiding behind a gentler default: under an
# orchestrator, a node failure takes the DNS record with it, which is exactly
# the kill case. Hostname-based monitoring survives a dead *process* but not a
# vanished *name*. Whether that risk is acceptable is a deployment decision —
# pinning IPs, or a DNS entry that outlives the container, would address it.
# The default is pause so the script measures Sentinel's failover timing rather
# than re-demonstrating this one known failure each run.
#
# ── Usage ────────────────────────────────────────────────────────────────────
#
#   ./scripts/run_sentinel_failover_test.sh
#   DOWN_AFTER_MS=500 ./scripts/run_sentinel_failover_test.sh
#   CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh  # the DNS case above
#   SKIP_LOAD=1 ./scripts/run_sentinel_failover_test.sh   # election timing only
#
# Requires the full compose stack (3 sentinels + 2 replicas), which
# scripts/run_nfr_tests.sh deliberately does not build — hence a separate
# script rather than another section there.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
MOUNT_ROOT="$(pwd -W 2>/dev/null || pwd)"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

DOWN_AFTER_MS="${DOWN_AFTER_MS:-1000}"
# parallel-syncs controls how many replicas Sentinel reconfigures at once after
# a promotion. The committed config uses 1, which makes reconfiguration serial;
# the event log shows each replica costing ~1s, so with two replicas that is
# ~2s of the failover. Exposed here so the trade-off can be measured rather
# than argued about. Empty leaves the committed value untouched.
PARALLEL_SYNCS="${PARALLEL_SYNCS:-}"
ELECTION_BUDGET_MS="${ELECTION_BUDGET_MS:-3000}"
AUTH_RESUME_BUDGET_MS="${AUTH_RESUME_BUDGET_MS:-5000}"
MASTER_NAME="${REDIS_MASTER_NAME:-bss_master}"
LOAD_RATE="${LOAD_RATE:-200}"
LOAD_DURATION="${LOAD_DURATION:-90s}"
SUBSCRIBERS="${SUBSCRIBERS:-500}"
SKIP_LOAD="${SKIP_LOAD:-0}"
CHAOS_MODE="${CHAOS_MODE:-pause}"   # pause | kill — see the header
# The sentinel whose log the timeline is read from. Any of the three records
# the same +sdown/+switch-master events; one is picked so the numbers all come
# from a single clock.
SENTINEL_LEADER="${SENTINEL_LEADER:-bss_redis_sentinel_1}"
SENTINEL_LEADER_SVC="${SENTINEL_LEADER_SVC:-redis_sentinel_1}"

# Must match the project name scripts/demo_up.sh exports, or every container
# and the compose network resolve under a different prefix.
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-isp_bss_demo}"
export COMPOSE_PROJECT_NAME

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; RESULTS+=("PASS|$1"); }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; RESULTS+=("FAIL|$1"); FAILURES=$((FAILURES + 1)); }
skip() { printf "${YELLOW}[SKIP]${NC} %s\n" "$1"; RESULTS+=("SKIP|$1"); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
head1() { printf "\n${BOLD}== %s ==${NC}\n" "$1"; }

FAILURES=0
RESULTS=()

SCRATCH="$(mktemp -d)"
LOAD_PID=""
PAUSED_CONTAINER=""
cleanup() {
    [ -n "$LOAD_PID" ] && kill "$LOAD_PID" 2>/dev/null
    # A paused container would otherwise stay frozen after the run, leaving the
    # stack in a state that looks like a broken Redis on the next test.
    [ -n "$PAUSED_CONTAINER" ] && docker unpause "$PAUSED_CONTAINER" >/dev/null 2>&1
    rm -rf "$SCRATCH"
    # The stack is left running on purpose: a failed failover is worth
    # inspecting, and tearing it down would destroy the evidence.
    info "stack left running — 'docker compose down -v' to tear it down"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { echo "docker is required"; exit 1; }

# sentinel_cli runs redis-cli against whichever sentinel is named.
sentinel_cli() {
    local node="$1"; shift
    docker compose exec -T "$node" redis-cli -p 26379 "$@" 2>/dev/null
}

# current_master echoes the master's host as Sentinel currently sees it. This
# may be a hostname (redis_primary) or a bare IP, depending on whether the node
# was configured by name or promoted during an earlier failover.
#
# Used only as a completion gate, never as the measurement: each call costs a
# `docker compose exec` round trip (~390ms measured on Docker Desktop), so a
# poll loop built on it reports its own latency, not Sentinel's. An earlier
# version of this script did exactly that and reported a 4955ms failover that
# Sentinel's own log showed completing in under 1.5s.
# Queries the same node the timeline is read from: sentinels switch a fraction
# of a second apart, so polling one and reading another's log races — the poll
# can report the new master before that node has written +switch-master.
current_master() {
    sentinel_cli "$SENTINEL_LEADER_SVC" sentinel get-master-addr-by-name "$MASTER_NAME" | head -1 | tr -d '\r'
}

# sentinel_event_ms echoes the epoch-milliseconds of the last Sentinel log line
# matching a pattern, or nothing if it never appeared.
#
# Sentinel timestamps its own events to the millisecond, and the container
# clock matches the host's, so this is both more accurate and more honest than
# timing from outside: it measures what Sentinel did, not how fast this script
# noticed. Log times are UTC and must be parsed as such — `date -d` would
# otherwise read them in the host's local zone.
sentinel_event_ms() {
    local pattern="$1"
    local line
    line=$(docker logs "$SENTINEL_LEADER" 2>&1 | grep -E "$pattern" | tail -1)
    [ -z "$line" ] && return 1
    local ts
    ts=$(echo "$line" | sed -E 's/^[0-9]+:X ([0-9]+ [A-Za-z]+ [0-9]+ [0-9:]+\.[0-9]+) .*/\1/')
    [ -z "$ts" ] && return 1
    date -u -d "$ts UTC" +%s%3N 2>/dev/null
}

# master_container maps whatever current_master reports to the container the
# chaos step should act on.
#
# It must not assume bss_redis_primary: after any previous failover the master
# is a promoted replica, and pausing the original primary would then take down
# a node Sentinel is not monitoring as master. The test would report "no
# election" and look like a failover failure when nothing was ever asked to
# fail over.
master_container() {
    local addr="$1"
    # Sentinel reports the compose *service* name (redis_primary), which is not
    # the container name (bss_redis_primary) — docker inspect on it fails. Ask
    # compose to resolve the service to a container id first.
    local cid
    cid=$(docker compose ps -q "$addr" 2>/dev/null | head -1 | tr -d '\r')
    if [ -n "$cid" ]; then
        docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##' | tr -d '\r'
        return
    fi
    # A literal container name also works.
    if docker inspect "$addr" >/dev/null 2>&1; then
        echo "$addr"
        return
    fi
    # Otherwise match the IP against each Redis container's network address.
    local c
    for c in bss_redis_primary bss_redis_replica_1 bss_redis_replica_2; do
        local ip
        ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$c" 2>/dev/null | tr -d '\r')
        if [ -n "$ip" ] && [ "$ip" = "$addr" ]; then
            echo "$c"
            return
        fi
    done
    return 1
}

# ── Config override ──────────────────────────────────────────────────────────

head1 "Environment"

if [ ! -f config/redis/sentinel.conf ]; then
    echo "config/redis/sentinel.conf not found"; exit 1
fi

info "sentinel down-after-milliseconds: ${DOWN_AFTER_MS}ms (committed config uses $(grep -oE 'down-after-milliseconds [a-z_]+ [0-9]+' config/redis/sentinel.conf | grep -oE '[0-9]+$'))"

# Sentinel rewrites its own config file at runtime (it records the observed
# topology), so it needs a writable copy — mounting the committed file would
# let a test run modify a tracked file.
mkdir -p "$SCRATCH/redis"
sed -E "s/(down-after-milliseconds ${MASTER_NAME}) [0-9]+/\1 ${DOWN_AFTER_MS}/" \
    config/redis/sentinel.conf > "$SCRATCH/redis/sentinel.conf"

if ! grep -q "down-after-milliseconds ${MASTER_NAME} ${DOWN_AFTER_MS}" "$SCRATCH/redis/sentinel.conf"; then
    echo "failed to rewrite down-after-milliseconds — is the master name still ${MASTER_NAME}?"
    exit 1
fi

if [ -n "$PARALLEL_SYNCS" ]; then
    info "sentinel parallel-syncs: ${PARALLEL_SYNCS} (committed config uses $(grep -oE 'parallel-syncs [a-z_]+ [0-9]+' config/redis/sentinel.conf | grep -oE '[0-9]+$'))"
    sed -i -E "s/(parallel-syncs ${MASTER_NAME}) [0-9]+/\1 ${PARALLEL_SYNCS}/" "$SCRATCH/redis/sentinel.conf"
fi

SCRATCH_MOUNT="$(cd "$SCRATCH" && { pwd -W 2>/dev/null || pwd; })"

# A compose override is cleaner than editing docker-compose.yml: it is additive,
# and it disappears with the scratch directory.
cat > "$SCRATCH/override.yml" <<YAML
services:
  redis_sentinel_1:
    volumes:
      - ${SCRATCH_MOUNT}/redis/sentinel.conf:/etc/redis/sentinel.conf
  redis_sentinel_2:
    volumes:
      - ${SCRATCH_MOUNT}/redis/sentinel.conf:/etc/redis/sentinel.conf
  redis_sentinel_3:
    volumes:
      - ${SCRATCH_MOUNT}/redis/sentinel.conf:/etc/redis/sentinel.conf
YAML

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
    MASTER_NAME="${REDIS_MASTER_NAME:-$MASTER_NAME}"
fi

# Restart only the sentinels, now with the lowered threshold. demo_up.sh is
# reused for everything else so this script cannot drift from how the stack is
# actually started (the NFR harness once diverged that way and silently stopped
# setting RADIUS_VERIFIER_SECRET).
info "restarting sentinels with down-after-milliseconds=${DOWN_AFTER_MS}"
# The -f path must be the Windows form: MSYS_NO_PATHCONV=1 (needed elsewhere for
# docker arguments) stops the shell rewriting /tmp/... , and the Docker Desktop
# binary then resolves that raw POSIX path against the current drive as
# D:\tmp\... and cannot find it. Output is NOT silenced: when this failed
# quietly, every run below reported timings for the committed 3000ms threshold
# while claiming to measure the overridden one.
if ! docker compose -f docker-compose.yml -f "${SCRATCH_MOUNT}/override.yml" up -d --force-recreate \
        redis_sentinel_1 redis_sentinel_2 redis_sentinel_3 2>&1 | tail -3; then
    fail "could not restart the sentinels with the overridden config"
    exit 1
fi

# Prove the override actually landed. A silently-ignored override produces
# plausible numbers for the wrong configuration, which is worse than an error.
sleep 3
EFFECTIVE=$(sentinel_cli "$SENTINEL_LEADER_SVC" sentinel master "$MASTER_NAME" \
    | tr -d '\r' | grep -A1 '^down-after-milliseconds$' | tail -1)
if [ -n "$EFFECTIVE" ] && [ "$EFFECTIVE" != "$DOWN_AFTER_MS" ]; then
    fail "sentinel is running with down-after-milliseconds=${EFFECTIVE}, not the requested ${DOWN_AFTER_MS} — the override did not take effect"
    exit 1
fi
info "verified sentinel is running with down-after-milliseconds=${EFFECTIVE:-unknown}"

info "waiting for the sentinels to agree on a master"
ORIGINAL_MASTER=""
for _ in $(seq 1 60); do
    ORIGINAL_MASTER="$(current_master)"
    [ -n "$ORIGINAL_MASTER" ] && break
    sleep 1
done
if [ -z "$ORIGINAL_MASTER" ]; then
    fail "sentinels never reported a master for ${MASTER_NAME}"
    docker compose logs --tail 30 redis_sentinel_1
    exit 1
fi
pass "sentinels agree the master is ${ORIGINAL_MASTER}"

QUORUM_OK=$(sentinel_cli redis_sentinel_1 sentinel ckquorum "$MASTER_NAME" | tr -d '\r')
info "quorum check: ${QUORUM_OK:-unknown}"

# ── Load ─────────────────────────────────────────────────────────────────────

if [ "$SKIP_LOAD" != "1" ]; then
    head1 "Starting RADIUS load"
    DSN="postgres://postgres:${DB_SECURE_PASSWORD:-postgres}@postgres_primary:5432/isp_bss_oss?sslmode=disable"
    COMPOSE_NETWORK="${COMPOSE_PROJECT_NAME}_bss_internal"

    # demo_up.sh already seeded demo subscribers and seed_load starts its IDs at
    # 1, so the two collide on subscribers_pkey. radload must authenticate as
    # these subscribers, which needs the shared password only seed_load sets, so
    # the demo rows cannot be reused. RESET_DB=0 to seed on top instead.
    if [ "${RESET_DB:-1}" = "1" ]; then
        info "clearing existing subscriber data (RESET_DB=0 to skip)"
        docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss -q \
            -c "TRUNCATE subscribers RESTART IDENTITY CASCADE;" >/dev/null 2>&1 || {
            fail "could not clear subscriber data"; exit 1;
        }
    fi

    # Not silenced: a failed seed here would otherwise surface later as a
    # 100% auth error rate that looks like a failover problem.
    info "seeding ${SUBSCRIBERS} subscribers"
    if ! docker run --rm --network "$COMPOSE_NETWORK" \
            -v "${MOUNT_ROOT}:/src" -w /src \
            -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
            -e GOFLAGS=-mod=mod golang:1.22 \
            go run ./scripts/seed_load -dsn "$DSN" -count "$SUBSCRIBERS" \
            -secret "${TEST_PASSWORD:-TestPass1234!}" -users-out /src/.failover_users.csv 2>&1 | tail -2; then
        fail "seeding failed"
        exit 1
    fi

    info "running RADIUS load at ${LOAD_RATE} req/s for ${LOAD_DURATION}"
    docker run --rm --network "$COMPOSE_NETWORK" \
        -v "${MOUNT_ROOT}:/src" -w /src \
        -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
        -e GOFLAGS=-mod=mod golang:1.22 \
        go run ./cmd/radload -addr "bss_aaa_core_daemon:1812" \
        -secret "${RADIUS_SECRET:-testing123}" -users /src/.failover_users.csv \
        -rate "$LOAD_RATE" -duration "$LOAD_DURATION" \
        > "$SCRATCH/radload.out" 2>&1 &
    LOAD_PID=$!
    sleep 10  # let the load settle before injecting the fault
else
    skip "RADIUS load skipped (SKIP_LOAD=1) — election timing only"
fi

# ── Chaos ────────────────────────────────────────────────────────────────────

head1 "NFR-AVAIL-001 — killing the Redis master"

TARGET="$(master_container "$ORIGINAL_MASTER")"
if [ -z "$TARGET" ]; then
    fail "could not map the current master (${ORIGINAL_MASTER}) to a container"
    exit 1
fi

case "$CHAOS_MODE" in
    pause)
        info "pausing ${TARGET} (stays in DNS, stops responding)"
        KILL_MS=$(date +%s%3N)
        docker pause "$TARGET" >/dev/null 2>&1 || {
            fail "could not pause ${TARGET}"; exit 1;
        }
        PAUSED_CONTAINER="$TARGET"
        ;;
    kill)
        info "killing ${TARGET} (also removes it from Docker DNS)"
        KILL_MS=$(date +%s%3N)
        docker kill "$TARGET" >/dev/null 2>&1 || {
            fail "could not kill ${TARGET}"; exit 1;
        }
        ;;
    *)
        echo "CHAOS_MODE must be 'pause' or 'kill', got '${CHAOS_MODE}'"; exit 1
        ;;
esac

# Wait for the switch to land. This loop is only a completion gate — its own
# latency does not enter the result, which comes from Sentinel's log below.
NEW_MASTER=""
DEADLINE=$(( KILL_MS + 60000 ))
while :; do
    NOW_MS=$(date +%s%3N)
    [ "$NOW_MS" -gt "$DEADLINE" ] && break
    CANDIDATE="$(current_master)"
    if [ -n "$CANDIDATE" ] && [ "$CANDIDATE" != "$ORIGINAL_MASTER" ]; then
        NEW_MASTER="$CANDIDATE"
        break
    fi
    sleep 0.2
done

if [ -z "$NEW_MASTER" ]; then
    fail "NFR-AVAIL-001: no new master elected within 60s"
    docker compose logs --tail 40 "$SENTINEL_LEADER"
else
    info "new master: ${NEW_MASTER} (was ${ORIGINAL_MASTER})"

    # Take the numbers from Sentinel's own event log rather than from the poll
    # loop above. Detection and failover are reported separately because they
    # are tuned by different knobs and only one of them is under Sentinel's
    # control once down-after-milliseconds is set.
    # The log line can lag the address change by a fraction of a second, so
    # retry briefly for an event newer than the fault rather than accepting a
    # stale one from a previous run.
    SDOWN_MS=""; SWITCH_MS=""
    for _ in $(seq 1 25); do
        SDOWN_MS=$(sentinel_event_ms '\+sdown master')
        SWITCH_MS=$(sentinel_event_ms '\+switch-master')
        if [ -n "$SWITCH_MS" ] && [ "$SWITCH_MS" -ge "$KILL_MS" ]; then
            break
        fi
        sleep 0.2
    done

    if [ -z "$SDOWN_MS" ] || [ -z "$SWITCH_MS" ] || [ "$SWITCH_MS" -lt "$KILL_MS" ]; then
        skip "NFR-AVAIL-001: could not read the failover timeline from ${SENTINEL_LEADER}'s log; falling back to the poll loop, which overstates by its own latency"
        ELECTED_MS=$(( $(date +%s%3N) - KILL_MS ))
    else
        DETECT_MS=$(( SDOWN_MS - KILL_MS ))
        FAILOVER_MS=$(( SWITCH_MS - SDOWN_MS ))
        ELECTED_MS=$(( SWITCH_MS - KILL_MS ))
        info "fault -> +sdown (detection, bounded by down-after-milliseconds=${DOWN_AFTER_MS}): ${DETECT_MS}ms"
        info "+sdown -> +switch-master (Sentinel's own failover sequence): ${FAILOVER_MS}ms"
    fi

    if [ "$ELECTED_MS" -le "$ELECTION_BUDGET_MS" ]; then
        pass "NFR-AVAIL-001: master elected in ${ELECTED_MS}ms (budget ${ELECTION_BUDGET_MS}ms)"
    else
        fail "NFR-AVAIL-001: master elected in ${ELECTED_MS}ms, over the ${ELECTION_BUDGET_MS}ms budget"
    fi
fi

# ── Auth recovery ────────────────────────────────────────────────────────────

head1 "Authentication recovery"

if [ "$SKIP_LOAD" = "1" ]; then
    skip "auth recovery not measured (SKIP_LOAD=1)"
else
    # A successful election is not the same as a working service: go-redis has
    # to notice the new topology too. Measure until an auth actually succeeds.
    COMPOSE_NETWORK="${COMPOSE_PROJECT_NAME}_bss_internal"
    RESUMED_MS=0
    DEADLINE=$(( KILL_MS + 60000 ))
    while :; do
        NOW_MS=$(date +%s%3N)
        [ "$NOW_MS" -gt "$DEADLINE" ] && break
        if docker run --rm --network "$COMPOSE_NETWORK" \
                -v "${MOUNT_ROOT}:/src" -w /src \
                -v isp_gomodcache:/go/pkg/mod -v isp_gobuildcache:/root/.cache/go-build \
                -e GOFLAGS=-mod=mod golang:1.22 \
                go run ./cmd/radload -addr "bss_aaa_core_daemon:1812" \
                -secret "${RADIUS_SECRET:-testing123}" \
                -users /src/.failover_users.csv -rate 5 -duration 1s >/dev/null 2>&1; then
            RESUMED_MS=$(( $(date +%s%3N) - KILL_MS ))
            break
        fi
    done

    if [ "$RESUMED_MS" -eq 0 ]; then
        fail "NFR-AVAIL-001: RADIUS auth never resumed within 60s of the kill"
    elif [ "$RESUMED_MS" -le "$AUTH_RESUME_BUDGET_MS" ]; then
        pass "NFR-AVAIL-001: RADIUS auth resumed ${RESUMED_MS}ms after the kill (budget ${AUTH_RESUME_BUDGET_MS}ms)"
    else
        fail "NFR-AVAIL-001: RADIUS auth resumed after ${RESUMED_MS}ms, over the ${AUTH_RESUME_BUDGET_MS}ms budget"
    fi

    wait "$LOAD_PID" 2>/dev/null
    LOAD_PID=""
    if [ -f "$SCRATCH/radload.out" ]; then
        info "load generator summary across the failover:"
        grep -E 'requests|error rate|latency p99' "$SCRATCH/radload.out" | sed 's/^/  /'
    fi
fi

rm -f "$REPO_ROOT/.failover_users.csv" 2>/dev/null

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
printf "Measured with down-after-milliseconds=%sms. The committed config uses 3000ms,\n" "$DOWN_AFTER_MS"
printf "which alone exceeds the %sms election budget — see this script's header.\n" "$ELECTION_BUDGET_MS"

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}SENTINEL FAILOVER TEST PASS${NC}\n"
    exit 0
fi
printf "${RED}SENTINEL FAILOVER TEST FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
