# Handoff — state of play

Written 2026-08-12 at commit `deb06d2`; updated same day after pushing to
`origin/main` and closing item 1. Delete this file once the items below are
done; it describes a moment, not the system.

---

## Do this first

**Pushed.** `origin/main` is now at `63b3c57` (the 6 commits that were
unpushed, including this file's own initial version, all landed). Pre-push
audit confirmed `.env` untracked, no key material in history, `node_modules`
and `e2e/snapshots` excluded.

**Still open: the `v1.1.0` tag.** `v1.0.0` still points at `74e2655` and should
— it's a released tag, not to be moved. Six commits of work sit on top of it
(five original + the FR-NOTIF-007 work below). Cutting `v1.1.0` is a naming
judgement call for a person, not something to decide unattended:

```bash
git tag -a v1.1.0 -m "..." && git push origin v1.1.0
```

---

## Completed this session

### FR-NOTIF-007 — notify on ticket status change ✅

The console let a CSR change a ticket status and the subscriber was never
told; `TMPL-008` (`ticket_update`) was seeded but nothing dispatched it. Fixed
in both places that change a ticket's status, not just the console:

- New package `internal/tickets` (`notify_task.go`): `TaskTypeTicketUpdate`,
  `UpdatePayload`, `UpdateHandler` — same shape as
  `internal/billing/dunning_task.go`.
- `internal/notifications/whatsapp.go`: registered `TMPL-008` →
  `ticket_update` in `templateNames` (it was seeded but had no Meta template
  name mapping — would have sent with the raw ID as the name).
- `internal/staffui`: added a `Tasks` dependency; `UpdateTicketStatus` enqueues
  after a successful change.
- `internal/api/tickets.go`: `UpdateTicket` (the JSON API's PATCH endpoint)
  also enqueues — it had the identical gap and shares the same
  `UpdateTicketAdmin` call, so leaving it out would have left one of the two
  real entry points still silently unwired. Guarded to fire only when
  `status` actually changed, not on an assignee-only patch.
- `cmd/api/main.go` / `cmd/radiusd/main.go`: wired the `Tasks` dependency and
  registered the new handler on the worker mux.
- `scripts/check_wiring.sh`: added `NewUpdateHandler` to the tracked
  components (now 12/12).

Verified past "it compiles": unit tests in `internal/tickets` with a
deliberate-break negative control (swapped the template var order, confirmed
the test failed, reverted); two new integration tests in
`internal/api/new_endpoints_integration_test.go` (status change enqueues,
assignee-only does not) with the same negative-control treatment; then
rebuilt `api_service` and `aaa_core_daemon`, ran the existing Playwright
ticket-workflow test against the real containers, and confirmed the full
round trip in Postgres:

```
notification_log: subscriber_id=1, template_id=TMPL-008, triggered_by_event=ticket_update, delivery_status=failed
```

`failed` is expected and correct — the demo stack's `WHATSAPP_ACCESS_TOKEN`
is not a real Meta credential (confirmed via a 401 in the worker log), the
same pre-existing limitation the dunning and FUP notifications already have
here. The point of the check was confirming the task actually reaches the
worker and a real send is attempted, which it does.

**One discrepancy found and worth knowing:** `golangci-lint run ./...`
reports 2 pre-existing `gosec` findings (G705 in `internal/api/invoices.go`,
G710 in `internal/staffui/screens.go`) — confirmed present on the untouched
`63b3c57` tree via `git stash`, not introduced this session. The "0 issues"
claim in the previous handoff no longer holds; nobody has fixed or triaged
these yet.

**Environment note for next time:** this repo has `core.autocrlf=true`. Any
`git stash`/`pop` round-trip re-CRLFs whatever it touches, which makes
`gofmt -l` flag entire files as unformatted even though nothing semantic
changed. Fix with `sed -i 's/\r$//' <file>` on the affected files rather than
touching `core.autocrlf` (never change git config per this project's rules).
Also: the demo stack's seeded Redis live-session key
(`session:active:{id}`) has a 24h TTL (`scripts/demo_up.sh`); a stack left
running longer than that will fail the two Playwright tests that check the
"online" dashboard state for reasons that have nothing to do with your
change. Reseed with the same `redis-cli SET ... EX 86400` command in
`demo_up.sh` before trusting a red result there.

---

## Next tasks, in priority order

### 1. Console write actions (~1 week)

`internal/staffui` is read-mostly. The write actions the API already supports
have no screen:

| Action | API route | Role |
|---|---|---|
| Disconnect session | `POST /api/v1/sessions/{id}/disconnect` | noc_engineer |
| FUP override | `POST /api/v1/sessions/{id}/fup-override` | noc_engineer |
| Credit wallet | `POST /api/v1/wallets/recharge` | billing_admin, csr |
| Create subscriber | `POST /api/v1/subscribers` | billing_admin |
| Update subscriber | `PATCH /api/v1/subscribers/{id}` | billing_admin |

Each needs a confirmation step and an audit entry — more design than typing.

### 2. Security review of `staff_users` (~2 days + a decision)

Migration 021 added a **new authentication surface**. Not specified anywhere,
so these are open decisions rather than omissions:

- no MFA
- no password rotation or expiry
- no lockout after repeated failures
- no screen for creating accounts (they come from the seed)
- 8-hour sessions

### 3. L6-004 Sentinel failover — a decision, not a sprint

Measured 5055ms on the shipped config against a 3000ms budget; ~3.1s with
`down-after-milliseconds` at 500. Recommendation: adopt 500 and move the target
to 8s. **Separately**, decide whether the DNS/tilt failure mode is acceptable —
when a master container leaves Docker DNS, Sentinel never promotes at all.
Reproduce: `CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh`.
See `DOD_STATUS_REPORT.md` Finding #6.

### 4. The 1-hour soak (L6-005) — pure time

`./scripts/run_soak_test.sh` is committed and validated on a 90s run. Needs one
uninterrupted hour on a machine that will not sleep.

### 5. Remaining unimplemented requirements

40 of 52 FRs are traceable to a `TestFR_` test (FR-NOTIF-007 moved from
"unimplemented" to tested this session). Genuinely unimplemented:

- `FR-NOTIF-003` / `FR-FUP-005` — notify when FUP throttle is applied (same
  missing-trigger shape FR-NOTIF-007 had)
- `FR-NET-003` — IPv6 prefix recording; the column exists in migration 010 and
  nothing writes it
- `FR-FRN-003` — consolidated P&L across LCO partners

---

## Things a fresh session will not know

### Running anything

```bash
./scripts/demo_up.sh                     # full stack; generates .env secrets
export COMPOSE_PROJECT_NAME=isp_bss_demo # demo_up.sh hardcodes this
```

Without that variable every `docker compose` command silently targets a
different project and reports containers as not running.

### Docker on this machine

- Docker Desktop drops out mid-session. Recover:
  `powershell -Command "Start-Process 'C:\Program Files\Docker\Docker\Docker Desktop.exe'"`
  then poll `until docker info; do sleep 3; done`.
- Registry pulls have failed with certificate errors when the host clock drifts
  (the cert reads as not-yet-valid). Cached images still work; do not "fix" it
  by changing the system clock.

### Paths and Docker

`MSYS_NO_PATHCONV=1` is required for most `docker run` arguments, but it means
POSIX paths reach the Windows Docker binary unconverted. A `/tmp/...` path
becomes `D:\tmp\...` and silently fails. Use `pwd -W` to get a Windows path for
any `-v` mount or `-f` file argument. This exact bug made a config override
silently not apply and produced two sessions' worth of wrong measurements.

### Verification that keeps finding real bugs

Run these before believing anything:

```bash
gofmt -l . && go build ./... && go vet -tags=integration ./...
go test -count=1 -race -tags=integration ./...     # 17 packages (internal/tickets is new)
golangci-lint run ./...                            # 2 known gosec issues, see above — not 0
./scripts/check_wiring.sh                          # 12/12 wired
npx playwright test                                # 45 browser tests
```

`-race` needs a C compiler on `PATH`; in this Windows/Git-Bash shell `gcc` was
not found (`CGO_ENABLED=1` without it fails outright), so this session's runs
were without `-race`. Whatever shell had `gcc` resolving before, use that one
if race detection matters for what you're changing.

`check_wiring.sh` exists because three components shipped complete, tested and
never called. It is a grep, not a test, and it caught what the suite could not.

### The pattern that produced most findings this session

Nine defects were found by **running** something, not reading it. Several
passed code review and failed execution:

- an endpoint served subscriber IPs unauthenticated while every auth test passed
- the NFR harness reported a 100% error rate that was a daemon which never started
- a TLS test reported a downgrade that never happened
- a Sentinel config override silently never applied
- three components were built, tested, and never wired

When a check could plausibly pass for the wrong reason, break it deliberately
and confirm it fails. Every negative control run this session found something.

---

## Where things are

| | |
|---|---|
| Task state | this file |
| DoD scorecard, all findings | `DOD_STATUS_REPORT.md` |
| Running and testing per persona | `TESTERS_MANUAL.md` |
| Release notes | `RELEASE_NOTES.md` |
| Subscriber portal | `internal/portalui` → `/ui` |
| Operations console | `internal/staffui` → `/staff` |
| Browser tests | `e2e/portal.spec.ts`, `e2e/staff.spec.ts` |
