# Document 8: Infrastructure Design Document (IDD)
**Version:** 2.1 | **Status:** Draft | **Date:** 2026-08-12 — §8.2a added (NFR-AVAIL-002, CRD §1.11 Phase 2); rest unchanged from v2.0
**Document ID:** IDD
**Traces From:** [SAD](03_SAD_System_Architecture.md)
**Traces To:** [DXD](11_DXD_Developer_Setup.md) → [OPS](12_OPS_Operations_Runbook.md)

---

## 8.1 Production Environment Topology

All services run as Docker containers managed by Docker Compose on Ubuntu 22.04 LTS nodes. The topology isolates the external network entry plane from the internal data tier via dedicated Docker networks.

```
Internet / NAS (CCR2004)
        │
        ├── UDP :1812/:1813  ──► aaa_core_daemon
        │
        └── HTTPS :443       ──► reverse_proxy (nginx/caddy)
                                        │
                                        └── api_service :8080

Internal network (bss_internal):
  aaa_core_daemon ──► redis_sentinel_primary
  api_service     ──► redis_sentinel_primary
  aaa_core_daemon ──► postgres_primary
  api_service     ──► postgres_primary
  api_service     ──► gotenberg_engine
```

---

## 8.2 Production Docker Compose Blueprint

```yaml
version: '3.8'

networks:
  bss_internal:
    driver: bridge

services:

  # ── PostgreSQL Primary ──────────────────────────────────────────────────────
  postgres_primary:
    image: postgres:15-alpine
    container_name: bss_postgres_primary
    environment:
      POSTGRES_DB: isp_bss_oss
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: ${DB_SECURE_PASSWORD}
    volumes:
      - pg_data_primary:/var/lib/postgresql/data
      - ./config/postgres/postgresql.conf:/etc/postgresql/postgresql.conf
      - ./config/postgres/pg_hba.conf:/etc/postgresql/pg_hba.conf
    ports:
      - "5432:5432"
    networks:
      - bss_internal
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres -d isp_bss_oss"]
      interval: 10s
      timeout: 5s
      retries: 5
    deploy:
      resources:
        limits:
          cpus: '2.0'
          memory: 4096M
    restart: always

  # ── Redis Sentinel Cluster (3-node) ────────────────────────────────────────
  # Primary
  redis_primary:
    image: redis:7-alpine
    container_name: bss_redis_primary
    command: redis-server /etc/redis/redis.conf
    volumes:
      - redis_primary_data:/data
      - ./config/redis/redis-primary.conf:/etc/redis/redis.conf
    networks:
      - bss_internal
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5
    deploy:
      resources:
        limits:
          cpus: '1.0'
          memory: 2048M
    restart: always

  # Replica 1
  redis_replica_1:
    image: redis:7-alpine
    container_name: bss_redis_replica_1
    command: redis-server /etc/redis/redis.conf
    volumes:
      - redis_replica_1_data:/data
      - ./config/redis/redis-replica.conf:/etc/redis/redis.conf
    networks:
      - bss_internal
    depends_on:
      - redis_primary
    restart: always

  # Replica 2
  redis_replica_2:
    image: redis:7-alpine
    container_name: bss_redis_replica_2
    command: redis-server /etc/redis/redis.conf
    volumes:
      - redis_replica_2_data:/data
      - ./config/redis/redis-replica.conf:/etc/redis/redis.conf
    networks:
      - bss_internal
    depends_on:
      - redis_primary
    restart: always

  # Sentinel 1
  redis_sentinel_1:
    image: redis:7-alpine
    container_name: bss_redis_sentinel_1
    command: redis-sentinel /etc/redis/sentinel.conf
    volumes:
      - ./config/redis/sentinel.conf:/etc/redis/sentinel.conf
    networks:
      - bss_internal
    depends_on:
      - redis_primary
      - redis_replica_1
      - redis_replica_2
    restart: always

  # Sentinel 2
  redis_sentinel_2:
    image: redis:7-alpine
    container_name: bss_redis_sentinel_2
    command: redis-sentinel /etc/redis/sentinel.conf
    volumes:
      - ./config/redis/sentinel.conf:/etc/redis/sentinel.conf
    networks:
      - bss_internal
    restart: always

  # Sentinel 3
  redis_sentinel_3:
    image: redis:7-alpine
    container_name: bss_redis_sentinel_3
    command: redis-sentinel /etc/redis/sentinel.conf
    volumes:
      - ./config/redis/sentinel.conf:/etc/redis/sentinel.conf
    networks:
      - bss_internal
    restart: always

  # ── Gotenberg PDF Engine ────────────────────────────────────────────────────
  gotenberg_engine:
    image: gotenberg/gotenberg:8
    container_name: bss_gotenberg_engine
    networks:
      - bss_internal
    deploy:
      resources:
        limits:
          cpus: '1.0'
          memory: 1024M
    restart: always

  # ── AAA Core Daemon ─────────────────────────────────────────────────────────
  aaa_core_daemon:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: bss_aaa_core_daemon
    depends_on:
      postgres_primary:
        condition: service_healthy
      redis_primary:
        condition: service_healthy
    ports:
      - "1812:1812/udp"
      - "1813:1813/udp"
      - "9100:9100"   # Prometheus metrics
    environment:
      - DB_DSN=host=postgres_primary user=postgres password=${DB_SECURE_PASSWORD} dbname=isp_bss_oss port=5432 sslmode=disable
      - REDIS_SENTINEL_ADDRS=bss_redis_sentinel_1:26379,bss_redis_sentinel_2:26379,bss_redis_sentinel_3:26379
      - REDIS_MASTER_NAME=bss_master
      - LOG_FORMAT=json
      - LOG_LEVEL=info
    networks:
      - bss_internal
    restart: always

  # ── API Service ─────────────────────────────────────────────────────────────
  api_service:
    build:
      context: .
      dockerfile: Dockerfile.api
    container_name: bss_api_service
    depends_on:
      postgres_primary:
        condition: service_healthy
      redis_primary:
        condition: service_healthy
    ports:
      - "8080:8080"
      - "9101:9101"   # Prometheus metrics
    environment:
      - DB_DSN=host=postgres_primary user=postgres password=${DB_SECURE_PASSWORD} dbname=isp_bss_oss port=5432 sslmode=disable
      - REDIS_SENTINEL_ADDRS=bss_redis_sentinel_1:26379,bss_redis_sentinel_2:26379,bss_redis_sentinel_3:26379
      - REDIS_MASTER_NAME=bss_master
      - JWT_SECRET=${JWT_SECRET}
      - RAZORPAY_WEBHOOK_SECRET=${RAZORPAY_WEBHOOK_SECRET}
      - GOTENBERG_URL=http://gotenberg_engine:3000
      - LOG_FORMAT=json
      - LOG_LEVEL=info
    networks:
      - bss_internal
    restart: always

volumes:
  pg_data_primary:
  redis_primary_data:
  redis_replica_1_data:
  redis_replica_2_data:
```

---

## 8.2a PostgreSQL High Availability *(new — NFR-AVAIL-002, v3)*

### Why this exists

Redis has had real HA since v2.0 of this document — 3 Sentinels, quorum 2,
tested failover (§8.3). PostgreSQL, where `subscribers`, `wallet_ledgers`,
and every billing record live, has been a single container this whole time.
OPS §12.3.2 already documents a promotion procedure that assumes a replica
(`bss_postgres_replica`) and claims RPO = 0 — neither the replica nor any
automation behind that claim has ever existed until this section. This
closes that gap rather than leaving the runbook describing a system that
was never built.

### Mechanism: Patroni + etcd, not repmgr or pg_auto_failover

**Patroni**, coordinating through a 3-node **etcd** cluster, was chosen over
the alternatives for reasons specific to this deployment, not in the
abstract:

- **repmgr** needs its own daemon (`repmgrd`) per node for automatic
  failover and is generally considered less battle-tested for *automatic*
  promotion than Patroni — repmgr's strength is manual/assisted failover,
  which OPS §12.3.2 already has a procedure for and which this section is
  explicitly trying to move beyond.
- **pg_auto_failover** avoids a separate DCS (it uses a monitor node
  instead of etcd/Consul), which is architecturally closer to how Redis
  Sentinel works here — a real point in its favor — but it sees
  meaningfully less production adoption and community troubleshooting
  material than Patroni, which matters for a small ops team debugging an
  incident at 3 a.m., not just for the failover mechanics themselves.
- **Patroni** is the most widely deployed, most actively maintained option,
  with the deepest community/StackOverflow surface for exactly the kind of
  incident OPS §12.3 is written for. The etcd dependency is real overhead
  (three more containers) but is the same shape of overhead this codebase
  already accepted for Redis Sentinel — three coordinator processes to get
  automatic leader election — so it is not a new category of complexity,
  just the Postgres-shaped version of one already running in production.

### Topology

```
                    ┌─────────────┐
                    │  etcd_1/2/3 │  (DCS — leader election, quorum 2)
                    └──────┬──────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
┌───────▼──────┐   ┌───────▼────────┐  ┌───────▼────────┐
│postgres_primary│  │postgres_standby_1│ │postgres_standby_2│
│  (Patroni +    │◄─┤ (Patroni +      │◄┤ (Patroni +      │
│   postgres)    │  │  postgres,      │ │  postgres,      │
│                │  │  streaming      │ │  streaming      │
│                │  │  replica)       │ │  replica)       │
└───────▲────────┘  └────────────────┘ └────────────────┘
        │
   aaa_core_daemon / api_service
   DB_DSN lists all three hosts +
   target_session_attrs=read-write —
   pgx (this codebase's driver) tries
   each in order and picks the one
   currently accepting writes. No
   HAProxy/PgBouncer layer: pgx's
   native multi-host support (verified
   against pgx v5's pgconn — the same
   libpq-compatible behavior as
   `host=a,b,c target_session_attrs=
   read-write`) makes a proxy
   unnecessary for this Go-only
   client population.
```

`postgres_primary`/`postgres_standby_N` are static service-name labels, the
same relationship `redis_primary`/`redis_replica_N` already have to actual
runtime role — after a failover, `postgres_standby_1` may be the real
primary. `curl postgres_primary:8008/primary` (Patroni's REST API, 200 only
on the current leader) is the source of truth for current role, not the
container name.

### Deployment files

| File | Purpose |
|---|---|
| `Dockerfile.postgres-ha` | `postgres:15-alpine` + Patroni, one image for all three nodes |
| `config/postgres/patroni.yml` | Identical on every node; `PATRONI_NAME`/`PATRONI_*_CONNECT_ADDRESS`/passwords come from environment, not separate YAML files — see the file's own header comment for why that's the correct pattern for Patroni specifically (unlike Redis Sentinel's genuinely asymmetric primary/replica configs) |
| `docker-compose.pg-ha.yml` | Overlay, not an edit to `docker-compose.yml` — apply with `docker compose -f docker-compose.yml -f docker-compose.pg-ha.yml up -d`. Local/demo use (`scripts/demo_up.sh`) is unaffected and keeps running a single Postgres |

### Commit-durability setting

`synchronous_mode: true` / `synchronous_mode_strict: false` in
`patroni.yml`: Patroni requires a synchronous standby when one is healthy,
but degrades to async rather than stalling every write if that standby
becomes unreachable — a single dead standby must not turn into a full write
outage on an otherwise-healthy primary. Within that, `synchronous_commit:
remote_write` (not the stricter `on`) acknowledges a commit once the
standby has *received* the WAL over the network, not once it has fsynced it
to its own disk — durable against a primary crash, without a second
disk-flush round-trip added to every `wallet_ledgers`/RADIUS-accounting
write (DBD §6.6 covers the query-routing side of this same trade-off).

### What is not solved by this file alone

The connection string change (multi-host DSN, below) makes every *new*
`internal/db` connection resolve to the correct current primary
automatically — no Go code changes needed for that part, confirmed against
`pgx/v5`'s `pgconn.ParseConfig`, which implements the same
`target_session_attrs` behavior as libpq. What it does **not** do: a
connection pgxpool already had checked out *at the moment of failover* is
still physically talking to the old primary, now a read-only standby. The
next write on that specific connection fails with SQLSTATE `25006`
(`read_only_sql_transaction`) — a normal SQL error, not a network fault, so
pgxpool has no built-in reason to evict that connection on its own. Closing
that gap is a small, real `internal/db` change (recognize `25006`, force
that one connection closed, let the pool dial fresh — which then resolves
correctly via `Fallbacks`) and is scoped as its own implementation pass once
this topology is actually running, not bundled into this configuration
change.

**Implemented as `internal/db/hapool.go`** (`dbPool` interface, `haPool`
wrapper around `*pgxpool.Pool`, `haRow`/`haTx` for the `QueryRow`/transaction
paths) — every store in `internal/db` now goes through it. Verified live,
not just unit-tested, using `scripts/pg_failover_drill` (a small program
using this codebase's own `internal/db.Connect` and a real store's write
method, run against an actual 3-node Patroni cluster while the primary was
killed/switched over):

| Test | What happened | Recovery mechanism | Time |
|---|---|---|---|
| `docker kill` on the primary | Connection died mid-write (`unexpected EOF`) | pgx's own connection-health tracking + `Fallbacks` reconnect — `haPool` never engaged | ~26s |
| Graceful `patroni ... switchover` | Patroni restarts the demoted node during role transition, terminating its connections (`SQLSTATE 57P01`) | Same as above — `haPool` never engaged | ~4s |
| Backend held read-only while the connection stayed alive (`default_transaction_read_only=on`, no restart) — the specific condition `haPool` targets | Write failed with `SQLSTATE 25006` exactly as designed | `haPool.checkFailover` detected it, logged, called `Pool.Reset()` | ~4s once a real primary existed again |

The honest finding: in this Patroni configuration, both real failure modes
tested (hard crash, graceful switchover) terminate connections outright
rather than leaving them alive-but-demoted — so `pgx`'s own native
reconnection already recovers them, without `haPool`'s SQLSTATE 25006 logic
ever engaging. That logic is still correct and worth keeping — the
alive-but-read-only condition it defends against is real (a manual
`pg_promote()` without full Patroni-orchestrated demotion of the old
primary, or certain proxy/timing configurations, can produce exactly this),
and the third test above confirms it fires precisely as designed when that
condition actually occurs. It just wasn't the mechanism that happened to
fire in either of the two most obvious failure drills.

---

## 8.3 Redis Sentinel Configuration

### `config/redis/redis-primary.conf`

```
port 6379
appendonly yes
appendfsync everysec
maxmemory 2gb
maxmemory-policy volatile-ttl
save 900 1
save 300 10
save 60 10000
```

### `config/redis/sentinel.conf`

```
port 26379
sentinel monitor bss_master redis_primary 6379 2
sentinel down-after-milliseconds bss_master 3000
sentinel failover-timeout bss_master 10000
sentinel parallel-syncs bss_master 1
```

> Quorum = 2 (2 of 3 sentinels must agree for failover).

---

## 8.4 Environment Variables Reference

| Variable | Required | Description |
|---|---|---|
| `DB_SECURE_PASSWORD` | Yes | PostgreSQL superuser password |
| `PG_REPLICATION_PASSWORD` | Only with `docker-compose.pg-ha.yml` | Postgres replication user password (§8.2a) |
| `JWT_SECRET` | Yes | HMAC secret for JWT signing |
| `RAZORPAY_WEBHOOK_SECRET` | Yes | HMAC secret for Razorpay webhook validation |
| `REDIS_SENTINEL_ADDRS` | Yes | Comma-separated sentinel addresses |
| `REDIS_MASTER_NAME` | Yes | Sentinel master name |
| `AES_KEY_STORE_URL` | Yes | Secret manager URL for AES key retrieval |
| `PAGERDUTY_ROUTING_KEY` | Yes | PagerDuty Events API v2 routing key |
| `LOG_FORMAT` | No | `json` (default) or `text` |
| `LOG_LEVEL` | No | `debug`, `info`, `warn`, `error` |

Store all secrets in a `.env` file excluded from version control (`.gitignore`), or inject via your secret manager of choice.

---

## 8.5 Backup Procedures

### PostgreSQL

```bash
# WAL archiving: add to postgresql.conf
archive_mode = on
archive_command = 'aws s3 cp %p s3://your-bucket/wal/%f'
wal_level = replica

# Weekly full backup (run via cron on backup host)
pg_dump -h localhost -U postgres -Fc isp_bss_oss \
  | aws s3 cp - s3://your-bucket/pgdump/$(date +%Y%m%d).dump

# Point-in-time restore target (RPO = last WAL archive ~5 min)
```

### Redis

```bash
# Verify AOF is enabled and healthy
redis-cli -h bss_redis_primary INFO persistence | grep aof_enabled

# Manual RDB snapshot
redis-cli -h bss_redis_primary BGSAVE

# Copy RDB to backup location
docker cp bss_redis_primary:/data/dump.rdb /backup/redis/$(date +%Y%m%d).rdb
```

---

## 8.6 Health & Readiness Probe Summary

| Service | Probe Type | Endpoint / Command | Interval |
|---|---|---|---|
| `postgres_primary` | TCP + query | `pg_isready` | 10s |
| `postgres_primary`/`_standby_N` (pg-ha overlay only) | HTTP | `GET :8008/primary` returns 200 only on the current leader (§8.2a) | operator/OPS-script use, not a container healthcheck |
| `redis_primary` | TCP | `redis-cli ping` | 5s |
| `aaa_core_daemon` | Prometheus scrape | `:9100/metrics` | 15s |
| `api_service` | HTTP | `GET /health` | 15s |
| `gotenberg_engine` | HTTP | `GET /health` | 30s |

---

## 8.7 WhatsApp Business API Container *(new — MOD-NOTIF)*
**FR:** FR-NOTIF-001..011 | **SAD Ref:** SAD-COMP-005

The notification service connects to Meta's Cloud API externally (no self-hosted container). Add the following environment variables to `api_service` and `aaa_core_daemon`:

```yaml
# Add to api_service and aaa_core_daemon environment in docker-compose.yml
environment:
  - WHATSAPP_PHONE_NUMBER_ID=${WHATSAPP_PHONE_NUMBER_ID}
  - WHATSAPP_ACCESS_TOKEN=${WHATSAPP_ACCESS_TOKEN}
  - WHATSAPP_WEBHOOK_VERIFY_TOKEN=${WHATSAPP_WEBHOOK_VERIFY_TOKEN}
  - SMS_GATEWAY_PROVIDER=${SMS_GATEWAY_PROVIDER}   # twilio | msg91 | exotel
  - SMS_GATEWAY_API_KEY=${SMS_GATEWAY_API_KEY}
  - SMS_GATEWAY_SENDER_ID=${SMS_GATEWAY_SENDER_ID}
```

**WhatsApp Webhook Public URL:** The `api_service` must be reachable from Meta's servers on `POST /webhooks/whatsapp`. Configure your reverse proxy to forward this path. Meta requires HTTPS with a valid TLS certificate (self-signed not accepted).

**Additional env vars reference (v2 additions):**

| Variable | Required | Description |
|---|---|---|
| `WHATSAPP_PHONE_NUMBER_ID` | Yes | Meta Business phone number ID |
| `WHATSAPP_ACCESS_TOKEN` | Yes | Meta permanent/system user access token |
| `WHATSAPP_WEBHOOK_VERIFY_TOKEN` | Yes | Random string for Meta webhook verification |
| `SMS_GATEWAY_PROVIDER` | Yes | `twilio`, `msg91`, or `exotel` |
| `SMS_GATEWAY_API_KEY` | Yes | Provider API key |
| `SMS_GATEWAY_SENDER_ID` | Yes | Approved SMS sender ID (DLT registered) |
| `PORTAL_JWT_SECRET` | Yes | Separate secret for subscriber-scoped portal JWTs |
