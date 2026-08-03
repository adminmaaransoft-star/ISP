BSS/OSS ISP Management Platform — Complete Project Package
===========================================================
Generated: 2025-06-01
Project: FTTH ISP BSS/OSS (Jaze replacement) for Indian operators

FILES IN THIS PACKAGE
─────────────────────

TRACKERS (Excel .xlsx)
  bss_oss_dev_tracker_v3.xlsx   ← USE THIS — latest, all changes applied
  bss_oss_dev_tracker_v2.xlsx   ← Previous version (before Gemini review)
  bss_oss_dev_tracker.xlsx      ← Original v1 tracker

  The v3 tracker contains 16 sheets:
    📋 Instructions              Read first — legend, 3 rules, context budget
    ✅ Definition of Done        66 checks across 9 levels (L0–L8)
    📊 Progress Dashboard        Live formulas — updates as you mark tasks Done
    🔧 Environment Setup         9 sessions (includes ENV-009 compliance.sh)
    🗄️ DB Migrations             15 sessions — 12 use Llama 8B, 3 use Qwen3 14B
    🔐 Crypto Module             4 sessions
    📡 RADIUS AAA                5 sessions
    💳 Billing Module            6 sessions
    🔔 Notifications             5 sessions (WhatsApp + SMS + Email)
    ⚡ FUP + Asynq               3 sessions
    🔑 API + Auth                4 sessions
    📊 Revenue + Franchise       5 sessions
    🌐 Portal                    4 sessions — 2 use Llama 8B, 2 use Qwen3 14B
    🧪 Integration Tests         34 INT-* test cases
    ⚡ NFR Load Tests            8 NFR validation tests
    📎 Prompt Snippets           15 snippets including DB Schema Map,
                                  context limit guide, model selection guide

SPECIFICATION DOCS (Markdown .md — specification_docs_v2/)
  00_IDX  Master Traceability Index    (BO → FR → Doc → Test cross-reference)
  01_CRD  Customer Requirements        (personas, business outcomes, WhatsApp spec)
  02_SRS  System Requirements          (52 FRs, 9 NFRs, all with IDs)
  03_SAD  System Architecture          (11 components, data flows, HA/DR)
  04_MDS  Module Design                (10 modules, Asynq task catalogue)
  05_DDS  Detailed Design              (Go code patterns, WhatsApp API, health API)
  06_DBD  Database Design              (22 tables, indexes, partitioning)
  07_API  OpenAPI 3.0 Contract         (25+ endpoints, full YAML spec)
  08_IDD  Infrastructure Design        (Docker Compose, Redis Sentinel, env vars)
  09_SecD Security Design              (STRIDE, AES key rotation, RBAC)
  10_DMP  Data Migration Plan          (phases, rollback, PII handling)
  11_DXD  Developer Setup Guide        (local env, test phases, seed data)
  12_OPS  Operations Runbook           (incident response, PoD/CoA, failover)
  13_TST  Test Strategy                (60+ test cases, load tests, chaos tests)

HOW TO START
────────────
1. Open bss_oss_dev_tracker_v3.xlsx
2. Read the 📋 Instructions sheet
3. Start with 🔧 Environment Setup, session ENV-001
4. Work top-to-bottom through each sheet in order
5. Before every LM Studio session, check the Model column:
   - Teal/green cell = load Llama3.1-8B-Q5_K_M (~5GB VRAM)
   - White cell      = load Qwen3-14B-Q4_K_M   (~9GB VRAM at 4k ctx)
6. Set LM Studio context to 4096 before every session
7. After each session, run: ./scripts/compliance.sh internal/{module}
8. Mark Status = Done only when all applicable DoD checks pass

VRAM QUICK REFERENCE
────────────────────
Qwen3-14B Q4_K_M @ 4k ctx  = ~9.1 GB  → 2.9 GB free for Docker
Llama3.1-8B Q5_K_M @ 4k ctx = ~5.1 GB  → 6.9 GB free for Docker
Before load tests: Eject model in LM Studio to free VRAM
