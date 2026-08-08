#!/usr/bin/env bash
# INT-PII-001 — pre-commit hook blocks PII in logs
#
# NFR-SEC-002 | ENV-008 | TST §13.5
#
# The hook in .githooks/pre-commit is the last line of defence against plaintext
# Aadhaar, PAN, mobile numbers or passwords reaching a log statement. This check
# proves it both blocks the violations it is meant to catch and stays quiet on
# the encrypted/hashed forms the codebase legitimately uses — a hook that fires
# on everything gets bypassed with --no-verify and protects nothing.
#
# Runs in a throwaway git repository, so the working tree is never touched.
#
# Usage: ./scripts/int_pii_001_precommit_hook.sh
# Exit 0 = hook behaves correctly. Exit 1 = hook missing, too loose, or too noisy.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK_SOURCE="$REPO_ROOT/.githooks/pre-commit"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }

FAILURES=0
SANDBOX="$(mktemp -d)"
cleanup() { rm -rf "$SANDBOX"; }
trap cleanup EXIT

if [ ! -f "$HOOK_SOURCE" ]; then
    fail "hook not found at .githooks/pre-commit"
    exit 1
fi

info "building sandbox repository"
git init -q "$SANDBOX"
cd "$SANDBOX"
git config user.email "int-pii-001@test.local"
git config user.name "INT-PII-001"
mkdir -p .git/hooks
cp "$HOOK_SOURCE" .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit

# A commit must already exist, otherwise --cached diffs behave differently.
echo "sandbox" > README.md
git add README.md
git commit -qm "base" --no-verify

# run_case <name> <expected exit: block|allow> <filename> <file content>
run_case() {
    local name="$1" expect="$2" filename="$3" content="$4"

    printf '%s\n' "$content" > "$filename"
    git add "$filename"

    local output exit_code
    output="$(.git/hooks/pre-commit 2>&1)"
    exit_code=$?

    git reset -q HEAD "$filename" >/dev/null 2>&1
    rm -f "$filename"

    if [ "$expect" = "block" ]; then
        if [ "$exit_code" -eq 1 ]; then
            pass "blocked: $name"
        else
            fail "NOT blocked (exit $exit_code): $name"
            FAILURES=$((FAILURES + 1))
        fi
        if ! echo "$output" | grep -qi "BLOCKED\|PII"; then
            fail "no explanatory message printed for: $name"
            FAILURES=$((FAILURES + 1))
        fi
    else
        if [ "$exit_code" -eq 0 ]; then
            pass "allowed: $name"
        else
            fail "wrongly blocked (exit $exit_code): $name"
            echo "  hook said: $output"
            FAILURES=$((FAILURES + 1))
        fi
    fi
}

echo ""
info "cases that must be blocked"

run_case "zerolog logging raw aadhaar" block "leak_aadhaar.go" \
'package main
import "github.com/rs/zerolog/log"
func leak(aadhaar string) {
	log.Info().Str("aadhaar", aadhaar).Msg("kyc received")
}'

run_case "zerolog logging pan_number" block "leak_pan.go" \
'package main
import "github.com/rs/zerolog/log"
func leak(pan string) {
	log.Warn().Str("pan_number", pan).Msg("kyc mismatch")
}'

run_case "zerolog logging mobile_number" block "leak_mobile.go" \
'package main
import "github.com/rs/zerolog/log"
func leak(m string) {
	log.Info().Str("mobile_number", m).Msg("otp sent")
}'

run_case "fmt.Printf leaking password" block "leak_password.go" \
'package main
import "fmt"
func leak(password string) {
	fmt.Printf("auth attempt password=%s\n", password)
}'

echo ""
info "cases that must be allowed"

run_case "encrypted aadhaar field" allow "ok_encrypted.go" \
'package main
import "github.com/rs/zerolog/log"
func ok(aadhaarEncrypted string) {
	log.Info().Str("aadhaar_encrypted", aadhaarEncrypted).Msg("kyc stored")
}'

run_case "hashed password field" allow "ok_hash.go" \
'package main
import "github.com/rs/zerolog/log"
func ok(hash string) {
	log.Debug().Str("password_hash", hash).Msg("credential rotated")
}'

run_case "aadhaar named but never logged" allow "ok_no_log.go" \
'package main
func store(aadhaar string) string {
	return encrypt(aadhaar)
}
func encrypt(s string) string { return s }'

run_case "non-Go file mentioning aadhaar in a log line" allow "notes.md" \
'# Notes
Never write log.Info().Str("aadhaar", raw) — use the encrypted column.'

echo ""
if [ "$FAILURES" -eq 0 ]; then
    printf "${GREEN}INT-PII-001 PASS${NC} — hook blocks plaintext PII in logs and permits encrypted/hashed fields\n"
    exit 0
fi
printf "${RED}INT-PII-001 FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
