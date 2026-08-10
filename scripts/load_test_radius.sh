#!/usr/bin/env bash
# scripts/load_test_radius.sh
# AAA-005: RADIUS load test — NFR-PERF-001 target: p99 auth latency <= 15 ms
#
# This script used to shell out to `radperf`, a third-party binary that is not
# vendored, not installed by any setup step here, and not pinned to a version
# — so it failed at the `command -v radperf` check on every machine and never
# actually measured NFR-PERF-001. It now drives cmd/radload, this repository's
# own RADIUS load generator, which needs no external install and is the tool
# scripts/run_nfr_tests.sh already uses for the same requirement.
#
# radload is also a better fit for what NFR-PERF-001 is really about: it can
# authenticate a whole CSV of distinct subscribers (-users) rather than one
# hardcoded account. That distinction matters, because a single account
# repeated is served almost entirely out of the fast-verifier cache after its
# first request and would report a latency the real workload never sees.
#
# Usage — against an already-running RADIUS daemon:
#   RADIUS_HOST=127.0.0.1 RADIUS_SECRET=testing123 bash scripts/load_test_radius.sh
#
# With a seeded subscriber CSV (strongly preferred — see above):
#   USERS_CSV=.nfr_users.csv bash scripts/load_test_radius.sh
#
# For the full NFR-PERF-001 run — stack, seeding, capacity sweep and
# daemon-side metrics all included — use scripts/run_nfr_tests.sh instead.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

RADIUS_HOST="${RADIUS_HOST:-127.0.0.1}"
RADIUS_PORT="${RADIUS_PORT:-1812}"
RADIUS_SECRET="${RADIUS_SECRET:-testing123}"
CONCURRENT="${CONCURRENT:-128}"   # matches internal/radius daemon worker pool
RATE="${RATE:-2000}"
DURATION="${DURATION:-30s}"
P99_THRESHOLD_MS="${P99_THRESHOLD_MS:-15}"
USERS_CSV="${USERS_CSV:-}"

ARGS=(
  -addr "${RADIUS_HOST}:${RADIUS_PORT}"
  -secret "${RADIUS_SECRET}"
  -rate "${RATE}"
  -duration "${DURATION}"
  -concurrency "${CONCURRENT}"
  -p99 "${P99_THRESHOLD_MS}ms"
)

if [ -n "$USERS_CSV" ]; then
  ARGS+=(-users "$USERS_CSV")
else
  echo "note: USERS_CSV unset — falling back to a single account, which the"
  echo "      fast-verifier cache will serve after the first request. Seed a"
  echo "      CSV (scripts/seed_load -users-out) for a representative p99."
  echo ""
  ARGS+=(-username "${RADIUS_USER:-loadtest01}" -password "${RADIUS_PASSWORD:-TestPass1234!}")
fi

cd "$REPO_ROOT"
exec go run ./cmd/radload "${ARGS[@]}"
