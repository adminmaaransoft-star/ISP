# Handoff — state of play

Rewritten 2026-08-12. The previous version described a moment that has since
passed and had gone stale in ways that would mislead a fresh session (it
still listed the already-shipped FR-NOTIF-007 as the next task, and quoted
package/wiring counts that no longer matched). Re-derive the numbers below
rather than trusting them if this file is more than a few sessions old —
`go test ./...`, `check_wiring.sh` and `git log` are the truth, this file is
a convenience.

---

## Where the project is

`v1.0.0` was cut, then ten commits landed on top of it. The work since falls
into two groups:

**Shipped and verified** (all pushed to `origin/main`):

| Feature | FR | Verification |
|---|---|---|
| Dunning scanner, nightly revenue reconciliation, operations console | FR-BIL-004, FR-REV-*, FR-SEC-005 | 45 Playwright tests across 5 staff personas |
| Ticket status-change notifications | FR-NOTIF-007 | Live: console click → `notification_log` row in 1s |
| Multi-vendor NAS attribute engine | FR-NAS-001..004 | 20 unit tests, byte-exact VSA encoding per vendor |
| PostgreSQL HA (Patroni + etcd) + connection-pool failover fix | NFR-AVAIL-002 | Live: 3-node cluster, 3 failover modes drilled |

**Designed, not built** — the Jaze-parity roadmap (CRD §1.11) adopted as
BO-007: 40 FRs across 11 groups, phased. Phase 2 (network correctness) is
done. Phase 3 (operations-suite parity) has one module designed:

- **Helpdesk & SLA Engine** (FR-SUP-001..003) — MDS §4.13 + DBD §6.2 are
  written and specific enough to build from. Migration would be
  `023_create_sla_engine.sql`. **This is the obvious next thing to build.**

Everything else in Phases 3–5 is requirements-stage only (SRS §2.1), with no
module design yet. Each gets its own design pass when scheduled — deliberately
not all at once.

---

## Current numbers (verified 2026-08-12)

| | |
|---|---|
| Go packages | 26 total; **18 have tests and pass** |
| Wiring check | **13/13** components have a production caller |
| Browser tests | 45 (Playwright, 5 staff personas + subscriber portal) |
| Migrations | 22 applied (`001`–`022`) |
| Lint | 2 known pre-existing `gosec` findings, untriaged (see below) |

---

## Open decisions that need a person, not a sprint

1. **L6-004 Sentinel failover target.** Measured 5055ms against a 3000ms
   budget; ~3.1s with `down-after-milliseconds` at 500. Recommendation:
   adopt 500 and move the target to 8s. **Separately**, decide whether the
   DNS/tilt failure mode is acceptable — when a master container leaves
   Docker DNS, Sentinel never promotes at all. Reproduce with
   `CHAOS_MODE=kill ./scripts/run_sentinel_failover_test.sh`. See
   `DOD_STATUS_REPORT.md` Finding #6.

2. **Two pre-existing `gosec` findings**, present since before the current
   round of work and confirmed not introduced by it (verified via
   `git stash`): G705 in `internal/api/invoices.go:222`, G710 in
   `internal/staffui/screens.go`. Both need a real triage — suppress with a
   reason, or fix. Note that earlier docs claiming "lint: 0 issues" were
   wrong.

3. **`staff_users` has no MFA, no lockout, no password rotation, and no
   account-creation screen** (accounts come from the seed). Migration 021
   added a new authentication surface and none of this was ever specified,
   so these are open decisions rather than omissions.

---

## Known gaps worth knowing about

- **`scripts/seed_local.sql` publishes demo credentials.** Bcrypt hashes for
  five staff accounts, with the password named in `TESTERS_MANUAL.md`
  (`staffpassword`). Same category as the existing `testpassword` subscriber
  hashes. Fine for a localhost demo, but this is a public repo — those
  accounts must never exist in a real deployment.
- **The franchise/LCO module is built but unreachable.**
  `internal/revenue/franchise.go` has commission calculation, franchise-scoped
  listing, and `franchise_admin`/`franchise_staff` roles — and zero routes.
  No `/api/v1/franchises` endpoint is registered anywhere. FR-FRN-004..006
  (SRS) covers wiring it up.
- **The 1-hour soak (L6-005) has never been run.**
  `./scripts/run_soak_test.sh` is committed and validated on a 90s run.
  Needs one uninterrupted hour on a machine that will not sleep.
- **`assigned_to` on `tickets` has no FK** — migration 009's own comment
  promises "FK to admin_users.id added in future migration"; that migration
  was never written and `admin_users` never existed. The real staff table is
  `staff_users` (migration 021). The SLA engine design (DBD §6.2) adds the
  FK that should have been there.

---

## Things a fresh session will not know

### Running anything

```bash
./scripts/demo_up.sh                     # full stack; generates .env secrets
export COMPOSE_PROJECT_NAME=isp_bss_demo # demo_up.sh hardcodes this
```

Without that variable every `docker compose` command silently targets a
different project and reports containers as not running.

### PostgreSQL HA is opt-in, not the default

The base `docker-compose.yml` runs one Postgres, same as it always has.
The Patroni/etcd HA topology is an overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml up -d
```

`demo_up.sh` and the whole test suite run against the single-node stack.
Note that `docker-compose.pg-ha.yml` uses fixed `container_name` values, so
it cannot run side-by-side with the demo stack on one machine without a
name/port override.

### Docker on this machine

- Docker Desktop drops out mid-session. Recover:
  `powershell -Command "Start-Process 'C:\Program Files\Docker\Docker\Docker Desktop.exe'"`
  then poll `until docker info; do sleep 3; done`.
- Registry pulls have failed with certificate errors when the host clock
  drifts (the cert reads as not-yet-valid). Cached images still work; do not
  "fix" it by changing the system clock.

### Paths and Docker

`MSYS_NO_PATHCONV=1` is required for most `docker run` arguments, but it
means POSIX paths reach the Windows Docker binary unconverted. A `/tmp/...`
path becomes `D:\tmp\...` and silently fails. Use `pwd -W` for any `-v` mount
or `-f` file argument. This exact bug produced two sessions' worth of wrong
measurements.

### Line endings

This repo has `core.autocrlf=true`. Any `git stash`/`pop` round-trip
re-CRLFs the files it touches, which makes `gofmt -l` flag entire files as
unformatted even though nothing semantic changed. Fix with
`sed -i 's/\r$//' <file>`; do not change the git config.

### The demo stack's seeded Redis session expires

`demo_up.sh` seeds `session:active:{id}` with a 24h TTL. A stack left running
longer than that fails the two Playwright tests that check the "online"
dashboard state, for reasons unrelated to whatever you just changed. Reseed
with the same `redis-cli SET ... EX 86400` command from `demo_up.sh`.

### `-race` needs a C compiler

`go test -race` requires cgo. In the Git-Bash shell used for recent sessions
`gcc` was not on `PATH`, so those runs were without `-race`. Use a shell
where `gcc` resolves if race detection matters for what you are changing.

---

## Verification that keeps finding real bugs

```bash
gofmt -l . && go build ./... && go vet -tags=integration ./...
go test -count=1 -tags=integration ./...   # 18 packages with tests
golangci-lint run ./...                    # 2 known pre-existing issues
./scripts/check_wiring.sh                  # 13/13 wired
npx playwright test                        # 45 browser tests
```

`check_wiring.sh` exists because three components once shipped complete,
tested, and never called. It is a grep, not a test, and it caught what the
suite could not.

### The pattern that keeps working

**Run it, don't read it.** Defects that passed code review and failed
execution, across several sessions:

- an endpoint served subscriber IPs unauthenticated while every auth test passed
- the NFR harness reported a 100% error rate that was a daemon which never started
- a TLS test reported a downgrade that never happened
- a Sentinel config override silently never applied
- three components were built, tested, and never wired
- Patroni's bootstrap silently did not create the application database
  (found by running migrations against a real cluster, not by reading docs)

When a check could plausibly pass for the wrong reason, break it
deliberately and confirm it fails. Every negative control run so far has
found something.

---

## Where things are

| | |
|---|---|
| Roadmap and phasing | `specification_docs_v2/01_CRD_...` §1.11 |
| All 92 FRs | `specification_docs_v2/02_SRS_...` |
| Module designs | `specification_docs_v2/04_MDS_...` (§4.11 NAS, §4.12 PG HA, §4.13 SLA) |
| Schema | `specification_docs_v2/06_DBD_...` |
| Infrastructure / HA topology | `specification_docs_v2/08_IDD_...` §8.2a |
| Incident runbook | `specification_docs_v2/12_OPS_...` |
| DoD scorecard, findings | `DOD_STATUS_REPORT.md` |
| Running/testing per persona | `TESTERS_MANUAL.md` |
| Subscriber portal | `internal/portalui` → `/ui` |
| Operations console | `internal/staffui` → `/staff` |
| Browser tests | `e2e/portal.spec.ts`, `e2e/staff.spec.ts` |
