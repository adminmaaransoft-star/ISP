# Document 8: Infrastructure Design Document (IDD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
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
