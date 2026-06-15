# 03 — Coverage matrix (whole-system audit, 2026-06-14)

Proves the 24-area fan-out touched every area + every audit dimension. Severity
columns are each area file's own `## Severity counts`; the files-read column is the
count from each file's "Files read" section. Synthesized 2026-06-15.

## Per-area coverage

| area | scope (1-line) | files read | C | H | M | L | I | dimensions exercised |
|---|---|---|---|---|---|---|---|---|
| A01 | Ingest core: ledgerstream / dispatcher / consumer / pipeline | 42 | 0 | 0 | 2 | 4 | 0 | D1 D2 D4 D5 D9 |
| A02 | ClickHouse lake: storage/clickhouse + deploy/clickhouse (explorer_reader, extract_entry_changes) | 28 | 0 | 0 | 2 | 3 | 1 | D1 D2 D3 D5 D6 |
| A03 | Served tier: storage/timescale + storage/redisclient (MinIO N/A — no such pkg) | 70 | 0 | 0 | 2 | 5 | 1 | D1 D2 D3 D4 D5 D6 |
| A04 | Projector + completeness + archivecompleteness + hashdb | 19 | 0 | 1 | 3 | 5 | 2 | D1 D2 D4 D5 D9 |
| A05 | On-chain Soroban sources + ADR-0035 gating (16 decoder packages) | 58 | 0 | 1 | 3 | 5 | 0 | D1 D2 X8 |
| A06 | External CEX/FX sources (11 venues + framework) | 63 | 0 | 0 | 0 | 4 | 5 | D1 D2 D3 D4 D5 |
| A07 | Supply observers + derivation (supply + 6 observer packages) | 46 | 0 | 1 | 2 | 4 | 0 | D1 D2 D4 D5 D6 D9 X8 X10 |
| A08 | Canonical types + scval + events (+ canonical/discovery) | 33 | 0 | 0 | 1 | 5 | 0 | D1 D2 D3 D4 D5 D8 D9 |
| A09 | Aggregation + divergence + currency + metadata | 77 | 0 | 0 | 1 | 5 | 9 | D1 D2 D3 D4 D5 X2 |
| A10 | API: pricing / catalogue handlers (ex-explorer/auth) | 53 | 0 | 0 | 3 | 6 | 8 | D1 D2 D3 D5 D7 |
| A11 | API: network explorer (ADR-0038 handlers + xdrjson + reader) | 14 | 0 | 3 | 3 | 4 | 3 | D1 D2 D3 D4 D7 D9 |
| A12 | Auth / account / dashboard / platform / ratelimit / usage / cachekeys | 41 | 0 | 2 | 6 | 7 | 4 | D1 D3 D4 |
| A13 | cmd binaries: indexer/aggregator/api/ops/migrate/sla-probe wiring | 19 | 0 | 0 | 0 | 3 | 5 | D1 D2 D4 D5 X4 |
| A14 | Public SDK: pkg/client (wire shapes + SemVer) | 10 | 0 | 1 | 2 | 5 | 2 | D1 D2 D3 SemVer |
| A15 | Migrations + schema (122 sql = 61 pairs + runner) | 122 | 0 | 4 | 7 | 6 | 3 | D1 D2 D6 X7 |
| A16 | obs / config / notify / incidents / customerwebhook / obstest / stellarrpc / version | 42 | 0 | 1 | 1 | 4 | 2 | D1 D2 D3 D4 D9 |
| A17 | Explorer Web UI (Next.js static-export explorer pages) | 24 | 0 | 2 | 2 | 4 | 2 | D1 D2 D3 D7 |
| A18 | Ops surface: configs / scripts / deploy / .github / Makefile | 95 | 0 | 1 | 1 | 4 | 5 | D1 D3 D9 |
| A19 | Docs integrity: CLAUDE.md / ADRs / OpenAPI / reference / runbooks | 41 | 0 | 3 | 5 | 6 | 0 | D9 |
| A20 | Test suites: integration / load (k6) / chaos / fixtures + coverage meta | 49 | 0 | 1 | 6 | 13 | 1 | D1 D8 |
| A21-A22 | Deps / build integrity + licensing / whole-tree secret sweep | tree¹ | 0 | 1 | 0 | 2 | 3 | D2 D3 D9 |
| X1 | Cross-cut: ingest dataflow end-to-end (LCM→…→API), one-writer | 31 | 0 | 0 | 3 | 4 | 1 | X1 X3 X8 |
| X3-X5 | Cross-cut: explorer dataflow + interface conformance | 26 | 0 | 1 | 2 | 3 | 0 | D1 D2 D3 D4 D5 X3 X5 |
| X4 | Cross-cut: config → wiring (every field → consumer) | 14 | 0 | 0 | 3 | 6 | 0 | X4 |
| X6 | Cross-cut: auth/permission flow (key mint→store→validate→mw; session; SEP-10) | 22 | 0 | 1 | 3 | 3 | 3 | X6 D3 D4 |
| X9 | Cross-cut: panic-safety in request + ingest paths | 29 | 0 | 1 | 3 | 1 | 1 | X9 D9 |
| **Totals** | **26 areas** | **1068²** | **0** | **25** | **66** | **121** | **61** | — |

> X6 + X9 were planned seams (00-audit-plan X1–X10) that had no written
> conclusion until they were commissioned during the 2026-06-15 remediation; the
> gap-check (X2/X7/X8/X10 were covered inside A-areas) surfaced them. Both found
> one real High (now FIXED). Full 26-area census = **273** findings; the
> consolidated register (`02`) folds in the 2 new Highs (→ 259 there) and leaves
> the 14 X6/X9 sub-High findings itemised in their own area files.

¹ A21-A22 swept the whole git-tracked tree (2,452 files) via pattern greps + targeted
reads of ~15 build/lint/license control files; no single "files read" integer.
² Sum of the per-area files-read integers (A21-A22 excluded — tree-wide sweep). The
severity columns sum to **257**, matching the consolidated register
(`02-findings-register.md`) row-for-row. A few area files' own `## Severity counts`
headers round their lowest tier ("Low / Info") together or are off by one vs their
findings table (e.g. A03 declares Medium 3 for 2 Medium + 5 Low rows; A04 declares
"Low/Info 6" for 7 rows) — the per-area cells above use the actual finding rows.

> Per-area notes where the file's own `Severity counts` differs slightly from a raw row
> count: A04's table reads "Low / Info 6" but the findings table has 5 Low + 2 Info (7
> rows). A03's verdict header says Medium 3 but its findings table has 2 Medium + 5 Low.
> A05/A07 grade their lowest tier "Low / Info" combined; the register splits them per
> the row's own label. A18's etcd item (R-A18-11) is graded Medium. In every case the
> matrix cells + register sections use the **actual finding rows**, so both sum to 257.

## Dimensions legend

The canonical dimension set is defined in `00-audit-plan.md §1` (D1–D9) and §3/§3b
(X1–X10). Every code that appears in any area's `dim` column, with its meaning:

**Per-area dimensions (D1–D9) — applied to every area:**

| code | meaning |
|---|---|
| D1 | Correctness — logic bugs, off-by-one, wrong defaults, mis-decodes, nil derefs, unchecked type assertions, error-swallowing. |
| D2 | ADR invariants — i128-never→int64 (0003); one-writer-per-domain (0031/0032); ingest only via Galexie→dispatcher (no stellar-rpc); ClickHouse=lake/Postgres=served (0034); factory-anchored gating (0035); closed-bucket serving (0015); no Horizon (0001); EVERY-event decoders; XLM dual-form alias loops. |
| D3 | Security — authn/authz gaps, API-key posture, SQL injection, secrets in code/logs, input validation, SSRF/path traversal, rate-limit bypass, PII in logs. |
| D4 | Concurrency — data races, unguarded shared maps, goroutine leaks, context propagation, cancellation, deadlocks. |
| D5 | Resource/lifecycle — leaked rows/conns/files/goroutines, unbounded queries/scans (no-unbounded-trade-scan rule), missing timeouts, batch sizing, memory blowups. |
| D6 | Data/schema — migration correctness + reversibility, cagg/retention drift, index coverage vs query patterns, NUMERIC for i128, FK/constraint integrity, ReplacingMergeTree dedup keys. |
| D7 | API contract — OpenAPI↔handler parity (paths/params/shapes/codes), envelope consistency, problem+json consistency, pagination correctness, cache-key/prewarm drift. |
| D8 | Tests — coverage gaps on critical paths, test rot (asserting stale behaviour), missing regression tests for known traps, integration-tag gaps. |
| D9 | Docs/config drift — CLAUDE.md / ADRs / reference vs reality; config defaults vs deployed; dead alerts/runbooks; stale brand. |

**Cross-cutting seam codes (X1–X10) — traced end-to-end across packages:**

| code | meaning |
|---|---|
| X1 | Ingest dataflow seam — LCM → ledgerstream → dispatcher (4 decoder seams) → sink → CH lake → projector → Postgres → API; type contracts + one-writer at each hop. |
| X2 | Pricing dataflow seam — trades/oracle_updates → caggs → aggregate (VWAP / stablecoin proxy / USD-peg combine) → API price/ohlc/history → wire. |
| X3 | Explorer dataflow seam — lake tables → ExplorerReader → xdrjson → handlers → OpenAPI → UI; index coverage vs query predicates. |
| X4 | Config→wiring seam — every config field → its consumer; no orphan/unwired fields, no consumer reading an unset field. |
| X5 | Interface conformance seam — every interface ↔ its implementations + stubs stay in sync (ExplorerReader, the decoder seams, external.Connector, HistoryReader). |
| X7 | Migration↔code seam — every table/column the code reads/writes exists in migrations (+ lake DDL); no drift. |
| X8 | Known-traps-still-handled seam — each CLAUDE.md "Things that will surprise you" trap still correctly handled (Soroswap sync-reserves, Phoenix 8-events, Comet shared topic, Reflector 3-contracts, Band E18/E9 + zero-events, Redstone feed_id-from-op-args, CAP-67/SEP-41 dual shape, stablecoin proxy layer, non-uniform decimals, XLM dual-form, WASM-version-aware backfill). |
| X10 | Determinism/region-stability — closed-bucket responses byte-identical across regions (ADR-0015); no time.Now()/map-order nondeterminism in serialized output. |

| other | meaning |
|---|---|
| SemVer | Public-SDK SemVer stability (ADR-0005) — exported wire-shape/method compatibility, the v0.x break-allowed / v1.0 binding contract. |

Seams in the plan but not surfaced as a `dim`-column tag in any area file: **X6**
(auth/permission flow — exercised within A12's D3 findings rather than tagged X6),
**X9** (panic-safety in request/ingest paths — exercised within A01/A11/X1 D1/D9
findings, e.g. the MuxedAccount.Address/ToAccountId panic items). Both seams WERE
audited; they simply carry per-area D-codes rather than an explicit X tag.

## Coverage gaps / not-in-scope (explicitly flagged by the area files)

Items the area files called out as not-read, deferred, or out of scope:

- **A03 — MinIO adapter not in scope / does not exist as a package.** There is no
  `internal/storage/minio/`; the MinIO/S3 read path lives in the ledgerstream/Galexie
  layer (A01 scope). A03 flags this so its scope line isn't read as "MinIO clean."
- **A05 — out-of-scope sources NOT audited here:** classic/SAC supply observers
  (`accounts`, `trustlines`, `claimable_balances`, `liquidity_pools`, `sac_balances`),
  `sdex`, `forex`, `frankfurter`, `external/` — covered by A06/A07.
- **A10 — `*_test.go` scoped in but not individually enumerated** (no test-rot surfaced
  in the handlers reviewed). 32 of ~53 handler files covered via two sub-agents.
- **A12 — test files enumerated/consulted but not line-audited individually** (production
  paths were the audited surface).
- **A13 — 19 of 68 in-scope files read in full**; the rest covered via targeted greps
  (i128-truncation, os.Exit-in-handlers, context/signal setup, test inventory).
- **A15 — read method:** ~12 migrations read in full; all 61 `up` + relevant `down`
  inspected by targeted extraction (BEGIN/COMMIT, retention calls, typing, chunk
  intervals); driver behaviour confirmed against vendored source.
- **A18 — ~95 of 308 in-scope files** read in full or via targeted grep/agent (full
  secret scan + alert↔runbook parity + ~30 of 150 ansible files + full-tree greps).
- **A20 — the biggest COVERAGE gaps below the served tier (deferred/missing tests):**
  (1) ClickHouse lake write/read round-trip is **unit-only, no live server** — zero
  integration coverage of the ADR-0034 source-of-truth (CH sink, tier1 DDL apply,
  galexie→CH→decoders→PG, `ch-rebuild` write path); (2) explorer endpoints +
  participant-index — handler stub-unit-tested at best, **the CH `explorer_reader.go`
  has no test at all**, no integration server stands up any explorer route; (3)
  entry-change extract reader untested end-to-end; (4) `/v1/changes` no integration;
  (5) aggregator compute→CAGG-refresh→read no end-to-end; (6) Soroswap/Phoenix
  swap+sync correlation no storage-integration (the promised e2e subtest is
  unimplemented); (7) SSE hub has no functional/chaos test; (8) **Chaos Wave-2 (HA)
  entirely deferred** + Wave-1 `04-redis-misconf.sh` never run; (9) webhook
  delivery (HMAC-sign + POST + retry drain) store-only, no mock-receiver round-trip.
- **A21-A22 — per-file license headers deliberately not enforced** (root LICENSE
  satisfies Apache-2.0); no documented per-file-header policy.
- **A14 — explorer/SSE/SEP-10 SDK surfaces deliberately absent or unreachable:**
  the R-018 multi-network types exist without methods (gap, not bug); SSE streaming
  + SEP-40 passthrough documented as deliberately excluded; a SEP-10 typed helper is
  a documented follow-up.
- **X3-X5 — `xdrjson` decode coverage is Phase-A-scoped:** 15 of 28 op types are
  field-decoded; the other 12 degrade losslessly to `raw_xdr` (a forward Phase-B
  participant-index coverage gap, not a current bug). `operation_results.result_xdr`
  is captured in the lake but not yet read.
- **X5 — implementation behaviour not individually diffed** for the dispatcher's 4
  decoder interfaces (that is the A05/A08 per-source job); X5 verified the seam
  shapes + that every interface has ≥1 impl + the stub stays in sync.
- **X6 / X9 (plan seams)** carry no dedicated area file; audited inside A12 (auth flow)
  and A01/A11/X1 (panic-safety) respectively.
