# Document 12: Operations & Incident Response Runbook
**Version:** 2.1 | **Status:** Draft | **Date:** 2026-08-12 — §12.3.2 rewritten for automated Patroni failover (NFR-AVAIL-002); rest unchanged from v2.0
**Document ID:** OPS
**Traces From:** [IDD](08_IDD_Infrastructure_Design.md) → [SAD](03_SAD_System_Architecture.md)
**Traces To:** —

---

## 12.1 On-Call Escalation Matrix

| Level | Role | Contact | Response SLA |
|---|---|---|---|
| L1 | NOC Engineer (on-call) | PagerDuty primary rotation | 5 minutes |
| L2 | Senior NOC / Network Lead | PagerDuty escalation | 15 minutes |
| L3 | Platform Engineering | Direct call / Slack `#oncall-eng` | 30 minutes |
| L4 | Engineering Lead | Direct call | 60 minutes (severity 1 only) |

### Incident Severity Definitions

| Severity | Definition | Example |
|---|---|---|
| S1 | Full platform outage; all subscribers disconnected | PostgreSQL primary down, Redis primary down |
| S2 | Partial outage; subset of subscribers affected | NAS connectivity loss, FUP CoA failing |
| S3 | Degraded performance; no subscriber impact | High RADIUS latency, Asynq queue depth growing |
| S4 | Minor issue; logged but no immediate action | Single webhook HMAC failure, one failed invoice PDF |

---

## 12.2 Routine Operational Procedures

### 12.2.1 Manually Disconnect a Subscriber (PoD)

Use when a subscriber must be force-disconnected (abuse, non-payment override, testing).

```bash
# Via API (preferred — audited)
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/disconnect \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}"
# Returns 202 Accepted; PoD is asynchronous

# Verify task was enqueued
redis-cli -h bss_redis_primary LLEN asynq:{bss}:network_commands
```

If the Asynq task fails (check dead-letter queue):

```bash
# Check dead-letter queue
redis-cli -h bss_redis_primary ZCARD asynq:{bss}:dead

# View dead-letter tasks
asynq task ls --queue network_commands --state archived
```

### 12.2.2 Manually Apply / Remove FUP Throttle

Use when FUP was incorrectly applied or needs to be manually enforced.

```bash
# Apply FUP throttle
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/fup-override \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"action": "apply"}'

# Remove FUP throttle (restore full speed)
curl -X POST https://api.yourdomain.com/api/v1/sessions/{session_id}/fup-override \
  -H "Authorization: Bearer {NOC_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"action": "remove"}'
```

### 12.2.3 Emergency FUP Policy Rollback Across All Live Sessions

Use when a plan configuration error has incorrectly triggered FUP throttling across a subscriber cohort.

```bash
# Step 1: Identify affected sessions (Redis scan)
redis-cli -h bss_redis_primary --scan --pattern "session:*" \
  | xargs -I{} redis-cli -h bss_redis_primary HGET {} fup_applied \
  | grep -c "1"
# This gives count of FUP-applied sessions

# Step 2: Enqueue CoA restore for all affected sessions via bulk script
go run scripts/fup_rollback.go --plan-id {PLAN_ID} --action remove

# Step 3: Monitor CoA ACK rate on Grafana dashboard
# Dashboard: "RADIUS / Network Commands" → CoA ACK rate should spike then normalize
```

### 12.2.4 Extend Grace Period for a Subscriber

```bash
# Update plan_expiry directly via API
curl -X PATCH https://api.yourdomain.com/api/v1/subscribers/{id} \
  -H "Authorization: Bearer {BILLING_JWT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"plan_expiry": "2025-02-15T23:59:59Z"}'
```

---

## 12.3 Infrastructure Incident Procedures

### 12.3.1 Redis Primary Failure

**Symptoms:** RADIUS auth latency spike; Grafana `redis_up` = 0 for primary node.

**Response:**

1. Check Sentinel election status:
   ```bash
   redis-cli -p 26379 -h bss_redis_sentinel_1 sentinel master bss_master
   # Verify 'ip' field shows the new primary
   ```
2. Sentinel should auto-promote a replica within 3 seconds. Verify new primary is accepting writes:
   ```bash
   redis-cli -h {NEW_PRIMARY_IP} set test_key test_val
   redis-cli -h {NEW_PRIMARY_IP} get test_key
   ```
3. Restart failed Redis node container. It will rejoin as a replica:
   ```bash
   docker-compose restart redis_primary
   ```
4. Verify replication status:
   ```bash
   redis-cli -h bss_redis_primary INFO replication
   ```
5. Update monitoring alert to confirm `redis_up` returns to 1 for all nodes.

**Escalate to L3 if:** Sentinel fails to elect a new primary within 30 seconds.

### 12.3.2 PostgreSQL Primary Failure

**Symptoms:** API 500 errors; Grafana `pg_up` = 0.

**This procedure assumes `docker-compose.pg-ha.yml` is deployed (IDD §8.2a,
NFR-AVAIL-002).** A deployment running only the base `docker-compose.yml`
has one Postgres container with no automated failover at all — restart is
the only lever, and an unrecoverable primary there means restore-from-backup
(§12.4), not promotion. Confirm which topology is actually running
(`docker compose ps | grep postgres`; three `postgres_*` containers means
the overlay is applied) before assuming automation exists.

**With the pg-ha overlay — automated path (try first):**

Patroni promotes a synchronous standby automatically, typically within
`ttl` (30s, `config/postgres/patroni.yml`) of the primary becoming
unreachable — no operator action needed for the promotion itself.

1. Confirm a promotion already happened rather than assuming one is needed:
   ```bash
   curl -s bss_postgres_standby_1:8008/primary   # 200 = this node is now primary
   curl -s bss_postgres_standby_2:8008/primary
   ```
2. If a standby has already been promoted, the applications need no DSN
   change — `DB_DSN` already lists all three hosts with
   `target_session_attrs=read-write` (IDD §8.2a), so `pgx` finds the new
   primary on its own for any *new* connection. Watch for a burst of
   `db_connection_retry_total` / `nas_...` — actually watch
   `radius_auth_duration_seconds` and API error rate — as pooled
   connections still pointed at the old primary hit SQLSTATE `25006` and
   get recycled; this should self-resolve within one pool cycle, not
   require a restart.
3. If self-resolution is not visible within a few minutes, restart the
   application containers to force a clean pool:
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml \
     up -d --force-recreate aaa_core_daemon api_service
   ```
4. Once the failed node recovers, Patroni rejoins it as a standby
   automatically (`use_pg_rewind: true`) — no manual `pg_basebackup` needed
   in the common case.

**Escalate to manual promotion if:** Patroni has not promoted anything
within 2 minutes (check `docker logs bss_postgres_primary` and
`bss_postgres_standby_1/2` for etcd connectivity — a lost etcd quorum, not
just a lost primary, is the failure mode that leaves Patroni unable to act).

**Manual fallback (automation itself is degraded, or no HA overlay is
deployed with a replica you provisioned by hand):**

```bash
# On the healthiest standby
docker exec bss_postgres_standby_1 patroni ctl -c /etc/patroni/patroni.yml \
  failover --candidate postgres_standby_1 --force
# or, if Patroni itself is unreachable, the raw promotion it would have run:
docker exec bss_postgres_standby_1 psql -U postgres -c "SELECT pg_promote();"
```

Update `DB_DSN` only if the manual `pg_promote()` path was used (Patroni's
own failover does not require a DSN change — see step 2 above).

**RPO/RTO:** RPO ≈ 0 in the common case (`synchronous_mode: true`), degrading
to seconds of possible loss if the synchronous standby was already
unreachable before the primary failed (`synchronous_mode_strict: false` —
IDD §8.2a explains why that trade-off is deliberate: a single dead standby
must not stall every write on an otherwise-healthy primary). Target RTO: under
1 minute for the automated Patroni path; 15 minutes for the manual fallback,
unchanged from before this section existed.

### 12.3.3 Asynq Dead-Letter Queue Non-Empty

**Symptoms:** PagerDuty alert `dead_letter_queue_non_empty`; Grafana `dead_letter_queue_depth` > 0.

**Response:**

1. Identify dead-letter tasks:
   ```bash
   asynq task ls --queue network_commands --state archived | head -20
   ```
2. Check task payload and error message to determine root cause (NAS unreachable, malformed packet).
3. If NAS is reachable: re-run dead-letter tasks:
   ```bash
   asynq task run --queue network_commands --state archived --all
   ```
4. If NAS is unreachable (CCR2004 down): do not retry. Escalate to network team. Affected subscribers will remain in their current state until NAS recovers.
5. Once NAS recovers, re-run all dead-letter tasks.
6. Archive tasks that cannot be retried with a documented reason.

### 12.3.4 High RADIUS Authentication Latency

**Symptoms:** Grafana `radius_auth_duration_seconds` p99 > 15 ms.

**Response:**

1. Check Redis response time:
   ```bash
   redis-cli -h bss_redis_primary --latency-history
   ```
2. Check RADIUS worker queue depth:
   ```bash
   # Prometheus metric: radius_worker_queue_depth
   # If > 100: worker pool may be exhausted — check for slow DB fallback queries
   ```
3. Check PostgreSQL for slow queries:
   ```sql
   SELECT query, mean_exec_time, calls
   FROM pg_stat_statements
   ORDER BY mean_exec_time DESC
   LIMIT 10;
   ```
4. If DB fallback queries are causing latency: pre-warm Redis cache for sessions without active cache entries:
   ```bash
   go run scripts/cache_warmup.go
   ```

---

## 12.4 Backup & Restore Procedures

### PostgreSQL Point-in-Time Restore

```bash
# Stop the application services
docker-compose stop aaa_core_daemon api_service

# Restore from WAL archive to a specific point in time
# (Example using pg_basebackup + WAL replay — adjust to your archiving setup)
pg_restore --jobs=4 -h localhost -U postgres -d isp_bss_oss /backup/pgdump/20250101.dump

# Restart services
docker-compose start aaa_core_daemon api_service

# Validate: row counts, wallet balance sum
psql -h localhost -U postgres -d isp_bss_oss \
  -c "SELECT COUNT(*) FROM subscribers WHERE status = 'active';"
```

### Redis Restore from RDB

```bash
# Stop Redis primary
docker-compose stop redis_primary

# Copy backup RDB into data volume
docker cp /backup/redis/20250101.rdb bss_redis_primary:/data/dump.rdb

# Restart Redis
docker-compose start redis_primary

# Verify key count
redis-cli -h bss_redis_primary DBSIZE
```

---

## 12.5 Scheduled Maintenance Checklist (Monthly)

```
□ Review and rotate RADIUS shared secrets on all NAS devices
□ Verify PII re-encryption Asynq job completed successfully (check encryption_keys table)
□ Review dead-letter queue archive — any recurring failures indicate code or infra issues
□ Test PostgreSQL replica promotion procedure in staging
□ Confirm TLS certificate expiry dates (auto-renewal should trigger 30d before expiry)
□ Review Prometheus alert firing history — tune thresholds if noisy
□ Run database VACUUM ANALYZE on high-write tables
□ Rotate JWT signing secret (coordinate with all API consumers)
□ Review and purge Asynq task history older than 30 days
```

---

## 12.6 Key Dashboard URLs

| Dashboard | URL |
|---|---|
| RADIUS Performance | `https://grafana.internal/d/radius` |
| Network Commands (CoA/PoD) | `https://grafana.internal/d/network_cmds` |
| Billing & Wallet | `https://grafana.internal/d/billing` |
| Infrastructure Health | `https://grafana.internal/d/infra` |
| Asynq Queue Depths | `https://grafana.internal/d/asynq` |
