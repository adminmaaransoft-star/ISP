# Draft NFR entries — for review, not yet in the SRS

Proposed additions to `02_SRS_System_Requirements.md` §2.2, covering three
features shipped this session with no existing NFR: document archival
(FR-DOC-001), the captive portal (FR-HSP-001), and report export
(FR-RPT-002).

Numbers are drawn from what the code currently does, not invented targets —
each row cites the constant it's checking, so a threshold here fails loudly
against reality rather than needing to be reverse-engineered later. Where the
existing behavior seemed like the wrong number to enshrine, that's flagged
rather than silently accepted.

Table format matches `02_SRS_System_Requirements.md` §2.2 exactly, for a
direct paste once reviewed.

| NFR ID | Category | Requirement | Validation Method | CRD Ref | TST Ref |
|---|---|---|---|---|---|
| NFR-DUR-002 | Durability | An archived document's checksum must verify against its stored bytes on every retrieval; a document must never be purged before its `retain_until` date | Integration test: archive, corrupt the stored file, verify retrieval detects it; attempt a purge before `retain_until` and confirm the DB constraint (`chk_archive_not_purged_before_retention`) rejects it | — | TST §13.12 *(new)* |
| NFR-SEC-003 | Security | The captive portal (unauthenticated by design) must rate-limit voucher redemption attempts and fail closed — refuse rather than allow — if the limiter backend is unavailable | Integration test: exceed 10 attempts per MAC in 15 minutes and confirm refusal; kill Redis mid-window and confirm the endpoint refuses rather than admits | — | TST §13.12 *(new)* |
| NFR-PERF-004 | Latency | Report export (CSV, synchronous) must complete within 10 s for the maximum 120-month window at current data volumes; anything requiring longer must use the scheduled/async export path rather than holding the HTTP connection open | Timed integration test against a seeded 120-month dataset per report type | — | TST §13.12 *(new)* |

---

## Notes for review

**NFR-DUR-002.** The two guarantees that already exist in code — checksum-on-write (`archive.LocalStore.Put` hashes while streaming) and the retention floor (`chk_archive_not_purged_before_retention` in migration 034/035) — were never stated as requirements, so nothing currently *tests* that a corrupted file is caught on read, only that one isn't produced on write. Worth deciding whether "verify on retrieval" should mean re-hashing on every read (a real cost at scale) or only on restore/audit access. I did not pick one; the draft leaves the validation method open to that decision.

**NFR-SEC-003.** The fail-closed direction is already a deliberate choice in `RedisLimiter.Allow` (a broken limiter refuses rather than admits), so this NFR mostly formalizes an existing decision rather than setting a new bar. The 10/15min figure is `DefaultAttemptLimit`/`DefaultAttemptWindow` as shipped — flagging that nobody has load-tested whether 10 attempts is generous enough for a legitimate user mistyping a voucher code, or already too loose against a targeted guessing script given the code space (12 chars, 30-symbol alphabet). That's a threshold decision, not a testing gap.

**NFR-PERF-004.** This is the one genuine invention — there's no existing behavior to cite a number from, unlike the other two. I picked 10s by analogy to `NFR-BIZ-001`'s 60s bound on a comparable-scale query, scaled down because a report is a narrower read than the unbilled-subscriber scan. `MaxMonths = 120` in `internal/reporting/export.go` is the actual cap the code enforces; nothing currently measures how long that worst case takes. This number is a guess and should be treated as one until measured — I'd suggest running it once against a realistically-sized seed before it goes in the SRS as a target, rather than committing to 10s now and finding out it's wrong later.
