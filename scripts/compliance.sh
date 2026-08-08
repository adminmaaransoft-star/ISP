#!/usr/bin/env bash
# compliance.sh — Automated DoD (Definition of Done) verification script
# Usage: ./scripts/compliance.sh [module]   e.g. ./scripts/compliance.sh internal/billing
# If no module is specified, checks the entire workspace.
#
# Session: ENV-009 | FR: all | DoD: ✅ Definition of Done sheet (9 levels L0–L8)
# Exit code: 0 = all checks pass, 1 = one or more checks failed

set -euo pipefail

MODULE="${1:-.}"
PASS=0
FAIL=0

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Colour

pass() { echo -e "${GREEN}[PASS]${NC} $1"; ((PASS++)); }
fail() { echo -e "${RED}[FAIL]${NC} $1"; ((FAIL++)); }
info() { echo -e "${YELLOW}[INFO]${NC} $1"; }

echo "================================================================"
echo " BSS/OSS Compliance Check — module: ${MODULE}"
echo "================================================================"

# ─── L0: Go build ────────────────────────────────────────────────────────────
info "L0: go build"
if go build ./"${MODULE}"/... 2>/dev/null; then
    pass "L0: go build ./\"${MODULE}\"/... succeeds"
else
    fail "L0: go build ./\"${MODULE}\"/... FAILED"
fi

# ─── L1: go vet ──────────────────────────────────────────────────────────────
info "L1: go vet"
if go vet ./"${MODULE}"/... 2>/dev/null; then
    pass "L1: go vet clean"
else
    fail "L1: go vet reported issues"
fi

# ─── L2: Unit tests (short — no infrastructure needed) ──────────────────────
info "L2: unit tests (-short)"
if go test -short -race ./"${MODULE}"/... 2>/dev/null; then
    pass "L2: unit tests pass (short mode, race detector on)"
else
    fail "L2: unit tests FAILED"
fi

# ─── L3: Coverage ≥ 80% for crypto, billing, radius ─────────────────────────
info "L3: coverage gate"
COVERAGE_DIRS=("pkg/crypto" "internal/billing" "internal/radius")
for dir in "${COVERAGE_DIRS[@]}"; do
    if [ -d "${dir}" ]; then
        # POSIX ERE, not grep -P: PCRE is absent in BusyBox/BSD grep and errors
        # out under a non-UTF-8 locale, which would report every module as 0%.
        COV=$(go test -short -coverprofile=/tmp/cov_check.out ./"${dir}"/... 2>/dev/null \
            | grep -oE '[0-9]+\.[0-9]+%' | tail -1 | tr -d '%')
        COV="${COV:-0}"
        COV_INT=${COV%%.*}
        if [ "${COV_INT:-0}" -ge 80 ]; then
            pass "L3: ${dir} coverage ${COV}% ≥ 80%"
        else
            fail "L3: ${dir} coverage ${COV}% < 80%"
        fi
    fi
done

# ─── L4: golangci-lint ───────────────────────────────────────────────────────
info "L4: golangci-lint"
if command -v golangci-lint &>/dev/null; then
    if golangci-lint run ./"${MODULE}"/... 2>/dev/null; then
        pass "L4: golangci-lint clean"
    else
        fail "L4: golangci-lint reported issues"
    fi
else
    echo -e "${YELLOW}[SKIP]${NC} L4: golangci-lint not installed (run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.55.2)"
fi

# ─── L5: PII plaintext scan ──────────────────────────────────────────────────
info "L5: PII plaintext scan"
# Must not log aadhaar/pan/mobile in plaintext — search for suspicious patterns
PII_HITS=$(grep -rn --include="*.go" \
    -E '(aadhaar|pan_number|mobile_number|password)[^_]*(=|:=|log\.|fmt\.)' \
    "${MODULE}" 2>/dev/null | grep -v "_encrypted\|_hash\|_test\|//.*" || true)
if [ -z "${PII_HITS}" ]; then
    pass "L5: no plaintext PII log patterns detected"
else
    fail "L5: potential plaintext PII found:"
    echo "${PII_HITS}" | head -20
fi

# ─── L6: Error wrapping — no bare errors.New / fmt.Errorf without %w ─────────
info "L6: error wrapping"
UNWRAPPED=$(grep -rn --include="*.go" \
    'fmt\.Errorf("[^"]*"[^,)]*)\|errors\.New(' \
    "${MODULE}" 2>/dev/null | grep -v "_test\|//" \
    | grep -v '%w' || true)
if [ -z "${UNWRAPPED}" ]; then
    pass "L6: all fmt.Errorf calls use %w wrapping"
else
    fail "L6: unwrapped errors found (use fmt.Errorf(\"...: %%w\", err)):"
    echo "${UNWRAPPED}" | head -10
fi

# ─── L7: No global state (package-level var mutables) ────────────────────────
info "L7: global state check"
GLOBALS=$(grep -rn --include="*.go" \
    -E '^var [A-Z][a-zA-Z]+ =' \
    "${MODULE}" 2>/dev/null | grep -v '_test\|metrics\|prometheus\|log\.' || true)
if [ -z "${GLOBALS}" ]; then
    pass "L7: no exported mutable package-level vars detected"
else
    fail "L7: exported package-level vars (potential global state):"
    echo "${GLOBALS}" | head -10
fi

# ─── L8: TIMESTAMPTZ usage in migrations ─────────────────────────────────────
info "L8: SQL TIMESTAMPTZ compliance"
if [ -d "migrations" ]; then
    BAD_TS=$(grep -rn --include="*.sql" 'TIMESTAMP[^Z]' migrations/ 2>/dev/null || true)
    if [ -z "${BAD_TS}" ]; then
        pass "L8: all migration timestamps use TIMESTAMPTZ"
    else
        fail "L8: TIMESTAMP without TZ found in migrations (must be TIMESTAMPTZ):"
        echo "${BAD_TS}" | head -10
    fi
fi

# ─── Summary ─────────────────────────────────────────────────────────────────
echo ""
echo "================================================================"
echo -e " Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo "================================================================"

if [ "${FAIL}" -gt 0 ]; then
    echo -e "${RED}Compliance check FAILED — do not mark session as Done${NC}"
    exit 1
else
    echo -e "${GREEN}All compliance checks PASSED — safe to mark session as Done${NC}"
    exit 0
fi
