#!/usr/bin/env bash
# verify_tls.sh — NFR-SEC-001 / DoD L6-006: TLS 1.3 minimum on the external edge.
#
# config/caddy/Caddyfile pins `protocols tls1.3`, but a pinned config line is
# a claim, not evidence — Caddy's own default would happily also accept TLS
# 1.2, so a typo or a reverted block would silently downgrade the edge with
# nothing failing. This script stands the real Caddy edge up and negotiates
# against it with a real TLS client, proving 1.3 connects and 1.2 and below
# are refused by the server.
#
# The upstream (api_service) is deliberately absent: a TLS handshake completes
# before Caddy ever proxies anything, so what is under test here is the edge's
# TLS parameters, not the application. A 502 after a successful handshake is
# the expected and correct result.
#
# Usage: ./scripts/verify_tls.sh
#
# Runs standalone (it is not part of run_nfr_tests.sh) because it needs only
# the edge container, not the seeded database and load-test stack.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MOUNT_ROOT="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

CADDY_IMAGE="${CADDY_IMAGE:-caddy:2-alpine}"
# The TLS client. golang:1.22 is used elsewhere in scripts/ and ships OpenSSL
# 3.x, so probing needs no image this repo does not already pull.
OPENSSL_IMAGE="${OPENSSL_IMAGE:-golang:1.22}"

SUFFIX="$$"
NETWORK="tls_net_${SUFFIX}"
EDGE="tls_edge_${SUFFIX}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'
pass() { printf "${GREEN}[PASS]${NC} %s\n" "$1"; RESULTS+=("PASS|$1"); }
fail() { printf "${RED}[FAIL]${NC} %s\n" "$1"; RESULTS+=("FAIL|$1"); FAILURES=$((FAILURES + 1)); }
skip() { printf "${YELLOW}[SKIP]${NC} %s\n" "$1"; RESULTS+=("SKIP|$1"); }
info() { printf "${YELLOW}[....]${NC} %s\n" "$1"; }
head1() { printf "\n${BOLD}== %s ==${NC}\n" "$1"; }

FAILURES=0
RESULTS=()

cleanup() {
    docker rm -f "$EDGE" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { echo "docker is required"; exit 1; }

# probe_tls VERSION -- negotiates against the edge forcing exactly one TLS
# version and echoes the client transcript. `-verify_return_error` is
# deliberately NOT set: with DOMAIN unset Caddy serves a certificate from its
# own local CA, which no container trusts, and an untrusted-chain rejection
# would mask the protocol-version result this script is actually measuring.
probe_tls() {
    docker run --rm --network "$NETWORK" "$OPENSSL_IMAGE" \
        sh -c "echo | openssl s_client -connect ${EDGE}:443 -servername localhost $1 2>&1" 2>&1
}

# negotiated_protocol reads a probe transcript and echoes the TLS version that
# was actually agreed, or nothing at all if the handshake failed.
#
# It must NOT read the "Protocol :" line of the SSL-Session block: OpenSSL
# prints that line echoing the version it *attempted* even when the server
# rejected it outright, so a refused TLS 1.2 attempt still reports
# "Protocol : TLSv1.2" there and would read as a successful downgrade. The
# honest signal is the "New, <version>, Cipher is <cipher>" line, which
# reports "New, (NONE), Cipher is (NONE)" on a failed handshake — a version
# is only real if a cipher was agreed alongside it.
negotiated_protocol() {
    echo "$1" | sed -nE 's/^New, (TLSv[0-9.]+), Cipher is (.+)$/\1 \2/p' \
        | grep -v '(NONE)' | awk '{print $1}' | head -1
}

negotiated_cipher() {
    echo "$1" | sed -nE 's/^New, TLSv[0-9.]+, Cipher is (.+)$/\1/p' \
        | grep -v '(NONE)' | head -1
}

head1 "Environment"
info "starting the Caddy edge with the repository Caddyfile"
docker network create "$NETWORK" >/dev/null

# The real config/caddy/Caddyfile, mounted read-only — testing a copy would
# prove nothing about what actually ships.
docker run -d --rm --name "$EDGE" --network "$NETWORK" \
    -v "${MOUNT_ROOT}/config/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" \
    "$CADDY_IMAGE" >/dev/null || { echo "could not start the edge container"; exit 1; }

READY=0
for _ in $(seq 1 30); do
    if probe_tls "-tls1_3" | grep -q "CONNECTED"; then READY=1; break; fi
    sleep 1
done
if [ "$READY" -ne 1 ]; then
    fail "the Caddy edge never accepted a TLS connection on :443"
    docker logs "$EDGE" 2>&1 | tail -20
else
    info "edge is listening on :443"
fi

# ── NFR-SEC-001 ──────────────────────────────────────────────────────────────

head1 "NFR-SEC-001 — TLS 1.3 minimum on the external edge"

if [ "$READY" -eq 1 ]; then
    # 1. TLS 1.3 must be accepted.
    OUT13=$(probe_tls "-tls1_3")
    if [ "$(negotiated_protocol "$OUT13")" = "TLSv1.3" ]; then
        pass "NFR-SEC-001: TLS 1.3 handshake succeeds (cipher $(negotiated_cipher "$OUT13"))"
    else
        fail "NFR-SEC-001: TLS 1.3 handshake did not complete"
        echo "$OUT13" | tail -15
    fi

    # 2. TLS 1.2 must be refused. This is the half that actually protects the
    #    requirement: a config that accepted both would still pass check 1.
    OUT12=$(probe_tls "-tls1_2")
    NEG12=$(negotiated_protocol "$OUT12")
    if [ -n "$NEG12" ]; then
        fail "NFR-SEC-001: ${NEG12} was NEGOTIATED — the edge accepts a version below the 1.3 floor"
    else
        pass "NFR-SEC-001: TLS 1.2 refused — no cipher agreed, handshake aborted"
    fi

    # 3. TLS 1.1 / 1.0. OpenSSL 3.x usually refuses these client-side at its
    #    default security level, which is a client refusal, not evidence about
    #    the server — so a client-side refusal is reported as SKIP rather than
    #    claimed as a server-side pass it does not demonstrate.
    for v in tls1_1 tls1; do
        OUTOLD=$(probe_tls "-$v")
        NEGOLD=$(negotiated_protocol "$OUTOLD")
        if [ -n "$NEGOLD" ]; then
            fail "NFR-SEC-001: ${NEGOLD} was NEGOTIATED via -${v} — the edge accepts a legacy TLS version"
        elif echo "$OUTOLD" | grep -qiE 'unsupported protocol|no protocols available|too small|no cipher match'; then
            skip "NFR-SEC-001: ${v} refused by the OpenSSL client before reaching the server"
        else
            pass "NFR-SEC-001: ${v} refused — no cipher agreed, handshake aborted"
        fi
    done

    # 4. Default negotiation — what a real client with no version pin gets.
    NEG=$(negotiated_protocol "$(probe_tls "")")
    if [ "$NEG" = "TLSv1.3" ]; then
        pass "NFR-SEC-001: an unpinned client negotiates TLSv1.3"
    else
        fail "NFR-SEC-001: an unpinned client negotiated ${NEG:-nothing}, expected TLSv1.3"
    fi
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
    printf "${GREEN}TLS VERIFICATION PASS${NC}\n"
    exit 0
fi
printf "${RED}TLS VERIFICATION FAIL${NC} — %d check(s) failed\n" "$FAILURES"
exit 1
