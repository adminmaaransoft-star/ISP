# Definition of Done — Status Report

**Date:** 2026-08-08
**Scope:** Whole-codebase audit against the 66-item DoD checklist in `bss_oss_dev_tracker_v3.xlsx` → sheet "✅ Definition of Done" (levels L0–L8).
**Methodology:** Every check below was either (a) executed live against this repo/a real Postgres instance/the running demo stack during this audit, or (b) verified by direct code/config inspection with exact file:line evidence. Checks that genuinely require infrastructure not available in this session (load-test tooling, multi-hour soak tests) are marked **NOT VERIFIED**, not silently assumed passing. See "Methodology & Limitations" at the end.

Of the 66 rows in the tracker, **65 carry an actual check**; row `L0-011` is an empty placeholder in the source spreadsheet (all cells blank, and it is explicitly excluded from the tracker's own `COUNTIF(H3:H76,"Y")` formula range) — treated here as a tracker artifact, not a gradable item.

## Executive Summary

| Result | Count | % of 65 |
|---|---|---|
| ✅ PASS | 41 | 63% |
| ❌ FAIL | 14 | 22% |
| 🟡 PARTIAL (spot-checked, not exhaustive) | 2 | 3% |
| ⬜ NOT VERIFIED (needs dedicated load-test session) | 4 | 6% |
| ⬜ PENDING (manual tracker administration) | 4 | 6% |

**The codebase itself — build, vet, race, lint, and the vast majority of functional/security correctness checks — is in excellent shape.** The failures are concentrated in four specific, well-defined areas, not spread randomly across the checklist.

## Critical Findings (read this part first)

### 1. Nothing has ever been committed to git except documentation
`git log` shows exactly one commit, `9d47cb6 "first commit"`, containing only `README.txt`, the three tracker `.xlsx` files, and `specification_docs_v2/*.md` — **18 files, zero lines of code**. Every single line of Go, every migration, every config file, every Dockerfile, `go.mod`/`go.sum` — the entire application, including all six months-worth-looking of module work and all six Portal UI phases built across this session — sits **untracked in the working directory**. `git status` currently shows 16 top-level untracked paths (`cmd/`, `internal/`, `migrations/`, `pkg/`, `scripts/`, `config/`, `go.mod`, `go.sum`, `docker-compose.yml`, both Dockerfiles, `.golangci.yml`, `.gitignore`, `.githooks/`, `.env.example`).

This single fact is the root cause of most of the L0/L7/L8 failures below (L0-001, L0-002, L0-009, L0-010, L7-001, L7-003, and all of L8) — they all assume a commit history that does not exist yet. **This is not a code-quality problem; it's a "the work has never been checked in" problem**, and it's the highest-leverage single fix available: committing what exists today would immediately resolve L7-003 outright and materially change several others.

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

### 3. RADIUS auth still cannot meet its 15ms p99 budget (L3-003 / L6-001)
`internal/radius/handlers.go` calls `d.db.GetSubscriberByUsername` (straight to Postgres, no cache) followed by `bcrypt.CompareHashAndPassword` at cost 12 on every single Access-Request. This matches a gap already known from earlier in this engagement (bcrypt cost-12 alone measured at ~282ms/comparison — ~19× the 15ms budget) and confirms the previously-agreed mitigation — a fast verifier cache in Redis for the RADIUS path specifically — **was never implemented**. It was not part of any phase actually requested this session (all six phases built were Portal UI). Flagging it here since it's a real, currently-failing NFR, not new information, but it has not been revisited.

### 4. `go.mod` / `go.sum` are not committed (L7-003)
Direct consequence of Finding #1, called out separately because it's independently checkable and independently blocking: a fresh `git clone` of this repository today would not even build, since the module files aren't in git.

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
| L0-010 | Conventional commit format | ❌ FAIL | The one existing commit message is `"first commit"` — doesn't match `feat()\|fix()\|test()\|db:\|chore:\|refactor()`. |

## L1 · DB Migration (9 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L1-001 | Migrations apply cleanly on empty DB | ✅ PASS | All 18 migrations applied cleanly against a fresh throwaway `postgres:15-alpine`, verified this audit. |
| L1-002 | Migrations roll back cleanly (Down works) | ✅ PASS | Full round trip tested this audit: all 18 migrations `down` to zero, then `up` again — every Down statement succeeded, re-up succeeded. **Not previously tested this session** — this was new coverage. |
| L1-003 | Columns match DBD §6.2 exactly | 🟡 PARTIAL | Spot-checked (types/defaults verified for tables touched this session — `subscribers`, `wallet_ledgers`, `invoices`, `tickets`, `plans`, `subscriber_session_history`); not exhaustively diffed column-by-column against DBD §6.2 for all 17 logical tables. |
| L1-004 | All DBD §6.4 indexes exist | 🟡 PARTIAL | 69 indexes exist on the schema; not exhaustively cross-referenced line-by-line against DBD §6.4. |
| L1-005 | INCLUDE clause on LEA index | ✅ PASS | `psql`: `idx_lea_ipv4_time ... INCLUDE (subscriber_id, stop_time)` confirmed present verbatim. |
| L1-006 | Constraint enforcement (chk_gst_logic) | ✅ PASS | Live test: inserting an invoice with both `cgst_amount` and `igst_amount` set → `ERROR: violates check constraint "chk_gst_logic"`. |
| L1-007 | Partitioned tables have ≥3 future partitions | ✅ PASS | `subscriber_session_history`: parent + `_2026_08` (current) + `_2026_09/10/11` (3 future) = matches `create_monthly_partitions(..., 3)` design exactly. |
| L1-008 | FK constraints enforced | ✅ PASS | Live test: inserting a subscriber with `plan_id=9999999` → `ERROR: violates foreign key constraint "subscribers_plan_id_fkey"`. |
| L1-009 | `lea_audit_log` blocks UPDATE via RLS | ❌ **FAIL (critical)** | See Finding #2 — live UPDATE succeeded and tampered a row. RLS is enabled but the app's own DB role bypasses it. |

## L2 · Unit Tests (6 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L2-001 | Every FR has a `TestFR_{ID}_...`-named test | ❌ FAIL | 220 test functions exist repo-wide; **zero** use the literal `TestFR_` prefix (actual style is descriptive, e.g. `TestDashboard_ShowsUsage`, `TestRenewal_IdempotentCallback`). Functional coverage of FRs is generally strong; the literal naming/traceability convention is not followed anywhere. |
| L2-002 | Coverage ≥80% (≥90% crypto/middleware) | ❌ FAIL | Measured this audit with `-tags=integration` against real Postgres: **overall 53.1%**. Per-package: `internal/health` 100%, `internal/revenue` 80.7% (both pass); `internal/portalui` 79.6%, `internal/db` 76.9%, `internal/fup` 76.8% (all just under 80%); `pkg/crypto` **58.7%** and `internal/middleware` **69.6%** (both below their required 90% threshold); `internal/api` 55.4%, `internal/billing` 53.6%, `internal/radius` 55.5%, `internal/cache` 47.7%, `internal/portal` 63.0%, `internal/notifications` 69.9%, `cmd/api` 5.3% all below the general 80% bar. |
| L2-003 | Happy-path tests exist | ✅ PASS | Present throughout (not literally suffixed `_HappyPath`, but functionally covered — e.g. every new Phase 0–6 handler has a valid-input test). |
| L2-004 | Error/rejection-path tests exist | ✅ PASS | Present throughout (e.g. `TestLogin_InvalidPassword_RerendersFormWithError`, `TestRenew_InvalidCSRFToken_Returns403`). |
| L2-005 | Edge cases (nil/zero/empty) handled without panic | ✅ PASS | Verified pattern throughout this session's own work (e.g. `TestDashboard_OfflineSubscriber_ShowsEmptyState`, `TestUsage_NoHistory_ShowsEmptyState`) and pre-existing tests. |
| L2-006 | Decimal precision to 2dp (billing) | ✅ PASS | `TestCalculateGstInvoice_Intrastate`/`_Interstate` assert exact `"71.91"`-style strings, not float comparisons. |

## L3 · Integration (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L3-001 | Docker stack healthy before integration tests | ✅ PASS | All 11 demo containers currently `Up`/`healthy` (postgres, redis×6, gotenberg, aaa_core_daemon, api_service, reverse_proxy). |
| L3-002 | Integration suite passes (`-tags=integration`) | ✅ PASS | Run repeatedly and successfully throughout this session; full `internal/db` suite re-confirmed clean this audit (63s, all green). |
| L3-003 | Redis actually used on the RADIUS auth hot path | ❌ **FAIL** | See Finding #3 — `handlers.go`'s auth path calls Postgres directly with no Redis subscriber-cache read; Redis is used elsewhere in `internal/radius` (accounting dedup, rate limiting) but not for auth caching. |
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
| L4-010 | LEA audit log append-only (DB-level) | ❌ **FAIL (critical)** | Same finding as L1-009 — see Finding #2. |

## L5 · Notifications (8 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L5-001 | WhatsApp uses `"type": "template"` | ✅ PASS | Confirmed in `internal/notifications/whatsapp.go:133-134`. |
| L5-002 | `provider_message_id` saved after send | ✅ PASS | Flows from `whatsapp.go` through to `internal/db/notifications.go`'s insert. |
| L5-003 | Meta webhook updates `delivery_status` sent→delivered | ✅ PASS | `TestWhatsAppWebhook_UpdatesDeliveryStatus` exists and exercises exactly this transition. |
| L5-004 | Meta webhook HMAC via `X-Hub-Signature-256` | ✅ PASS | `ValidateMetaSignature` reads and verifies the header before any DB write; 5 related tests exist. |
| L5-005 | DND check fires before any channel API call | ✅ PASS | Direct code read of `Dispatcher.Dispatch`: DND check at line 38 unconditionally precedes both the WhatsApp branch (line 63) and SMS branch (line 75). |
| L5-006 | All 8 templates (TMPL-001..008) registered | ✅ PASS | Confirmed in `scripts/seed_local.sql` — exactly 8, correctly numbered. |
| L5-007 | E.164 phone format validated before SMS send | ❌ FAIL | No validation logic exists anywhere in the codebase — phone fields are typed `string` with an `// E.164` comment but nothing enforces the format at any layer. |
| L5-008 | Dispatch latency ≤5s (dequeue → `sent_at`) | ❌ FAIL / NOT VERIFIED | No dedicated E2E latency test exists (`TestFUPWarningTask_E2ELatency` or equivalent is not present). |

## L6 · Performance (6 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L6-001 | RADIUS auth p99 ≤15ms @ 5,000 req/s | ❌ **FAIL (blocked)** | See Finding #3. Not re-measured live with `radperf` this audit (tool unavailable), but the root cause (no cache, bcrypt cost-12 on every request) is directly confirmed present in current code, so the NFR cannot currently be met regardless of live measurement. |
| L6-002 | API p99 ≤200ms @ 500 concurrent (k6) | ⬜ NOT VERIFIED | `k6` not run this session. |
| L6-003 | Unbilled report ≤60s for 20k subscribers | ⬜ NOT VERIFIED | Not run this session; no 20k-row dataset loaded. |
| L6-004 | Redis Sentinel failover ≤3s | ⬜ NOT VERIFIED | Sentinel topology/config verified correct earlier this session (quorum, `resolve-hostnames`), but literal failover timing was never measured. |
| L6-005 | No goroutine leak after 1h @ 20k sessions | ⬜ NOT VERIFIED | No soak test run. |
| L6-006 | TLS 1.3 minimum, TLS 1.2 disabled | ✅ PASS | Verified **live** earlier this session against the real Caddy reverse proxy: `curl --tls-max 1.2` → handshake failure (exit 35); `curl --tlsv1.3` → HTTP 200. Not run via `testssl.sh` specifically, but the actual protocol-negotiation behavior was directly proven, which is what the check cares about. |

## L7 · Git Hygiene (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L7-001 | Commit message conventional format | ❌ FAIL | Same as L0-010. |
| L7-002 | No `.env`/secret files in git history | ✅ PASS | `git log --all --full-history -- ".env" "*.key" "*.pem"` → empty. (Trivially true today since only docs were ever committed — worth re-checking once code is committed.) |
| L7-003 | `go.mod`/`go.sum` committed | ❌ **FAIL** | Both show as untracked (`??`) in `git status` right now. |
| L7-004 | Migrations numbered sequentially, no gaps | ✅ PASS | `001` through `018`, verified via `ls migrations/`, no gaps. |

## L8 · Sign-Off (4 checks)

| ID | Check | Result | Evidence |
|---|---|---|---|
| L8-001 | Tracker Status column = Done | ⬜ PENDING | Manual tracker administration — outside code-audit scope. None of this session's Portal UI work (or anything else) has been marked in the tracker's module sheets yet. |
| L8-002 | All prerequisite tasks also Done | ⬜ PENDING | Same — depends on L8-001 first. |
| L8-003 | Notes column documents deviations | ⬜ PENDING | Same. |
| L8-004 | Matching INT-* row marked Done in Integration Tests sheet | ⬜ PENDING | Same. |

---

## Gaps Requiring Explicit Attention — Ranked by Priority

1. **Commit the codebase.** Nothing else on this list can be meaningfully re-graded (L0-001, L0-002, L0-009, L0-010, L7-001, L7-003, all of L8) until the actual application code exists in git history. This is the single highest-leverage next action.
2. **Fix the LEA audit log's real tamper protection** (L1-009 / L4-010). Needs a dedicated, least-privilege Postgres role for the application — a genuine scope item (new role, new grants, new DSN), not a quick patch. This is a compliance-relevant gap for a table that exists specifically for law-enforcement audit trail integrity.
3. **Install the pre-commit hook** (L0-009). Trivial fix (`./scripts/install-hooks.sh`) but currently the PII-leak guard it provides is not actually running.
4. **RADIUS auth performance** (L3-003 / L6-001) remains architecturally unresolved — the Redis fast-verifier-cache design was discussed earlier in this engagement but never built. Still an open item, not new.
5. **Test coverage is below the DoD's stated thresholds** (L2-002) — overall 53.1% vs. the required 80%, and `pkg/crypto`/`internal/middleware` specifically below their required 90%. Functional/security test *quality* is generally strong (see L4's near-clean sweep); this is a *quantity* gap in less-critical code paths.
6. **E.164 phone validation** (L5-007) doesn't exist anywhere — worth adding before SMS volume scales, to avoid silent gateway rejections or misdirected sends.
7. **FR-traceable test naming** (L2-001) — a documentation/process convention, not a functional gap. Tests exist and pass; they just aren't named `TestFR_{ID}_...` the way the tracker's own verification command expects.
8. **L6 performance NFRs** (API latency, unbilled-report runtime, Sentinel failover timing, goroutine-leak soak test) need a dedicated load-testing session with `k6`/`radperf`/`testssl.sh`/`pprof` — none of that tooling was exercised in this audit.

## Methodology & Limitations

- All L0/L1/L4/L7 checks with concrete pass/fail language above were **executed live** during this audit (fresh `go build`/`vet`/`race`×3/`lint` runs, a throwaway Postgres for migration up/down/constraint/FK/partition/RLS testing, live `curl`-based TLS verification, direct `git log`/`git status` inspection) — not inferred from memory of earlier session work.
- L2/L3/L5 checks are primarily **code-verified** (exact file:line citations, existing test names confirmed present) rather than re-executed live one-by-one, given the volume (220 existing tests); L3-002's full suite and the coverage numbers in L2-002 *were* freshly re-run.
- L6 (except L6-006) is honestly marked **NOT VERIFIED** rather than assumed — this repo has no load-testing tools installed, and fabricating pass results for NFR-PERF/NFR-AVAIL/NFR-SCAL checks would be worse than leaving them open.
- L1-003/L1-004 are marked **PARTIAL** rather than PASS because a full 17-table, column-by-column diff against DBD §6.2/§6.4 was not performed — spot checks on tables touched this session all matched.
- This report is a standalone Markdown file, not a rewrite of `bss_oss_dev_tracker_v3.xlsx`'s "Your Result"/"Pass?" columns — editing that spreadsheet's XML directly by hand risks corrupting its formulas/formatting. If you'd like those columns populated too, that's a reasonable next step now that the underlying findings are established here.
