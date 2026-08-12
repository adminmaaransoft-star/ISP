#!/usr/bin/env bash
# check_wiring.sh — every long-running component must be reachable from a
# binary, not just from tests.
#
# This exists because three components shipped complete, correct and fully
# tested with no caller anywhere outside tests:
#
#   TransitionDunning   nobody was reminded to pay, nobody was ever suspended
#   ReconcileJob        no revenue snapshot, no ledger variance, ever
#   TMPL-002..007       templates registered, nothing dispatched them
#
# None of that failed a test, because nothing was wrong — there was simply
# nothing there to fail. The suite was green, coverage was reported, and the
# features did not run. A checklist item that reads "is it tested" cannot
# catch this; only asking "is it called" can.
#
# Usage:
#   ./scripts/check_wiring.sh          # report and exit non-zero on findings
#   ./scripts/check_wiring.sh --list   # list what is checked, then report
#
# Exit 0 = every tracked component has a production caller.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
FAILURES=0

pass() { printf "${GREEN}[WIRED]${NC}  %s\n" "$1"; }
fail() { printf "${RED}[UNWIRED]${NC} %s\n" "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf "${YELLOW}[....]${NC}  %s\n" "$1"; }

# Constructors and entry points that only make sense if something runs them.
# A component belongs here when "nobody calls it" is a silent production
# outage rather than dead code a linter would flag.
COMPONENTS=(
    "NewScanner:FUP scanner (throttles subscribers over quota)"
    "NewDeadLetterMonitor:dead-letter monitor (surfaces stuck tasks)"
    "NewDunningScanner:dunning scanner (reminders and suspensions)"
    "NewReconcileScheduler:nightly revenue reconciliation"
    "NewDunningNoticeHandler:dunning notification worker"
    "NewPaymentReceiptHandler:payment receipt worker"
    "NewWarningHandler:80% FUP warning worker"
    "NewUpdateHandler:ticket status-change notification worker"
    "NewCoAHandler:CoA worker (applies rate limits)"
    "NewPoDHandler:PoD worker (disconnects sessions)"
    "NewVerifierCache:RADIUS fast-verifier cache"
    "NewSubscriberCache:RADIUS subscriber cache"
    "NewResolver:multi-vendor NAS attribute/secret resolver"
    "NewSLAScanner:SLA breach scanner (helpdesk deadlines)"
)

if [ "${1:-}" = "--list" ]; then
    printf "${BOLD}Components checked for a production caller:${NC}\n"
    for entry in "${COMPONENTS[@]}"; do
        printf "  %-26s %s\n" "${entry%%:*}" "${entry#*:}"
    done
    echo ""
fi

printf "${BOLD}== Wiring check ==${NC}\n"

for entry in "${COMPONENTS[@]}"; do
    symbol="${entry%%:*}"
    description="${entry#*:}"

    # A caller counts only if it is production code: not a _test.go file, not
    # the declaration itself, and not a doc comment mentioning the name.
    callers=$(grep -rn --include="*.go" "\b${symbol}(" cmd/ internal/ 2>/dev/null \
        | grep -v "_test\.go:" \
        | grep -vE ":[0-9]+:\s*//" \
        | grep -vE "func ${symbol}\(" \
        | wc -l)

    if [ "$callers" -gt 0 ]; then
        pass "$(printf '%-26s %s' "$symbol" "$description")"
    else
        fail "$(printf '%-26s %s' "$symbol" "$description")"
        # Show where it IS referenced, which is usually tests only — that is
        # the shape of the bug and makes the report actionable rather than
        # merely accusatory.
        grep -rn --include="*.go" "\b${symbol}(" cmd/ internal/ 2>/dev/null \
            | grep -vE "func ${symbol}\(" | head -3 | sed 's/^/           /'
    fi
done

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}WIRING OK${NC} — every tracked component has a caller outside tests\n"
    exit 0
fi
printf "${RED}WIRING FAIL${NC} — %d component(s) are built and tested but never run\n" "$FAILURES"
printf "Add the caller, or delete the component. A third option — leaving it\n"
printf "wired to nothing while its tests pass — is what this check exists to stop.\n"
exit 1
