# Document 10: Data Migration Plan (DMP)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** DMP
**Traces From:** [DBD](06_DBD_Database_Design.md) → [SRS](02_SRS_System_Requirements.md)
**Traces To:** [DXD](11_DXD_Developer_Setup.md)

---

## 10.1 Migration Overview

This document describes the process for migrating subscriber records, plan configurations, session histories, and wallet balances from the legacy Jaze ISP Manager (or equivalent flat schema) into the new BSS/OSS platform.

### Guiding Principles

- **Zero balance discrepancy:** Wallet balances must reconcile to the cent before cutover.
- **PII encrypted on entry:** No plaintext Aadhaar or PAN enters the target database.
- **Idempotent pipeline:** The migration job can be re-run safely; re-processing an already-migrated record produces no side effects.
- **Dry-run mandatory:** A full staging migration must complete successfully before production cutover is authorized.

---

## 10.2 Migration Phases

### Phase 1: Staging Dry-Run

1. Spin up a full staging environment identical to production (see Doc 8).
2. Extract a full dump of the legacy database to the staging import volume.
3. Execute the migration pipeline against staging.
4. Run all validation and reconciliation checks (see § 10.5).
5. Sign off on staging results before proceeding.
6. Document any anomalies and their resolutions.

### Phase 2: Production Pre-Migration Preparation

1. Place the legacy system in read-only mode (disable new account creation and wallet top-ups).
2. Perform a final export of the legacy database.
3. Confirm the production BSS/OSS database schema migrations have been applied (`goose up`).
4. Pre-warm Redis caches for existing active subscribers.

### Phase 3: Production Migration Execution

1. Execute the migration pipeline against production.
2. Run all validation checks (see § 10.5).
3. If reconciliation passes: release DNS / routing to the new system.
4. If reconciliation fails: execute rollback procedure (see § 10.6).

### Phase 4: Post-Cutover Validation

1. Allow NOC and billing teams to perform spot-checks over 24 hours.
2. Monitor RADIUS authentication success rates against pre-migration baseline.
3. Confirm Asynq dead-letter queue remains empty.
4. Archive the legacy system snapshot; keep read-only access available for 30 days.

---

## 10.3 Legacy Data Extraction

```bash
# Export legacy MySQL/PostgreSQL to CSV staging files
mysqldump --no-tablespaces legacy_db subscribers > /tmp/staging/subscribers.csv
mysqldump --no-tablespaces legacy_db plans > /tmp/staging/plans.csv
mysqldump --no-tablespaces legacy_db sessions > /tmp/staging/sessions.csv
mysqldump --no-tablespaces legacy_db payments > /tmp/staging/payments.csv

# Or for flat CSV exports from Jaze:
# Place exported CSV files in /tmp/staging/ and proceed to pipeline
```

---

## 10.4 Transformation & Scrubbing Pipeline

The Go-based migration binary (`cmd/migrate/main.go`) processes records sequentially with the following transformations:

### Subscriber Records

| Field | Transformation |
|---|---|
| `password` | Pass through `bcrypt.GenerateFromPassword(cost=12)` |
| `mobile_number` | Normalize to E.164: strip leading 0, prepend `+91` if missing; reject non-10-digit values |
| `aadhaar` | Pass plaintext through `AESEncryptor.Encrypt()`; store versioned ciphertext |
| `pan` | Pass plaintext through `AESEncryptor.Encrypt()`; store versioned ciphertext |
| `status` | Map legacy status codes to new enum: `1=active`, `0=suspended`, `2=terminated` |
| `registered_state` | Derive from legacy address field or default to `TN` if absent |
| `dnd_opt_out` | Map from legacy DND column; default `FALSE` if absent |

### Plan Records

| Field | Transformation |
|---|---|
| `fup_threshold_bytes` | Convert GB value: `volume_gb * 1024^3` |
| `price` | Assert `NUMERIC(12,2)` precision; reject if non-numeric |

### Session History

- Import only sessions with a valid `start_time`. Sessions missing `stop_time` are treated as terminated with `stop_time = migration_run_timestamp`.
- Legacy session IDs are preserved in `session_id` for audit continuity.

### Wallet / Balance Records

- Each legacy wallet balance imports as a single opening credit entry in `wallet_ledgers` with `description = 'Opening balance — legacy migration'`.
- The `transaction_token` is set to `legacy_migration:{legacy_subscriber_id}` to ensure idempotency on re-run.

---

## 10.5 Reconciliation & Validation

### Pre-Cutover Checklist

```
□ Total subscriber row count (new) == Total active subscriber count (legacy)
□ Sum(wallet_balance) across all subscribers matches legacy system total
□ Per-subscriber wallet balance spot-check (sample 100 random accounts)
□ Plan count matches legacy plan count
□ Zero plaintext PII in kyc_verifications (automated scan)
□ Zero records with NULL key_version_id in kyc_verifications
□ RADIUS authentication test: radtest on 10 migrated accounts → all accept
□ Invoice generation test: generate PDF for 3 migrated accounts → renders correctly
□ Asynq dead-letter queue depth = 0
```

### Balance Reconciliation Query

```sql
-- Must return zero rows. Any row indicates a balance mismatch requiring investigation.
SELECT
    n.id,
    n.username,
    n.wallet_balance AS new_system_balance,
    l.legacy_balance,
    ABS(n.wallet_balance - l.legacy_balance) AS variance
FROM subscribers n
JOIN migration_legacy_balance_staging l ON n.caf_number = l.caf_number
WHERE ABS(n.wallet_balance - l.legacy_balance) > 0.01;
```

If any variance is detected, the migration pipeline pauses and logs an alert. Cutover must not proceed until all variances are resolved or explicitly approved with justification.

---

## 10.6 Rollback Procedure

If production migration fails reconciliation checks:

1. Halt migration pipeline immediately.
2. Do not switch DNS / routing to the new system.
3. Restore the legacy system from read-only mode (re-enable writes).
4. Notify all stakeholders of the rollback.
5. Truncate all target tables in the new database (`TRUNCATE ... CASCADE`).
6. Investigate failure root cause from migration logs.
7. Fix pipeline, re-run staging validation, then re-schedule production cutover.

**Legacy system snapshot** must remain available and untouched until production cutover is confirmed successful and the 30-day legacy read-only period has elapsed.

---

## 10.7 Post-Migration Password Verification

Since bcrypt hashes cannot be validated without the original plaintext, migrated passwords must be verified on first login:

- On first successful RADIUS authentication post-migration, verify the supplied password against the stored bcrypt hash.
- If verification fails (legacy password was stored in a non-bcrypt format), prompt the subscriber to reset their password via SMS OTP.
- Log first-login verification outcomes for the first 7 days post-migration.
