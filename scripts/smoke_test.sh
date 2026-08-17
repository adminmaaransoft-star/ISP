#!/usr/bin/env bash
# smoke_test.sh — prove the wired services actually boot and serve traffic.
#
# Builds both container images from the real Dockerfiles, brings up PostgreSQL
# and Redis, migrates, seeds, and then exercises the running system end to end:
# an API health and readiness probe, an authenticated API request, a portal
# login, and a real RADIUS Access-Request answered by the daemon.
#
# Usage: ./scripts/smoke_test.sh
#        KEEP_UP=1 ./scripts/smoke_test.sh   # leave the stack running
#
# Exit 0 = the stack is wired correctly.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MOUNT_ROOT="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

PG_IMAGE="${PG_IMAGE:-postgres:15-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
GO_IMAGE="${GO_IMAGE:-golang:1.22}"

SUFFIX="$$"
# Per-run key store. The containers mount config/keys as a directory, so the
# file has to live there — but it must not be the shared aes_keys.json a demo
# stack is using. .gitignore already covers config/keys/*.json.
KEY_NAME="aes_keys.smoke-${SUFFIX}.json"
KEY_PATH="${REPO_ROOT}/config/keys/${KEY_NAME}"
NETWORK="smoke_net_${SUFFIX}"
PG="smoke_pg_${SUFFIX}"
REDIS="smoke_redis_${SUFFIX}"
API="smoke_api_${SUFFIX}"
RADIUSD="smoke_radiusd_${SUFFIX}"
API_IMAGE="isp-bss-api:smoke"
RADIUSD_IMAGE="isp-bss-radiusd:smoke"

# Test-only secrets. Both are >= 32 chars because config.Load enforces that.
JWT_SECRET="smoke_test_jwt_secret_32_chars_min!!"
RADIUS_SECRET="smoke_test_radius_secret_32_chars!!"
TEST_PASSWORD="TestPass1234!"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }

FAILURES=0

cleanup() {
    if [ "${KEEP_UP:-0}" = "1" ]; then
        info "KEEP_UP=1 — leaving the stack running on network $NETWORK"
        return
    fi
    docker rm -f "$API" "$RADIUSD" "$PG" "$REDIS" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
    # Only this run's own key file — see the note in run_nfr_tests.sh: writing
    # or deleting the shared config/keys/aes_keys.json breaks a running demo
    # stack, fatally for the API.
    rm -f "$KEY_PATH" 2>/dev/null || true
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

info "creating network and starting PostgreSQL + Redis"
docker network create "$NETWORK" >/dev/null
docker run -d --rm --name "$PG" --network "$NETWORK" \
    -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=isp_bss_oss "$PG_IMAGE" >/dev/null
docker run -d --rm --name "$REDIS" --network "$NETWORK" "$REDIS_IMAGE" >/dev/null

for _ in $(seq 1 120); do
    docker exec "$PG" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$PG" pg_isready -U postgres -d isp_bss_oss >/dev/null 2>&1 || {
    fail "PostgreSQL never became ready"; docker logs "$PG" 2>&1 | tail -20; exit 1;
}
pass "PostgreSQL and Redis up"

DSN="postgres://postgres:postgres@${PG}:5432/isp_bss_oss?sslmode=disable"

info "applying migrations"
if ! MIG=$(in_go_container "$GO_IMAGE" go run github.com/pressly/goose/v3/cmd/goose@v3.19.2 \
        -dir ./migrations postgres "$DSN" up 2>&1); then
    fail "migrations failed"; echo "$MIG" | tail -20; exit 1
fi
pass "migrations applied"

# ── Key store ────────────────────────────────────────────────────────────────

info "generating a local AES key store"
mkdir -p "$REPO_ROOT/config/keys"
KEY_B64=$(docker run --rm "$GO_IMAGE" sh -c "head -c 32 /dev/urandom | base64 -w0")
cat > "$KEY_PATH" <<EOF
{"active_version":"v1","keys":{"v1":"${KEY_B64}"}}
EOF
pass "key store written"

# ── Seed ─────────────────────────────────────────────────────────────────────

info "seeding a franchise, plan and subscriber"
BCRYPT_HASH=$(in_go_container "$GO_IMAGE" go run ./scripts/gen_bcrypt "$TEST_PASSWORD" 2>/dev/null | tr -d '\r\n')
if [ -z "$BCRYPT_HASH" ]; then
    fail "could not generate a bcrypt hash"
    exit 1
fi

psql_q "INSERT INTO franchises (id,name,owner_name,mobile_number,commission_rate_pct,status)
        VALUES (1,'Smoke LCO','Owner','+919000000000',10.00,'active');" >/dev/null
psql_q "INSERT INTO plans (id,name,rate_limit_string,volume_gb,fup_threshold_bytes,fup_throttle_string,price,validity_days)
        VALUES (1,'TN_Super_100M','100M/100M',3300,3543348019200,'10M/10M',799.00,30);" >/dev/null
psql_q "INSERT INTO subscribers (id,caf_number,username,password_hash,mobile_number,plan_id,franchise_id,
                                 status,dunning_state,wallet_balance,registered_state,plan_expiry)
        VALUES (1,'CAF-SMOKE-1','smoke@isp','${BCRYPT_HASH}','+919876543210',1,1,
                'active','active',799.00,'TN',NOW() + INTERVAL '30 days');" >/dev/null
pass "seed data inserted"

# ── Build images ─────────────────────────────────────────────────────────────

info "building API image from Dockerfile.api"
if ! BUILD=$(docker build -q -f "${MOUNT_ROOT}/Dockerfile.api" -t "$API_IMAGE" "$MOUNT_ROOT" 2>&1); then
    fail "API image build failed"; echo "$BUILD" | tail -25; exit 1
fi
pass "API image built"

info "building RADIUS daemon image from Dockerfile"
if ! BUILD=$(docker build -q -f "${MOUNT_ROOT}/Dockerfile" -t "$RADIUSD_IMAGE" "$MOUNT_ROOT" 2>&1); then
    fail "RADIUS image build failed"; echo "$BUILD" | tail -25; exit 1
fi
pass "RADIUS daemon image built"

# ── Run the services ─────────────────────────────────────────────────────────

info "starting the API service"
docker run -d --rm --name "$API" --network "$NETWORK" \
    -v "${MOUNT_ROOT}/config/keys:/app/config/keys:ro" \
    -e "DB_DSN=${DSN}" \
    -e "REDIS_ADDR=${REDIS}:6379" \
    -e "JWT_SECRET=${JWT_SECRET}" \
    -e "AES_KEY_STORE_URL=local:/app/config/keys/${KEY_NAME}" \
    -e "LOG_FORMAT=json" \
    -p 18080:8080 \
    "$API_IMAGE" >/dev/null

info "starting the RADIUS daemon"
docker run -d --rm --name "$RADIUSD" --network "$NETWORK" \
    -e "DB_DSN=${DSN}" \
    -e "REDIS_ADDR=${REDIS}:6379" \
    -e "RADIUS_SECRET=${RADIUS_SECRET}" \
    -e "LOG_FORMAT=json" \
    "$RADIUSD_IMAGE" >/dev/null

# Wait for the API to report ready rather than sleeping a fixed amount.
READY=0
for _ in $(seq 1 60); do
    if docker exec "$API" wget -q -O- http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
        READY=1; break
    fi
    if ! docker ps --format '{{.Names}}' | grep -qx "$API"; then
        fail "API container exited during startup"
        docker logs "$API" 2>&1 | tail -30
        exit 1
    fi
    sleep 1
done
if [ "$READY" = "1" ]; then
    pass "API reports ready (PostgreSQL + Redis reachable)"
else
    fail "API never became ready"
    docker logs "$API" 2>&1 | tail -30
    exit 1
fi

if docker ps --format '{{.Names}}' | grep -qx "$RADIUSD"; then
    pass "RADIUS daemon running"
else
    fail "RADIUS daemon exited during startup"
    docker logs "$RADIUSD" 2>&1 | tail -30
fi

# ── Exercise the API ─────────────────────────────────────────────────────────

info "probing the API"
HEALTH=$(docker exec "$API" wget -q -O- http://127.0.0.1:8080/health 2>&1)
if echo "$HEALTH" | grep -q '"status":"ok"'; then
    pass "GET /health returns ok"
else
    fail "GET /health unexpected: $HEALTH"
fi

UNAUTH_CODE=$(docker exec "$API" sh -c \
    'wget -q -S -O /dev/null http://127.0.0.1:8080/api/v1/subscribers/1 2>&1 | grep HTTP/ | tail -1' || true)
if echo "$UNAUTH_CODE" | grep -q "401"; then
    pass "GET /api/v1/subscribers/1 without a token returns 401"
else
    fail "unauthenticated request should be 401, got: $UNAUTH_CODE"
fi

info "issuing an admin JWT and reading a subscriber"
TOKEN=$(in_go_container -e "JWT_SECRET=${JWT_SECRET}" "$GO_IMAGE" \
    go run ./scripts/gen_jwt -role billing_admin -secret "${JWT_SECRET}" 2>/dev/null | tr -d '\r\n')
if [ -z "$TOKEN" ]; then
    fail "could not mint a JWT"
else
    SUB_JSON=$(docker exec "$API" sh -c \
        "wget -q -O- --header='Authorization: Bearer ${TOKEN}' http://127.0.0.1:8080/api/v1/subscribers/1" 2>&1 || true)
    if echo "$SUB_JSON" | grep -q '"username":"smoke@isp"'; then
        pass "authenticated GET returned the seeded subscriber from PostgreSQL"
    else
        fail "authenticated GET unexpected: $SUB_JSON"
    fi
    if echo "$SUB_JSON" | grep -q '"wallet_balance":"799.00"'; then
        pass "wallet balance serialised exactly as 799.00"
    else
        fail "wallet balance not exact in: $SUB_JSON"
    fi
fi

info "exercising newly-wired admin endpoints against the real containers"
if [ -n "$TOKEN" ]; then
    LEDGER_JSON=$(docker exec "$API" sh -c \
        "wget -q -O- --header='Authorization: Bearer ${TOKEN}' http://127.0.0.1:8080/api/v1/wallets/1/ledger" 2>&1 || true)
    # No recharge has happened in this smoke run, so an empty array is the
    # correct answer — the point is that the route, auth and store are wired,
    # not that there is ledger history yet.
    if [ "$LEDGER_JSON" = "[]" ]; then
        pass "GET /api/v1/wallets/1/ledger returns the (empty) ledger from PostgreSQL"
    else
        fail "wallet ledger endpoint unexpected: $LEDGER_JSON"
    fi

    TICKET_JSON=$(docker exec "$API" sh -c \
        "wget -q -O- --header='Authorization: Bearer ${TOKEN}' --header='Content-Type: application/json' \
         --post-data='{\"subscriber_id\":1,\"category\":\"connectivity\",\"description\":\"smoke test ticket\"}' \
         http://127.0.0.1:8080/api/v1/tickets" 2>&1 || true)
    if echo "$TICKET_JSON" | grep -q '"status":"open"'; then
        pass "POST /api/v1/tickets created a real row (admin ticket store wired)"
    else
        fail "admin ticket creation unexpected: $TICKET_JSON"
    fi
fi

info "logging in to the subscriber portal"
PORTAL_LOGIN=$(docker exec "$API" sh -c \
    "wget -q -O- --post-data='{\"username\":\"smoke@isp\",\"password\":\"${TEST_PASSWORD}\"}' \
     --header='Content-Type: application/json' http://127.0.0.1:8080/portal/login" 2>&1 || true)
if echo "$PORTAL_LOGIN" | grep -q '"token"'; then
    pass "portal login succeeded against the real bcrypt hash"
else
    fail "portal login unexpected: $PORTAL_LOGIN"
fi

# ── Exercise RADIUS ──────────────────────────────────────────────────────────

info "sending a real RADIUS Access-Request"
AUTH_OUT=$(in_go_container "$GO_IMAGE" go run ./cmd/radload \
    -addr "${RADIUSD}:1812" -secret "$RADIUS_SECRET" \
    -username "smoke@isp" -password "$TEST_PASSWORD" 2>&1)
if echo "$AUTH_OUT" | grep -q "Access-Accept"; then
    pass "RADIUS returned Access-Accept for the seeded subscriber"
else
    fail "RADIUS auth unexpected: $AUTH_OUT"
    docker logs "$RADIUSD" 2>&1 | tail -20
fi

REJECT_OUT=$(in_go_container "$GO_IMAGE" go run ./cmd/radload \
    -addr "${RADIUSD}:1812" -secret "$RADIUS_SECRET" \
    -username "smoke@isp" -password "WrongPassword" 2>&1 || true)
if echo "$REJECT_OUT" | grep -q "Access-Reject"; then
    pass "RADIUS returned Access-Reject for a wrong password"
else
    fail "RADIUS reject unexpected: $REJECT_OUT"
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}SMOKE TEST PASS${NC} — both services boot from their Dockerfiles and serve real traffic\n"
    exit 0
fi
printf "${RED}SMOKE TEST FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
