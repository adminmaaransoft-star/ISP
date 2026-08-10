# Tester's Manual

How to run the system, sign in as each persona, and exercise the parts that
matter. Every command here was run against the live stack before being written
down.

There is a visual version of the subscriber-portal walkthrough, with
screenshots, published separately; this file is the reference that ships with
the code.

---

## 1. Starting the system

```bash
./scripts/demo_up.sh
```

That brings up PostgreSQL, Redis with three Sentinels and two replicas,
Gotenberg, the RADIUS daemon, the API and the reverse proxy; applies all 20
migrations; generates any missing secrets into `.env`; and seeds demo data.

The portal is then at **`https://localhost/ui/login`**.

Your browser will warn that the connection is not private. That is expected —
the demo issues its own certificate rather than buying a public one. Choose
"Advanced" and continue. Use `curl -k` for the same reason.

To stop: `./scripts/demo_up.sh down` keeps the data, `down -v` wipes it.

### What the seed gives you

| | |
|---|---|
| Subscribers | `test_user` (active), `suspended_user` (hard_suspended) |
| Password | `testpassword` for both |
| Plans | TN_Basic_50M, TN_Super_100M, TN_Ultra_200M |
| Invoices | 1 on `test_user` (₹942.82 = ₹799 + 18% GST) |
| Past sessions | 4, between 67 GB and 512 GB |
| Live session | 1, at 67% of the 3300 GB quota |

Usage sits at 67% deliberately — below the 80% mark that fires a FUP warning,
so testing does not generate notifications nobody asked for.

---

## 2. The six personas

The CRD defines six personas. **Only one of them has a screen.**

| ID | Persona | How they reach the system |
|---|---|---|
| PER-001 | ISP Owner | JSON API, role `isp_owner` |
| PER-002 | NOC Engineer | JSON API, role `noc_engineer` |
| PER-003 | Billing / Finance Admin | JSON API, role `billing_admin` |
| PER-004 | CSR | JSON API, role `csr` |
| PER-005 | Ground Technician | JSON API, role `technician` |
| PER-006 | End Subscriber | **Web portal** at `/ui` |

There is no admin or staff web interface, and none was specified — the CRD
scopes the web UI as subscriber self-service, and staff needs are specified as
APIs. If a stakeholder is expecting an admin console, that expectation needs
correcting before a demo, not during one.

### PER-006 — End Subscriber (the portal)

Browse to `https://localhost/ui/login` and sign in as `test_user` /
`testpassword`. Use `suspended_user` to see how a cut-off customer's account
appears.

Walkthrough: sign in → read the dashboard (balance, plan, status, live usage) →
Usage → Invoices → download a PDF → Renew → file a support ticket → check
Notifications → sign out. Then confirm that typing a portal address while
signed out returns you to the sign-in page.

### PER-001..005 — staff personas (API)

Mint a token, then call the API with it:

```bash
SECRET=$(grep '^JWT_SECRET=' .env | cut -d= -f2-)

TOKEN=$(go run ./scripts/gen_jwt -secret "$SECRET" -role noc_engineer -ttl 2h)

curl -sk -H "Authorization: Bearer $TOKEN" \
  https://localhost/api/v1/subscribers/1/health
```

Swap `-role` for the persona you want. Verified behaviour:

| `-role` | subscriber health | subscriber record | wallet ledger |
|---|---|---|---|
| `isp_owner` | 200 | 200 | 200 |
| `noc_engineer` | 200 | 200 | **403** |
| `billing_admin` | 200 | 200 | 200 |
| `csr` | 200 | 200 | 200 |
| `technician` | 200 | 200 | **403** |

**The 403s are correct, not failures.** NOC engineers and ground technicians
have no business reading billing ledgers, and a test that "fixes" them has
broken the authorisation model.

### LCO / franchise partners

```bash
go run ./scripts/gen_jwt -secret "$SECRET" -role franchise_admin -franchise 1
```

The `-franchise` claim scopes rows to that franchise — an LCO must not be able
to see another LCO's subscribers.

### `gen_jwt` flags

| Flag | Purpose |
|---|---|
| `-secret` | HMAC signing secret (required) — `JWT_SECRET` from `.env` |
| `-role` | Role claim; default `billing_admin` |
| `-subject` | Subject claim, recorded in audit logs; default `loadtest` |
| `-subscriber` | `subscriber_id` claim, for portal tokens |
| `-franchise` | `franchise_id` claim, for LCO tokens |
| `-lea` | Sets the `lea_access` claim — see below |
| `-ttl` | Token lifetime; default 1h |

---

## 3. Testing LEA lookups

`POST /api/v1/lea/lookup` answers "which subscriber held this IP at this
moment" for a law-enforcement request. It is the most access-sensitive
endpoint in the system and needs **two** independent grants:

1. a role of `noc_engineer` or `isp_owner`, **and**
2. the `lea_access` claim.

They are separate on purpose (SecD §9.3): LEA reach must never arrive as a side
effect of a role assignment made for some other reason. A `noc_engineer` token
without the claim is refused.

```bash
SECRET=$(grep '^JWT_SECRET=' .env | cut -d= -f2-)

# Without the claim — expect 403
NOLEA=$(go run ./scripts/gen_jwt -secret "$SECRET" -role noc_engineer)
curl -sk -X POST -H "Authorization: Bearer $NOLEA" -H "Content-Type: application/json" \
  -d '{"public_ip":"100.64.0.14","timestamp":"2026-08-09T18:00:00Z"}' \
  https://localhost/api/v1/lea/lookup
# → forbidden: lea_access claim required

# With the claim
LEA=$(go run ./scripts/gen_jwt -secret "$SECRET" -role noc_engineer -lea -subject "insp.rao")
curl -sk -X POST -H "Authorization: Bearer $LEA" -H "Content-Type: application/json" \
  -d '{"public_ip":"100.64.0.14","timestamp":"2026-08-09T18:00:00Z"}' \
  https://localhost/api/v1/lea/lookup
```

A resolving lookup returns the subscriber, their CAF number, mobile number,
registered state and the session that held the address:

```json
{"subscriber_id":1,"caf_number":"CAF-0001","username":"test_user",
 "mobile_number":"+919876543210","registered_state":"TN",
 "session_id":"demo-sess-4","session_start":"2026-08-09T16:03:22Z",
 "session_stop":"2026-08-09T23:03:22Z","source":"direct_ip"}
```

Pass `-subject` something identifying: it is recorded as the accessor.

### Which IPs resolve

The lookup searches **session history**, so the timestamp must fall inside a
session's window. The seeded sessions:

| IP | Window |
|---|---|
| `100.64.0.14` | most recent, ~7 hours, ending yesterday evening |
| `100.64.0.13` | ~2 hours, 3 days ago |
| `100.64.0.12` | ~3 hours, 4 days ago |
| `100.64.0.11` | ~9 hours, 6 days ago |

Get the exact windows with:

```bash
docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss \
  -c "SELECT assigned_ipv4, start_time, stop_time FROM subscriber_session_history ORDER BY start_time DESC;"
```

`100.64.0.7` is the *live* session, which is held in Redis and not yet in
history, so it returns 404. That is correct behaviour, not a bug.

### Verifying the audit trail

Every lookup is recorded **whether or not it found anything** — an attempted
query is as auditable as a successful one:

```bash
docker compose exec -T postgres_primary psql -U postgres -d isp_bss_oss \
  -c "SELECT id, accessor_identity, accessor_role, queried_public_ip,
             result_row_count, accessed_at FROM lea_audit_log ORDER BY id;"
```

A miss records `result_row_count = 0`; a hit records the subscriber id.

The table is append-only at the database level — the application's role is
neither its owner nor a superuser, so Row-Level Security genuinely applies and
an `UPDATE` is rejected rather than silently permitted. Worth testing directly
if you are validating the compliance story.

---

## 4. Known gaps

| Gap | Status |
|---|---|
| Renewal payments | **Blocked** — needs `RAZORPAY_KEY_ID` / `RAZORPAY_KEY_SECRET`. The screen works; the payment round trip cannot complete. Report as blocked, not failed. |
| Admin / staff UI | **Does not exist** and was never specified. Staff use the JSON API. |
| Sentinel failover | ~5.1s on the shipped config against a 3s budget. See `DOD_STATUS_REPORT.md` finding 6. |

---

## 5. Reporting findings

Three things make a report actionable: **which page or endpoint**, **what you
did**, and **what you saw instead of what this manual says to expect**. A
screenshot of the whole browser window, address bar included, usually answers
all three.

Mark anything involving payment as *blocked* rather than failed, so the same
known gap does not get raised repeatedly and bury real findings.
