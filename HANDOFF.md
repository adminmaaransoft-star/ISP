# Handoff — state of play

Written 2026-08-12 at commit `deb06d2`. Delete this file once the items below
are done; it describes a moment, not the system.

---

## Do this first

**Five commits are unpushed and the `v1.0.0` tag is five commits stale.**

`origin/main` is at `74e2655`. It does **not** have the dunning scanner, the
nightly revenue reconciliation, or the operations console. Anyone cloning today
gets a system where nobody is reminded to pay and staff have no UI.

```bash
git log --oneline origin/main..HEAD    # 5 commits
git push origin main
git tag -a v1.1.0 -m "..." && git push origin v1.1.0
```

`v1.0.0` should stay where it is — it is a released tag. The work since is a
minor release, not a re-cut.

---

## Next tasks, in priority order

### 1. FR-NOTIF-007 — notify on ticket status change (~half a day)

The console lets a CSR change a ticket status. `FR-NOTIF-007` requires telling
the subscriber. `TMPL-008` (`ticket_update`) is seeded. **Nothing dispatches
it** — `db.TicketStore.UpdateTicketAdmin` is a bare `UPDATE`.

Harmless when only an API call could change a status; now a CSR resolves a
ticket from the console and the customer is never told. That is the
"embarrassed in front of a subscriber" problem CRD PER-004 names for the role.

Follow the pattern already in the tree: `internal/billing/dunning_task.go`
(handler) + enqueue from the console's `UpdateTicketStatus`, registered on the
worker mux in `cmd/radiusd/main.go`.

### 2. Console write actions (~1 week)

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

### 3. Security review of `staff_users` (~2 days + a decision)

Migration 021 added a **new authentication surface**. Not specified anywhere,
so these are open decisions rather than omissions:

- no MFA
- no password rotation or expiry
- no lockout after repeated failures
- no screen for creating accounts (they come from the seed)
- 8-hour sessions

### 4. L6-004 Sentinel failover — a decision, not a sprint

Measured 5055ms on the shipped config against a 3000ms budget; ~3.1s with
`down-after-milliseconds` at 500. Recommendation: adopt 500 and move the target
to 8s. **Separately**, decide whether the DNS/tilt failure mode is acceptable —
when a master container leaves Docker DNS, Sentinel never promotes at all.
Reproduce: `CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh`.
See `DOD_STATUS_REPORT.md` Finding #6.

### 5. The 1-hour soak (L6-005) — pure time

`./scripts/run_soak_test.sh` is committed and validated on a 90s run. Needs one
uninterrupted hour on a machine that will not sleep.

### 6. Remaining unimplemented requirements

39 of 52 FRs are traceable to a `TestFR_` test. Genuinely unimplemented:

- `FR-NOTIF-003` / `FR-FUP-005` — notify when FUP throttle is applied (same
  missing-trigger shape as #1)
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
go test -count=1 -race -tags=integration ./...     # 16 packages
golangci-lint run ./...                            # 0 issues
./scripts/check_wiring.sh                          # 11/11 wired
npx playwright test                                # 45 browser tests
```

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
