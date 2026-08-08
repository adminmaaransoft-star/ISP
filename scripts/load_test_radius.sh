#!/usr/bin/env bash
# scripts/load_test_radius.sh
# AAA-005: RADIUS load test — NFR-PERF-001 target: p99 auth latency ≤ 15 ms
# Requires: radperf (https://github.com/nicholasgasior/radperf) or equivalent
#           A running RADIUS server on ${RADIUS_HOST}:1812
#
# Usage:
#   export RADIUS_HOST=127.0.0.1
#   export RADIUS_SECRET=testing123
#   bash scripts/load_test_radius.sh
set -euo pipefail

RADIUS_HOST="${RADIUS_HOST:-127.0.0.1}"
RADIUS_PORT="${RADIUS_PORT:-1812}"
RADIUS_SECRET="${RADIUS_SECRET:-testing123}"
CONCURRENT="${CONCURRENT:-128}"   # matches internal/radius daemon worker pool
TOTAL_REQUESTS="${TOTAL_REQUESTS:-10000}"
P99_THRESHOLD_MS="${P99_THRESHOLD_MS:-15}"

echo "=== RADIUS AAA Load Test (NFR-PERF-001) ==="
echo "Target   : ${RADIUS_HOST}:${RADIUS_PORT}"
echo "Workers  : ${CONCURRENT}"
echo "Requests : ${TOTAL_REQUESTS}"
echo "P99 limit: ${P99_THRESHOLD_MS} ms"
echo ""

if ! command -v radperf &>/dev/null; then
  echo "ERROR: radperf not found. Install from https://github.com/nicholasgasior/radperf"
  echo "       or use: go install github.com/nicholasgasior/radperf/cmd/radperf@latest"
  exit 1
fi

RESULT_FILE=$(mktemp /tmp/radperf_result_XXXXXX.json)

radperf \
  --host "${RADIUS_HOST}" \
  --port "${RADIUS_PORT}" \
  --secret "${RADIUS_SECRET}" \
  --concurrency "${CONCURRENT}" \
  --requests "${TOTAL_REQUESTS}" \
  --username "loadtest01" \
  --password "TestPass1234!" \
  --output json \
  > "${RESULT_FILE}"

P99=$(python3 -c "
import json, sys
d = json.load(open('${RESULT_FILE}'))
print(d.get('p99_ms', d.get('percentile_99', 'N/A')))
")

echo "P99 latency: ${P99} ms"

if python3 -c "
import sys
p99 = float('${P99}')
limit = float('${P99_THRESHOLD_MS}')
sys.exit(0 if p99 <= limit else 1)
"; then
  echo "PASS — P99 (${P99} ms) is within ${P99_THRESHOLD_MS} ms threshold"
else
  echo "FAIL — P99 (${P99} ms) exceeds ${P99_THRESHOLD_MS} ms threshold (NFR-PERF-001)"
  cat "${RESULT_FILE}"
  exit 1
fi

rm -f "${RESULT_FILE}"
