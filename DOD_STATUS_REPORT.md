# Definition of Done — Status Report

**Original audit:** 2026-08-08
**Last revised:** 2026-08-10 (post-remediation, through commit `cb1d2bf`)
**Scope:** Whole-codebase audit against the 66-item DoD checklist in `bss_oss_dev_tracker_v3.xlsx` → sheet "✅ Definition of Done" (levels L0–L8).
**Methodology:** Every check below was either (a) executed live against this repo/a real Postgres instance/the running demo stack, or (b) verified by direct code/config inspection with exact file:line evidence. Checks that genuinely require infrastructure not available are marked **NOT VERIFIED**, not silently assumed passing. See "Methodology & Limitations" at the end.

Of the 66 rows in the tracker, **65 carry an actual check**; row `L0-011` is an empty placeholder in the source spreadsheet (all cells blank, and it is explicitly excluded from the tracker's own `COUNTIF(H3:H76,"Y")` formula range) — treated here as a tracker artifact, not a gradable item.

## Executive Summary

| Result | Count | % of 65 | Was (2026-08-08) |
|---|---|---|---|
| ✅ PASS | 49 | 75% | 41 |
| ❌ FAIL | 6 | 9% | 14 |
| 🟡 PARTIAL (spot-checked, not exhaustive) | 4 | 6% | 2 |
| ⬜ NOT VERIFIED (needs infrastructure not available) | 2 | 3% | 4 |
| ⬜ PENDING (manual tracker administration) | 4 | 6% | 4 |

**All four of the original audit's critical findings are now closed**, and the entire L6 performance block is either measured-passing or explicitly scoped out. The six remaining failures are concentrated in test-process conventions (TDD commit ordering, `TestFR_` naming) and one coverage shortfall — not in functional or security correctness.

### What changed since the original audit

| ID | Check | Was | Now | Basis |
|---|---|---|---|---|
| L1-009 / L4-010 | `lea_audit_log` append-only via RLS | ❌ FAIL (critical) | ✅ PASS | Migration `019_create_app_role.sql` — least-privilege `bss_app` role (commit `9cdb1dd`) |
| L3-003 | Redis on the RADIUS auth hot path | ❌ FAIL | ✅ PASS | `internal/radius/verifiercache.go` (commit `9cdb1dd`) |
| L5-007 | E.164 validated before SMS send | ❌ FAIL | ✅ PASS | `pkg/validate/phone.go` + migration `020` (commit `40d04f5`) |
| L6-001 | RADIUS p99 ≤15ms @ 5,000 req/s | ❌ FAIL (blocked) | ✅ PASS | **Measured: p99 10.4ms**, 149,850 requests, 0 errors |
| L6-002 | API p99 ≤200ms @ 500 concurrent | ⬜ NOT VERIFIED | ✅ PASS | **Measured: p99 11.69ms** at 500 VUs |
| L6-003 | Unbilled report ≤60s for 20k subscribers | ⬜ NOT VERIFIED | ✅ PASS | **Measured: 9.716ms** at 20,000 subscribers |
| L7-003 | `go.mod`/`go.sum` committed | ❌ FAIL | ✅ PASS | Commit `ae75ee4` |
| L0-010 / L7-001 | Conventional commit format | ❌ FAIL | 🟡 PARTIAL | 4 of 5 commits conform; the docs-only root commit predates the convention |

## Critical Findings (read this part first)

> **All four findings below are RESOLVED as of 2026-08-10.** They are kept in
> full rather than deleted, because each one records a real defect that was
> live in this codebase, and the resolution notes are only meaningful next to
> the original diagnosis. Each carries a ✅ **Resolved** note at its end.

### 1. Nothing has ever been committed to git except documentation
`git log` shows exactly one commit, `9d47cb6 "first commit"`, containing only `README.txt`, the three tracker `.xlsx` files, and `specification_docs_v2/*.md` — **18 files, zero lines of code**. Every single line of Go, every migration, every config file, every Dockerfile, `go.mod`/`go.sum` — the entire application, including all six months-worth-looking of module work and all six Portal UI phases built across this session — sits **untracked in the working directory**. `git status` currently shows 16 top-level untracked paths (`cmd/`, `internal/`, `migrations/`, `pkg/`, `scripts/`, `config/`, `go.mod`, `go.sum`, `docker-compose.yml`, both Dockerfiles, `.golangci.yml`, `.gitignore`, `.githooks/`, `.env.example`).

This single fact is the root cause of most of the L0/L7/L8 failures below (L0-001, L0-002, L0-009, L0-010, L7-001, L7-003, and all of L8) — they all assume a commit history that does not exist yet. **This is not a code-quality problem; it's a "the work has never been checked in" problem**, and it's the highest-leverage single fix available: committing what exists today would immediately resolve L7-003 outright and materially change several others.

✅ **Resolved** (commit `ae75ee4`, 2026-08-08). The full application — `cmd/`, `internal/`, `migrations/`, `pkg/`, `scripts/`, `config/`, `go.mod`/`go.sum`, both Dockerfiles, `docker-compose.yml` — is now tracked. Every commit since has been verified by cloning the repository to a clean directory and building it there in an isolated container, so "it builds from a fresh clone" is a tested claim rather than an assumption. L7-003 is now PASS. L0-001/L0-002 remain FAIL and always will: TDD red-phase ordering cannot be demonstrated retroactively for code that was written before it was ever committed.

### 2. The LEA audit log is not actually tamper-proof (L1-009 / L4-010)
Migration 014 enables Row-Level Security on `lea_audit_log` with an INSERT-only policy, intended to make it append-only per DBD §6.5. I tested this live: **the UPDATE succeeded and the row was tampered.**

```
UPDATE lea_audit_log SET accessor_identity = 'tampered' WHERE ...;
UPDATE 1
SELECT accessor_identity FROM lea_audit_log WHERE id = ...;
 tampered
```

Root cause: `docker-compose.yml` has the application connect to Postgres as `POSTGRES_USER=postgres` for everything — there is no separate least-privilege application role anywhere in the stack. PostgreSQL RLS **always** exempts the table owner (and always exempts literal superusers, regardless of `FORCE ROW LEVEL SECURITY`), and `postgres` here is both. So the exact role the application uses in production can silently rewrite or delete LEA lookup audit records — the one table that exists specifically to be tamper-evident for compliance. This is a real, exploitable gap, not a theoretical one.
**Fix requires:** a dedicated non-superuser, non-owner Postgres role for the application (`GRANT INSERT, SELECT` only on `lea_audit_log`, no `UPDATE`/`DELETE`), wired through a new `DB_DSN` — a real scope change, not a one-line patch.

✅ **Resolved** (migration `019_create_app_role.sql`, commit `9cdb1dd`). A `bss_app` role that is neither superuser nor table owner now exists, so RLS is no longer bypassed — PostgreSQL exempts owners and superusers from RLS, which was the entire root cause. The same UPDATE that previously tampered a row is now rejected. L1-009 and L4-010 are both PASS.

### 3. RADIUS auth still cannot meet its 15ms p99 budget (L3-003 / L6-001)
`internal/radius/handlers.go` calls `d.db.GetSubscriberByUsername` (straight to Postgres, no cache) followed by `bcrypt.CompareHashAndPassword` at cost 12 on every single Access-Request. This matches a gap already known from earlier in this engagement (bcrypt cost-12 alone measured at ~282ms/comparison — ~19× the 15ms budget) and confirms the previously-agreed mitigation — a fast verifier cache in Redis for the RADIUS path specifically — **was never implemented**. It was not part of any phase actually requested this session (all six phases built were Portal UI). Flagging it here since it's a real, currently-failing NFR, not new information, but it has not been revisited.

✅ **Resolved** (`internal/radius/verifiercache.go`, commit `9cdb1dd`) and now **measured**, not merely reasoned about. At 5,000 req/s against 20,000 distinct seeded subscribers: **p99 10.4ms**, 149,850 requests, 0 errors. Daemon-side counters show 249,850 cache hits against 39,960 misses, confirming the cache — not a faster database — is what carries the result.

The cache stores `HMAC-SHA256(secret, password ‖ passwordHash)`, never the password or the bcrypt hash. Binding to the *current* password hash is what makes a password change self-invalidate every cached verifier for that subscriber immediately; keying on the password alone would have kept accepting a rotated-away password for the full 5-minute TTL. A wrong guess cannot forge a matching verifier without the server-side secret, so incorrect passwords still fall through to the full bcrypt comparison exactly as before.

### 4. `go.mod` / `go.sum` are not committed (L7-003)
Direct consequence of Finding #1, called out separately because it's independently checkable and independently blocking: a fresh `git clone` of this repository today would not even build, since the module files aren't in git.

✅ **Resolved** (commit `ae75ee4`). Re-verified at every commit since by cloning to a clean directory and running `go build ./...`, `go vet -tags=integration ./...` and the test suite there in an isolated container.

### 5. The NFR harness could not have passed since the verifier cache landed *(found 2026-08-10)*

This one was found while running the load tests rather than reading the code, and is recorded because the failure it produced was actively misleading.

`scripts/run_nfr_tests.sh` never set `RADIUS_VERIFIER_SECRET`. Finding #3's fix made that variable **required** at radiusd startup, so from the moment the fast-verifier cache landed the daemon exited during config validation on every NFR run. The container had `--rm`, so it deleted itself on exit and left no logs behind.

What the harness reported was:

```
requests    : 1920 (accept 0, reject 0, error 1920)
throughput  : 64 req/s
error rate  : 100.0000%
FAIL: NFR-PERF-001: RADIUS p99 (threshold 15ms) at 2000 req/s
```

That reads as "RADIUS authentication is catastrophically slow." The daemon was not running at all. Two further problems in the same harness:

- `RATE` and `API_VUS` defaulted to 2,000 and 100 while the DoD specifies **5,000 req/s** (L6-001) and **500 concurrent** (L6-002) — so a fully green run demonstrated roughly a fifth of the required load in both dimensions. Both now default to the DoD levels.
- `scripts/load_test_radius.sh` shelled out to `radperf`, a third-party binary that is not vendored and not installed by any setup step in this repository, so it exited at its own `command -v radperf` check on every machine and had never measured anything. It now drives `cmd/radload`, which needs no external install.

✅ **Resolved** (commit `cb1d2bf`). All three fixes are in the committed harness, and the numbers in L6 below come from runs with them applied.

---

## L0 · Every Task (10 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L0-001 | Test written before impl (commit order) | ❌ FAIL | No commit history exists to check — see Finding #1. |
| L0-002 | Test fails before impl (red phase) | ❌ FAIL | Same — unverifiable via git; no red-phase commits exist. (Red→green was observed live in-session for individual TDD-style edits, but the DoD's literal check is git-log-based.) |
| L0-003 | `go build ./...` clean | ✅ PASS | Fresh run this audit: exit 0, no output. |
| L0-004 | `go vet ./...` clean | ✅ PASS | Fresh run this audit (`-tags=integration`): exit 0, no output. |
| L0-005 | Race detector clean ×3 | ✅ PASS | Fresh run this audit, full repo, 3 consecutive runs, 0 data races each time. |
| L0-006 | `golangci-lint` clean | ✅ PASS | Fresh run this audit: "0 issues." |
| L0-007 | No float64 for money | ✅ PASS | Literal grep (`float64\|math.Round\|Sprintf.*%.2f`) across all of `internal/`, `pkg/`, `cmd/`: 8 hits, all inspected — Prometheus metrics, percentage fields (`PctUsed`), latency histogram buckets, a load-test CLI's error-rate calc, and two comments *documenting* the no-float64-for-money rule. Zero money usage. |
| L0-008 | No plaintext PII in log statements | ✅ PASS | Grep for aadhaar/pan/mobile_number piped through `log\.`: zero matches. |
| L0-009 | Pre-commit hook passes on staged files | ❌ FAIL | `.githooks/pre-commit` exists and is well-written (verified it runs clean standalone), but is **not installed** — `.git/hooks/pre-commit` does not exist and `core.hooksPath` is unset. It would not fire on a real `git commit` today. One-line fix: `./scripts/install-hooks.sh`. |
| L0-010 | Conventional commit format | 🟡 PARTIAL | 4 of the 5 commits now conform (`feat:`, `fix:`, `feat:`, `test:`). The sole exception is the original docs-only root commit `9d47cb6 "first commit"`, which predates the convention and cannot be reworded without rewriting published history. Every commit containing code conforms. |

## L1 · DB Migration (9 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L1-001 | Migrations apply cleanly on empty DB | ✅ PASS | All **20** migrations apply cleanly against a fresh throwaway `postgres:15-alpine` — re-confirmed repeatedly during the load-test runs, which rebuild the schema from zero on every invocation. |
| L1-002 | Migrations roll back cleanly (Down works) | ✅ PASS | Full round trip re-tested at **all 20** migrations (2026-08-10): `up` → version 20, `down-to 0` → version 0, `up` again → version 20. Every Down statement succeeded, including the new `019` and `020`. |
| L1-003 | Columns match DBD §6.2 exactly | 🟡 PARTIAL | Spot-checked (types/defaults verified for tables touched this session — `subscribers`, `wallet_ledgers`, `invoices`, `tickets`, `plans`, `subscriber_session_history`); not exhaustively diffed column-by-column against DBD §6.2 for all 17 logical tables. |
| L1-004 | All DBD §6.4 indexes exist | 🟡 PARTIAL | 69 indexes exist on the schema; not exhaustively cross-referenced line-by-line against DBD §6.4. |
| L1-005 | INCLUDE clause on LEA index | ✅ PASS | `psql`: `idx_lea_ipv4_time ... INCLUDE (subscriber_id, stop_time)` confirmed present verbatim. |
| L1-006 | Constraint enforcement (chk_gst_logic) | ✅ PASS | Live test: inserting an invoice with both `cgst_amount` and `igst_amount` set → `ERROR: violates check constraint "chk_gst_logic"`. |
| L1-007 | Partitioned tables have ≥3 future partitions | ✅ PASS | `subscriber_session_history`: parent + `_2026_08` (current) + `_2026_09/10/11` (3 future) = matches `create_monthly_partitions(..., 3)` design exactly. |
| L1-008 | FK constraints enforced | ✅ PASS | Live test: inserting a subscriber with `plan_id=9999999` → `ERROR: violates foreign key constraint "subscribers_plan_id_fkey"`. |
| L1-009 | `lea_audit_log` blocks UPDATE via RLS | ✅ PASS | Was the critical failure in Finding #2. Migration `019_create_app_role.sql` adds the least-privilege `bss_app` role (neither superuser nor table owner), so RLS is no longer bypassed and the previously-succeeding UPDATE is rejected. |

## L2 · Unit Tests (6 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L2-001 | Every FR has a `TestFR_{ID}_...`-named test | ❌ FAIL | 220 test functions exist repo-wide; **zero** use the literal `TestFR_` prefix (actual style is descriptive, e.g. `TestDashboard_ShowsUsage`, `TestRenewal_IdempotentCallback`). Functional coverage of FRs is generally strong; the literal naming/traceability convention is not followed anywhere. |
| L2-002 | Coverage ≥80% (≥90% crypto/middleware) | ❌ FAIL (improved) | **The ≥90% sub-requirement now passes**: `pkg/crypto` 58.7% → **93.5%**, `internal/middleware` 69.6% → **98.2%**. The general ≥80% bar does not: overall 53.1% → **58.7%** (merged native + real-Postgres profiles). Largest movers: `internal/billing` 53.6% → **87.5%**, `internal/api` 55.4% → **69.3%**. Also passing: `pkg/validate` 100%, `internal/health` 100%, `internal/revenue` 80.7%. Still short: `internal/portalui` 79.6%, `internal/fup` 76.8%, `internal/notifications` 71.7%, `internal/portal` 63.0%, `internal/radius` 61.0%, `internal/cache` 47.7%, `cmd/api` 5.3%. |
| L2-003 | Happy-path tests exist | ✅ PASS | Present throughout (not literally suffixed `_HappyPath`, but functionally covered — e.g. every new Phase 0–6 handler has a valid-input test). |
| L2-004 | Error/rejection-path tests exist | ✅ PASS | Present throughout (e.g. `TestLogin_InvalidPassword_RerendersFormWithError`, `TestRenew_InvalidCSRFToken_Returns403`). |
| L2-005 | Edge cases (nil/zero/empty) handled without panic | ✅ PASS | Verified pattern throughout this session's own work (e.g. `TestDashboard_OfflineSubscriber_ShowsEmptyState`, `TestUsage_NoHistory_ShowsEmptyState`) and pre-existing tests. |
| L2-006 | Decimal precision to 2dp (billing) | ✅ PASS | `TestCalculateGstInvoice_Intrastate`/`_Interstate` assert exact `"71.91"`-style strings, not float comparisons. |

**Caveat on the "220 tests exist" figure used throughout L2.** Raising coverage in `internal/api` and `internal/billing` surfaced four tests that passed without testing what their names claimed, which means test *count* is a weaker signal here than it looks:

- `TestDunningTransition_ValidEdge` assigned its inputs to `_` and never called `TransitionDunning` at all.
- `TestDunningToSubscriberStatus` compared `billing.DunningActive` against the literal `"active"` it is defined as — an assertion that could not fail — and never reached the unexported `dunningToSubscriberStatus` it was named for.
- `TestGetSubscriberNotFound` and `TestWalletRechargeValidation` both stopped at the 401 auth gate, never reaching `GetSubscriber` or `WalletRecharge`.

All four are fixed in commit `cb1d2bf` (renamed where they were testing something real but mis-named, rewritten where they were not). The dunning tests now drive a fake querier through all nine edges in `validTransitions` and assert the state and status actually persisted. These were found only because coverage measurement pointed at functions reported as 0% despite having named tests — which is a good argument for keeping the L2-002 coverage gate rather than trusting the test count.

## L3 · Integration (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L3-001 | Docker stack healthy before integration tests | ✅ PASS | All 11 demo containers currently `Up`/`healthy` (postgres, redis×6, gotenberg, aaa_core_daemon, api_service, reverse_proxy). |
| L3-002 | Integration suite passes (`-tags=integration`) | ✅ PASS | Run repeatedly and successfully throughout this session; full `internal/db` suite re-confirmed clean this audit (63s, all green). |
| L3-003 | Redis actually used on the RADIUS auth hot path | ✅ PASS | `internal/radius/verifiercache.go` puts a Redis-backed fast verifier directly on the auth path. Confirmed in the load run by daemon-side counters, not just by code reading: 249,850 cache hits vs 39,960 misses at 5,000 req/s. |
| L3-004 | Asynq task actually enqueued in correct queue | ✅ PASS (code-verified) | Confirmed real `asynq.NewTask(...)` + `.Enqueue(...)`/`.EnqueueContext(...)` call sites for PoD, CoA, and FUP-warning tasks in `internal/api/sessions.go` and `internal/fup/scanner.go`. Not observed live via the `asynq` CLI this audit. |
| L3-005 | `notification_log` row created per dispatch attempt | ✅ PASS (code-verified) | `Dispatcher.Dispatch` calls `CreateNotificationLog` on every path, including the `suppressed_dnd` short-circuit. |
| L3-006 | Idempotency: same `transaction_token` twice = single credit | ✅ PASS | `TestRenewal_IdempotentCallback`, `TestRenewal_DistinctPaymentsBothCredit` exist and pass; this session additionally verified the renewal *expiry* side of idempotency live (Phase 4). |
| L3-007 | DND subscriber gets `suppressed_dnd` in log | ✅ PASS | Tested at both unit (`notifications_test.go`) and integration (`integration_test.go`) level; DND check confirmed (via direct code read) to unconditionally precede both the WhatsApp and SMS send branches. |
| L3-008 | Webhook HMAC: invalid signature → 400, no state change | ✅ PASS | `TestWebhookHMAC_InvalidSignatureRejected`, `TestWebhookHMAC_TamperedBodyRejected`, `TestRazorpayWebhook_InvalidSignatureRejected` all exist. |

## L4 · Security (10 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L4-001 | Zero plaintext Aadhaar/PAN in codebase logs | ✅ PASS | Grep scan clean. |
| L4-002 | Zero plaintext PII in DB column values | ✅ PASS (code-path verified) | `UpsertKYC` only ever receives pre-encrypted ciphertext from the API handler layer by construction (per its own doc comment); `Encrypt()` always produces the `{version}:{base64}` format. No live KYC rows currently seeded in the demo DB to visually spot-check. |
| L4-003 | AES ciphertext non-deterministic | ✅ PASS | `TestEncryptNonDeterministic` exists in `pkg/crypto`. |
| L4-004 | Cross-rotation decryption works | ✅ PASS | `TestCrossRotationDecrypt`, `TestDecrypt_CrossKeyVersion` exist. |
| L4-005 | Expired JWT → 401 | ✅ PASS | `TestJWTMiddleware_ExpiredToken` exists. |
| L4-006 | Wrong role → 403 | ✅ PASS | `TestRequireRole_WrongRoleReturns403`, `TestRequireRole_RoleMatrix` exist. |
| L4-007 | `hmac.Equal` (timing-safe), not `==` | ✅ PASS | Confirmed at all 3 HMAC comparison sites: `internal/billing/webhook.go`, `internal/notifications/webhook.go`, and this session's own `internal/portalui/auth.go` CSRF check. |
| L4-008 | HMAC validated before `json.Unmarshal` | ✅ PASS | `internal/api/webhook_razorpay.go`: `ValidateRazorpaySignature` at line 61, `json.Unmarshal` at line 68 — correct order. |
| L4-009 | `gosec` clean | ✅ PASS | `gosec` is enabled in `.golangci.yml`'s linter set; fresh `golangci-lint run` this audit: 0 issues. |
| L4-010 | LEA audit log append-only (DB-level) | ✅ PASS | Same fix as L1-009 — see Finding #2's resolution note. |

## L5 · Notifications (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L5-001 | WhatsApp uses `"type": "template"` | ✅ PASS | Confirmed in `internal/notifications/whatsapp.go:133-134`. |
| L5-002 | `provider_message_id` saved after send | ✅ PASS | Flows from `whatsapp.go` through to `internal/db/notifications.go`'s insert. |
| L5-003 | Meta webhook updates `delivery_status` sent→delivered | ✅ PASS | `TestWhatsAppWebhook_UpdatesDeliveryStatus` exists and exercises exactly this transition. |
| L5-004 | Meta webhook HMAC via `X-Hub-Signature-256` | ✅ PASS | `ValidateMetaSignature` reads and verifies the header before any DB write; 5 related tests exist. |
| L5-005 | DND check fires before any channel API call | ✅ PASS | Direct code read of `Dispatcher.Dispatch`: DND check at line 38 unconditionally precedes both the WhatsApp branch (line 63) and SMS branch (line 75). |
| L5-006 | All 8 templates (TMPL-001..008) registered | ✅ PASS | Confirmed in `scripts/seed_local.sql` — exactly 8, correctly numbered. |
| L5-007 | E.164 phone format validated before SMS send | ✅ PASS | `pkg/validate/phone.go` (100% covered) enforces `^\+[1-9]\d{1,14}$` at three layers: `CreateSubscriber`, both the SMS and WhatsApp send paths, and a DB `CHECK` constraint in migration `020_phone_e164_constraint.sql`. |
| L5-008 | Dispatch latency ≤5s (dequeue → `sent_at`) | ❌ FAIL / NOT VERIFIED | No dedicated E2E latency test exists (`TestFUPWarningTask_E2ELatency` or equivalent is not present). |

## L6 · Performance (6 checks)

All L6 rows below were run at the levels the DoD itself specifies, against 20,000 seeded subscribers on a throwaway stack. Raw harness: `scripts/run_nfr_tests.sh` (L6-001/002/003) and `scripts/verify_tls.sh` (L6-006).

| ID | Check | Result | Evidence |
|---|---|---|---|
| L6-001 | RADIUS auth p99 ≤15ms @ 5,000 req/s | ✅ PASS | **Measured at the full 5,000 req/s**: p99 **10.4ms**, p50 4.538ms, p95 7.934ms across 149,850 requests, 0 errors, 0 dropped to saturation. Server-side mean auth 3.211ms over 289,810 requests; 289,315 of them completed within 15ms. A capacity sweep confirms the budget holds at 3,000 and 4,000 req/s too, so 5,000 is not a cliff edge. |
| L6-002 | API p99 ≤200ms @ 500 concurrent (k6) | ✅ PASS | **Measured at the full 500 VUs** for 30s via `scripts/k6_api_load.js`: p99 **11.69ms** against the 200ms budget, 286,386 checks, 100% succeeded, `http_req_failed` 0.00%. Both k6 thresholds (`health`, `subscriber_get`) passed independently. |
| L6-003 | Unbilled report ≤60s for 20k subscribers | ✅ PASS | **9.716ms** against a 60,000ms budget, on a real 20,000-subscriber / 18,000-invoice dataset. The planner chooses a sequential scan at this row count rather than an index scan — expected, and ~6,000× inside budget regardless. |
| L6-004 | Redis Sentinel failover ≤3s | ⬜ NOT VERIFIED | Unchanged. Sentinel topology/config verified correct (quorum, `resolve-hostnames`), but literal failover timing still needs the full Sentinel stack, which `run_nfr_tests.sh` deliberately does not stand up. |
| L6-005 | No goroutine leak after 1h @ 20k sessions | ⬜ NOT VERIFIED | Unchanged. Needs an hour of sustained load; not run. |
| L6-006 | TLS 1.3 minimum, TLS 1.2 disabled | ✅ PASS | Now covered by a committed, repeatable script (`scripts/verify_tls.sh`) rather than a one-off `curl`, run against the **real** `config/caddy/Caddyfile`: TLS 1.3 connects (`TLS_AES_128_GCM_SHA256`), TLS 1.2 / 1.1 / 1.0 all refused, and an unpinned client negotiates 1.3. See the note below on why this test is trustworthy. |

**On L6-006's trustworthiness.** The first version of this script passed TLS 1.3 and reported TLS 1.2 as *negotiated* — a false alarm caused by reading OpenSSL's `Protocol :` session line, which echoes the version the client *attempted* even when the server rejected it outright. A refused TLS 1.2 handshake still prints `Protocol : TLSv1.2` there. The script now keys off the `New, <version>, Cipher is <cipher>` line instead, since a version is only real if a cipher was agreed alongside it (`New, (NONE), Cipher is (NONE)` on failure). It was then checked against a deliberately weakened Caddyfile permitting `tls1.2 tls1.3`, and correctly failed — so the pass above reflects the server's actual behaviour, not an assertion that cannot fail.

## L7 · Git Hygiene (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L7-001 | Commit message conventional format | 🟡 PARTIAL | Same as L0-010 — 4 of 5 conform; the docs-only root commit does not and cannot be reworded without rewriting published history. |
| L7-002 | No `.env`/secret files in git history | ✅ PASS | Re-checked after the code was committed, which is when this became a meaningful test rather than a trivially-true one: `git log --all --full-history -- ".env" "*.key" "*.pem"` → still empty. |
| L7-003 | `go.mod`/`go.sum` committed | ✅ PASS | Committed in `ae75ee4`. Verified by cloning the repo to a clean directory and building there — not merely by `git status`. |
| L7-004 | Migrations numbered sequentially, no gaps | ✅ PASS | Now `001` through `020` (019 = least-privilege app role, 020 = E.164 constraint), no gaps. |

## L8 · Sign-Off (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L8-001 | Tracker Status column = Done | ⬜ PENDING | Manual tracker administration — outside code-audit scope. None of this session's Portal UI work (or anything else) has been marked in the tracker's module sheets yet. |
| L8-002 | All prerequisite tasks also Done | ⬜ PENDING | Same — depends on L8-001 first. |
| L8-003 | Notes column documents deviations | ⬜ PENDING | Same. |
| L8-004 | Matching INT-* row marked Done in Integration Tests sheet | ⬜ PENDING | Same. |

---

## Gaps Requiring Explicit Attention — Ranked by Priority

Items 1, 2, 4 and 6 from the original 2026-08-08 list are closed; see the resolution notes in Critical Findings. What remains:

1. **Install the pre-commit hook** (L0-009). Still the cheapest open item and still not done: `.githooks/pre-commit` exists and runs clean standalone, but `core.hooksPath` is unset and `.git/hooks/pre-commit` does not exist, so the PII-leak guard does not fire on a real commit. One command: `./scripts/install-hooks.sh`. Left undone deliberately — it changes how every future commit on this machine behaves, which is the user's call, not a side effect of a test run.
2. **Test coverage below the ≥80% general bar** (L2-002) — 58.7% overall. The stricter ≥90% crypto/middleware requirement now passes (93.5% / 98.2%). The remaining shortfall is concentrated in `cmd/api` (5.3%), `internal/cache` (47.7%) and `internal/radius` (61.0%). Note that `internal/radius` is now security-relevant in a way it was not before: the fast-verifier cache is on the auth path, so that package is the highest-value coverage target of the three.
3. **Redis Sentinel failover timing** (L6-004) — needs the full Sentinel stack stood up and a master killed under load. The config is verified correct; the 3-second recovery claim is not measured.
4. **Goroutine-leak soak test** (L6-005) — needs an hour of sustained load at 20k sessions plus `pprof` sampling. Nothing structural blocks it; it is purely a time cost, and `run_nfr_tests.sh` already builds the stack it would need.
5. **FR-traceable test naming** (L2-001) — a documentation/process convention, not a functional gap. Tests exist and pass; they just aren't named `TestFR_{ID}_...` the way the tracker's own verification command expects.
6. **Dispatch-latency E2E test** (L5-008) — no test measures dequeue → `sent_at` against the 5s budget.
7. **TDD commit ordering** (L0-001, L0-002) — permanently unachievable for existing code, since red-phase commits cannot be reconstructed after the fact. Worth deciding whether to enforce going forward or mark N/A in the tracker, rather than leaving it as a standing failure.
8. **Tracker administration** (all of L8) — the spreadsheet's own Status/Notes columns are still unpopulated. See the last bullet under Methodology.

## Methodology & Limitations

- Rows revised on 2026-08-10 carry their evidence inline and are summarised in "What changed since the original audit" at the top. Rows not mentioned there are unchanged from the 2026-08-08 audit and were **not** re-executed on 2026-08-10 — they retain their original basis, which is stated per row.
- All L0/L1/L4/L7 checks with concrete pass/fail language above were **executed live** during the original audit (fresh `go build`/`vet`/`race`×3/`lint` runs, a throwaway Postgres for migration up/down/constraint/FK/partition/RLS testing, live `curl`-based TLS verification, direct `git log`/`git status` inspection) — not inferred from memory of earlier session work.
- L2/L3/L5 checks are primarily **code-verified** (exact file:line citations, existing test names confirmed present) rather than re-executed live one-by-one, given the volume (220 existing tests); L3-002's full suite and the coverage numbers in L2-002 *were* freshly re-run.
- **L6 was re-graded from live measurement on 2026-08-10**, not from code reading. Every number in L6-001/002/003 comes from a run against a throwaway Postgres + Redis + API + radiusd stack with 20,000 seeded subscribers, at the load levels the DoD specifies. L6-004 and L6-005 remain **NOT VERIFIED** and are not being presented as anything else.
- **Caveat on the L6 runs:** the containers were built from current source, but the images were built earlier in the session and reused for the final measurement runs, because Docker Hub certificate validation was failing on this machine (the host clock is roughly 6 hours behind real time, so registry certs read as not-yet-valid). No source changed between the image build and the measurement runs. The system clock was left alone rather than adjusted for a test run.
- The load runs also exposed that a green NFR result is not self-validating: the harness reported a 100% RADIUS error rate that looked like a latency failure and was actually a daemon that never started, and the first TLS script reported a downgrade that never happened. Both are written up in Finding #5 and L6-006 respectively. Where a check could plausibly pass for the wrong reason, it was tested against a deliberately broken configuration to confirm it fails when it should.
- L1-003/L1-004 are marked **PARTIAL** rather than PASS because a full 17-table, column-by-column diff against DBD §6.2/§6.4 was not performed — spot checks on tables touched this session all matched.
- This report is a standalone Markdown file, not a rewrite of `bss_oss_dev_tracker_v3.xlsx`'s "Your Result"/"Pass?" columns — editing that spreadsheet's XML directly by hand risks corrupting its formulas/formatting. If you'd like those columns populated too, that's a reasonable next step now that the underlying findings are established here.
