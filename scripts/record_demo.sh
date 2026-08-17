#!/usr/bin/env bash
# record_demo.sh — record narrated walkthrough videos, one per persona.
#
# Produces a .webm per persona in e2e/demo-videos/, captioned and deliberately
# slowed so a viewer can follow what is happening. Intended for demos and
# onboarding, not for CI.
#
# Requires a stack already up:
#   ./scripts/demo_up.sh
#   ./scripts/record_demo.sh
#
# Pacing (all milliseconds, all optional):
#   DEMO_SLOWMO=450     delay between every browser action
#   DEMO_READ_MS=2200   how long an ordinary caption stays up
#   DEMO_BEAT_MS=3400   pause on a frame worth dwelling on
#
# Slower for a live audience:
#   DEMO_SLOWMO=700 DEMO_READ_MS=3200 ./scripts/record_demo.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RAW="e2e/demo-recordings"
OUT="e2e/demo-videos"

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
pass() { printf "${GREEN}[ ok ]${NC} %s\n" "$1"; }

# The portal must be reachable, or every recording is 40 seconds of error page.
if ! curl -sk -o /dev/null --max-time 5 "https://localhost/health"; then
    printf "${RED}the stack is not answering on https://localhost${NC}\n"
    printf "start it first:  ./scripts/demo_up.sh\n"
    exit 1
fi

info "recording — this takes a few minutes by design"
rm -rf "$RAW" "$OUT"
npx playwright test --config playwright.demo.config.ts || {
    printf "${RED}recording failed — see the output above${NC}\n"
    exit 1
}

# Playwright names the directory after the test title; rename to something a
# person would pick out of a folder.
info "collecting"
mkdir -p "$OUT"
n=0
while IFS= read -r vid; do
    dir="$(basename "$(dirname "$vid")")"
    case "$dir" in
        # Matched on the spec-file prefix only. Playwright builds this folder
        # name from the spec file *and* the test title, then truncates the
        # middle with a hash when it runs long — so anything from the title
        # onwards is unreliable. Each spec here holds exactly one test, which
        # makes the leading number sufficient and stable.
        01-*) name="01-isp-owner" ;;
        02-*) name="02-noc-engineer" ;;
        03-*) name="03-billing-admin" ;;
        04-*) name="04-csr-vs-technician" ;;
        05-*) name="05-subscriber-portal" ;;
        06-*) name="06-subscriber-suspended" ;;
        07-*) name="07-captive-portal" ;;
        *)    name="$dir" ;;
    esac
    cp "$vid" "$OUT/${name}.webm"
    sz=$(( $(stat -c%s "$OUT/${name}.webm") / 1024 ))
    printf "       %-28s %5s KB\n" "${name}.webm" "$sz"
    n=$((n + 1))
done < <(find "$RAW" -name "*.webm" | sort)

echo ""
if [ "$n" -eq 0 ]; then
    printf "${RED}no videos were produced${NC}\n"
    exit 1
fi
pass "$n videos in ${OUT}/"
printf "${BOLD}Play them in any browser — .webm needs no player install.${NC}\n"
