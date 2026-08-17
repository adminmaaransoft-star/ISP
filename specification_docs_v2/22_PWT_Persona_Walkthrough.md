# Persona Walkthrough — what each role actually sees

An illustrated tour of the six personas from CRD §2, showing what each one sees
when they sign in and — just as importantly — what they are prevented from
seeing. Companion to `TESTERS_MANUAL.md`, which is the terse command reference;
this document is the visual one.

Written 2026-08-17 against commit `9871599`, captured from a running stack.

---

## About the images

Every screenshot referenced below lives in `e2e/snapshots/`, written by the
Playwright suite against the live demo stack:

```bash
./scripts/demo_up.sh
npx playwright test
```

**That directory is deliberately git-ignored** (`.gitignore:42` — "Playwright
snapshots are regenerated on every run"), so the image links in this file
resolve only after you have run the suite locally. That is the intended
trade-off: the images can never drift from what the software currently does,
because they are rebuilt from the running system rather than version-controlled
alongside it.

A fully self-contained version with the images embedded is published as an
artifact; ask whoever ran this session for the link if you need one that works
without a local stack.

---

## Signing in

| Who | Username | Password | URL |
|---|---|---|---|
| Five staff personas | `owner`, `noc`, `billing`, `csr`, `tech` | `staffpassword` | `https://localhost/staff/login` |
| Subscriber | `test_user`, `suspended_user` | `testpassword` | `https://localhost/ui/login` |

> These passwords are published in this repository and must never exist in a
> real deployment.

All five staff personas share one sign-in screen, and it never says which field
was wrong on failure — naming it would confirm whether a username exists,
turning the form into an account enumerator.

![Operations console sign-in](../e2e/snapshots/login.png)

---

## PER-001 — ISP Owner (`isp_owner`)

The only persona with the full run of the console: Subscribers, Billing,
Support, Revenue, and LEA Lookup.

![Owner navigation, all five sections](../e2e/snapshots/nav-owner.png)

Revenue is unique to this persona — three figures read live, with the same
numbers written to a nightly snapshot at 02:00 IST. A non-zero ledger variance
means money moved without a matching double-entry counter-entry and should be
investigated rather than dismissed.

![Revenue dashboard](../e2e/snapshots/revenue.png)

---

## PER-002 — NOC Engineer (`noc_engineer`)

Network operations: Subscribers and LEA Lookup, and notably *not* Billing or
Revenue.

![NOC navigation, two sections](../e2e/snapshots/nav-noc.png)

LEA lookup resolves which subscriber held an IP address at a given time. The
form states plainly that every lookup is recorded.

![LEA lookup form](../e2e/snapshots/lea-form.png)

> **A role is not enough here.** LEA access requires the `lea_access` claim *in
> addition to* the NOC role — it can never be a side effect of a role
> assignment. Without the claim the API answers 403 even for an owner.

---

## PER-003 — Billing / Finance Admin (`billing_admin`)

Subscribers and Billing. No Support queue, no Revenue dashboard, no LEA.

![Billing admin navigation](../e2e/snapshots/nav-billing.png)

---

## PER-004 — CSR (`csr`)

The customer-facing desk: Subscribers, Billing, and Support — everything needed
to answer a call without switching systems.

![CSR navigation](../e2e/snapshots/nav-csr.png)

---

## PER-005 — Ground Technician (`technician`)

Subscribers and Support only. A technician needs to know whether a line is up
and what the customer reported — not what they owe.

![Technician navigation](../e2e/snapshots/nav-tech.png)

### The same subscriber, two personas

The clearest demonstration that the restriction is real rather than cosmetic.
Both images are the same customer record, opened seconds apart, by two
different staff accounts.

As CSR — wallet balance and ledger present:

![Subscriber detail as CSR](../e2e/snapshots/subscriber-detail-csr.png)

As technician — the money panels are simply absent:

![Subscriber detail as technician](../e2e/snapshots/subscriber-detail-tech.png)

> **Hiding a link is not the control.** The console hides what a role cannot
> use, but the API refuses it independently. Typing the URL directly returns 403
> rather than the data.

---

## PER-006 — End Subscriber (the portal)

The only persona who is not staff. They sign in at `/ui` and see their own
account and nothing else.

![Subscriber portal sign-in](../e2e/snapshots/portal-login.png)

### Active and suspended, side by side

Worth seeing together, because the suspended state is what a customer sees when
collections has cut them off — and it is the screen a CSR will be asked about.

Active — ₹799 balance, 2,235 GB of 3,300 GB used, full speed:

![Active subscriber dashboard](../e2e/snapshots/portal-dashboard.png)

Hard-suspended — ₹0.00, and no session at all:

![Suspended subscriber dashboard](../e2e/snapshots/portal-suspended.png)

### The rest of the portal

![Usage history](../e2e/snapshots/portal-usage.png)

![Invoices with GST breakdown](../e2e/snapshots/portal-invoices.png)

Plus Renew (one-tap, paid from the wallet), Support (raise and track tickets),
and Notifications (delivery history of messages sent to them).

> **If the usage panel says "offline"**, the demo's Redis session record has
> passed its 24-hour TTL. The panel is correctly reporting no active session;
> the software has not broken. Two of the 45 browser tests check that panel and
> fail for the same reason.

---

## The seventh surface — walk-up Wi-Fi

Not a persona in the specification, but a real person: someone who has just
associated with a hotspot and has no account yet.

![Captive portal sign-in](../e2e/snapshots/hotspot-signin.png)

Two details in that screen are deliberate. The MAC is normalised and echoed
back — `aa-bb-cc-dd-ee-ff` was typed into the URL, `AA:BB:CC:DD:EE:FF` is what
the system will actually authorise — and both routes onto the network are
offered together: prepaid voucher, or an existing subscriber account.

> **The portal cannot let anyone online by itself.** Redeeming a voucher writes
> a grant; the NAS still authenticates the device over RADIUS afterwards. This
> is why voucher *issuance* lives behind staff authentication and only
> redemption is public.

---

## Access at a glance

| Persona | Role | Subscribers | Billing | Support | Revenue | LEA |
|---|---|:---:|:---:|:---:|:---:|:---:|
| ISP Owner | `isp_owner` | ✅ | ✅ | ✅ | ✅ | ✅ |
| NOC Engineer | `noc_engineer` | ✅ | — | — | — | ✅ |
| Billing Admin | `billing_admin` | ✅ | ✅ | — | — | — |
| CSR | `csr` | ✅ | ✅ | ✅ | — | — |
| Ground Technician | `technician` | ✅ | — | ✅ | — | — |
| End Subscriber | — (portal) | Own account only, at `/ui` | | | | |

LCO and franchise roles (`lco`, `franchise_admin`, `franchise_staff`) see the
same screens narrowed to their own franchise. That scoping comes from their
sign-in token, so it cannot be widened by editing a URL.
