# Release Notes

## v1.0.0 — 2026-08-10

First production release of the BSS/OSS platform for ISP subscriber management:
RADIUS AAA, GST-compliant billing, FUP enforcement, notifications, and a
subscriber self-service portal.

Go 1.22 · PostgreSQL 15 · Redis 7 (Sentinel) · 20 migrations · 13 internal modules

---

### Subscriber Portal UI

Server-rendered self-service portal at `/ui`, 14 routes:

| Area | Capability |
|---|---|
| Auth | Login / logout, session cookies, CSRF-protected forms |
| Dashboard | Account status, wallet balance, and live session usage against quota |
| Usage | Per-session history with volumes and durations |
| Invoices | Invoice history and PDF download (Gotenberg) |
| Renewal | Plan renewal via Razorpay, idempotent on `transaction_token` |
| Support | Ticket raising and notification history |

### LEA audit log — tamper protection made real

Migration `014` enabled Row-Level Security on `lea_audit_log` with an
INSERT-only policy, intended to make law-enforcement lookup records
append-only. It did not work: the application connected as `postgres`, which
is both the table owner and a superuser, and **PostgreSQL exempts both from
RLS regardless of `FORCE ROW LEVEL SECURITY`**. A live `UPDATE` successfully
tampered a record during audit.

Migration `019` adds a least-privilege `bss_app` role — neither superuser nor
owner — so the policy now actually applies. The same `UPDATE` is rejected.

### RADIUS fast-verifier cache

DDS §5.1 mandates bcrypt cost 12. Measured, that is ~282ms per comparison —
roughly **19× the entire 15ms p99 budget** for a single authentication. Cost 12
and NFR-PERF-001 could not both hold.

Rather than lower the cost, `internal/radius/verifiercache.go` caches
`HMAC-SHA256(secret, password ‖ passwordHash)` in Redis after the first
successful bcrypt, letting repeat authentications skip it. The cache stores
neither the password nor the hash, and binding the verifier to the subscriber's
*current* password hash means a password change invalidates it immediately
rather than after a TTL. A wrong guess cannot forge a match without the
server-side secret, so incorrect passwords still fall through to full bcrypt.

**Measured: p99 10.4ms at 5,000 req/s** across 149,850 requests, zero errors.

### Definition of Done — 52 of 65 passing (80%)

Up from 41 (63%) at the opening audit; failures down from 14 to 4.

| Result | v1.0.0 | Opening audit |
|---|---|---|
| PASS | **52 (80%)** | 41 (63%) |
| FAIL | **4 (6%)** | 14 (22%) |
| PARTIAL | 4 | 2 |
| NOT VERIFIED | **1** | 4 |
| PENDING (tracker admin) | 4 | 4 |

All four critical findings from the opening audit are closed. Full detail in
[`DOD_STATUS_REPORT.md`](DOD_STATUS_REPORT.md).

---

## Performance — measured, not assumed

| Requirement | Budget | Measured |
|---|---|---|
| RADIUS auth p99 @ 5,000 req/s | ≤ 15ms | **10.4ms** (149,850 req, 0 errors) |
| API p99 @ 500 concurrent | ≤ 200ms | **11.69ms** (0 failures) |
| Unbilled revenue query @ 20k subscribers | ≤ 60s | **9.716ms** |
| Notification dispatch latency | ≤ 5s | **1.011s** idle, **92ms** worst case under backlog |
| TLS floor | 1.3 min, 1.2 disabled | 1.3 negotiated; 1.2 / 1.1 / 1.0 refused |
| Plaintext PII in logs | zero | zero, source and runtime |

Reproduce with `./scripts/run_nfr_tests.sh` and `./scripts/verify_tls.sh`.

## Trying the portal

`./scripts/demo_up.sh` brings the whole stack up and seeds a demo account,
then the portal is at `https://localhost/ui/login` — sign in as `test_user`
with password `testpassword`. The browser will warn about the certificate,
because the demo issues its own rather than buying a public one.

The seed includes four past sessions and one live session, so the Dashboard
and Usage screens show real figures rather than empty states. Usage sits at
67% of quota — deliberately under the 80% mark that triggers a FUP warning,
so nothing fires a notification nobody asked for.

## Security

- Least-privilege `bss_app` Postgres role; LEA audit log genuinely append-only
- AES-GCM encryption of Aadhaar/PAN with key versioning and cross-rotation decrypt
- RADIUS verifier cache that stores neither password nor hash
- E.164 phone validation at API, both send paths, and a DB `CHECK` constraint
- HMAC signature validation before payload parsing on all webhooks (`hmac.Equal`, timing-safe)
- Brute-force lockout on the RADIUS auth path
- TLS 1.3 floor at the edge, verified against a deliberately weakened config
- Pre-commit hook blocking plaintext PII in log statements, verified against a real commit

## Known issues

**NFR-AVAIL-001 / L6-004 — Sentinel failover misses its budget.** Failover
works but is slow: **5055ms** on the shipped configuration, **3086–3192ms**
with `down-after-milliseconds` lowered to 500, against a 3000ms budget.
Recommendation is to adopt detection = 500 and move the target to 8s.

Separately, Sentinel monitors the master *by hostname* (`resolve-hostnames yes`,
which Redis 7.4+ requires for a hostname in `sentinel monitor`). If the master
container leaves Docker DNS entirely — a node failure under an orchestrator —
Sentinel logs `Failed to resolve hostname`, enters tilt mode and **never
promotes a replica**. Reproduce with
`CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh`. This is a separate
defect that changing the timing target does not address.

**NFR-SCAL-001 / L6-005 — 1-hour soak not run.** `scripts/run_soak_test.sh`
exists and is validated end to end on a 90-second run (9,000 requests, 0
errors, goroutines and file descriptors flat), but 90 seconds is not evidence
about an hour.

**Renewal payments cannot be completed.** The Renew screen reports "Payment
gateway is not configured" until `RAZORPAY_KEY_ID` and `RAZORPAY_KEY_SECRET`
are supplied. The screen and the form work; only the payment round trip is
unavailable. Testers should log this as *blocked*, not failed.

**There is no staff or admin web interface.** The portal is subscriber-facing
only; staff operations (creating subscribers, issuing invoices, running
reports) are available through the JSON API at `/api/v1/*` and have no screen.

**Test coverage 60.4% against an 80% target.** `pkg/crypto` (93.5%) and
`internal/middleware` (98.2%) meet their stricter 90% requirement. The
shortfall is concentrated in `cmd/api` (5.3%, mostly service wiring),
`internal/portal` (63.0%) and `internal/notifications` (71.7%).

**TDD commit-ordering checks (L0-001, L0-002) cannot be satisfied**
retroactively for code written before it was first committed.

## Verification

```
go build ./...                        clean
go vet -tags=integration ./...        clean
golangci-lint run ./...               0 issues
go test -race -tags=integration ./... 15 packages pass
```

Build and test suite verified from a fresh `git clone` at `edf86bd`.

## Operational scripts

| Script | Purpose |
|---|---|
| `scripts/demo_up.sh` | Bring up the full stack, generating any missing secrets |
| `scripts/run_nfr_tests.sh` | NFR-PERF-001/002, NFR-BIZ-001, NFR-SEC-002 |
| `scripts/verify_tls.sh` | NFR-SEC-001 TLS 1.3 floor |
| `scripts/run_sentinel_failover_test.sh` | NFR-AVAIL-001 failover timing |
| `scripts/run_soak_test.sh` | NFR-SCAL-001 goroutine/FD/heap soak |
| `scripts/run_db_tests.sh` | Persistence suite against a real PostgreSQL |
| `scripts/install-hooks.sh` | Install the pre-commit PII guard |
