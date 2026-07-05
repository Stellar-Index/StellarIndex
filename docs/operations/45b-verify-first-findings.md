---
title: BACKLOG 45b — verify-first findings (code-sweep cluster)
last_verified: 2026-07-05
status: current
---

# BACKLOG 45b — verify-first findings

The 45b "code-sweep P4 cluster" entry accumulated claims from the
2026-07-04 TODO sweep. Several were stale by the time they were read
back. This file is the durable record of what a code-first verification
(2026-07-05) actually found, per item — the backlog entry should be
read through this lens.

## Items verified and acted on

### 1. Freeze-state recovery worker — ALREADY DONE (claim was stale)

The worker exists and runs: `internal/aggregate/freeze/recovery.go`
(F-1229) lists open `freeze_events` rows every 60s, checks the Redis
marker per (asset, quote), and stamps `recovered_at` when the TTL has
elapsed. It is constructed and started in
`cmd/stellarindex-aggregator/main.go` whenever the anomaly checker +
Redis are both configured (same condition under which freezes can be
written at all, so there is no lifecycle branch where a marker is
written but never recovered). The wave-91 metrics
(`anomaly_freeze_recovery_sweep*`) instrument this worker — they were
not orphaned instrumentation. No gap found; the 45b line item is
closed as already-shipped.

### 2. Resume-stalled SDEX/classic gap detection — REAL GAP, closed

Verified: `stellarindex-ops resume-stalled` gated Soroban-era plans
against data-derived ground truth (`FindSorobanEventsLedgerGaps`) but
blanket-skipped SDEX-only plans with "data-derived gap detection not
yet implemented for non-Soroban decoders". Meanwhile the per-source
scan it needed already existed: the gap detector's
`sdex`/`trades` target (`FindPerSourceLedgerGaps` with
`WhereFilter source = 'sdex'` in
`internal/storage/timescale/per_source_gaps.go`).

Closed in `cmd/stellarindex-ops/resume_stalled.go`: SDEX-only plans
now gate against that same scan, scoped to the served-tier retention
window. The scan floors at the actual oldest served sdex ledger
(mirrors `compute-completeness`'s `retentionFloor` — below-floor
absence is a retention artifact, not a gap, per ADR-0034), is built
lazily (only when a classic-only plan survived parsing), and yields
four outcomes: actionable (overlaps a real gap inside the retained
window), false-positive skip (no gap), below-floor skip (history
lives in the lake; `ch-rebuild` territory), and straddling-floor skip
(needs operator review). `--force-classic-cursors` still bypasses the
whole gate, unchanged.

### 3. Per-venue pair YAML — pattern shipped for binance

Verified: `binance/pairs.go` and `kraken/pairs.go` were Go-literal
tables feeding the canonical asset constructors. The 45b framing
("YAML would ease ops") over-promises: a `//go:embed` YAML is still
compile-time — a pair edit still means rebuild + redeploy. What it
does buy is a declarative, reviewable venue-pair table where adding a
pair is a data edit, not constructor plumbing.

Shipped the pattern for ONE venue: binance now loads
`internal/sources/external/binance/pairs.yaml` via `//go:embed`, with
a golden test (`pairs_test.go`) pinning the exact pre-swap map —
symbol set, base/quote identity, and asset class (fiat EUR vs crypto
EUR is load-bearing). Kraken (and future venues) should copy this
shape when next touched; a true runtime override remains the config
follow-up already anticipated in the original pairs.go comment.

### 4. CH-native completeness preseed — design note, not implemented

Two distinct things travel under this name; neither is a
"materialized expected-counts" table:

- **What the code actually flags as follow-up** (comment in
  `cmd/stellarindex-ops/compute_completeness.go`, factory-preseed
  block): `preseedFactoryChildren` reads the *Postgres*
  `soroban_events` landing zone to warm ADR-0035 factory-child gates
  before a `-from`-windowed re-derive. A `-ch` run is therefore not
  purely lake-backed — it still needs the PG landing zone for
  creation events. The CH-native version is small and well-defined:
  stream the factories' creation events from the lake (the
  `ReconcileEventStreamer` used for the re-derive already supports
  contract + topic filtering) instead of
  `store.StreamSorobanEvents`. It is a purity fix, not a runtime
  fix — factory creation events are rare and indexed on both sides.
- **The runtime claim ("preseed expected counts to cut re-derive
  cost") does not survive verification.** Expected counts are not a
  queryable aggregate: they are produced by running the real
  decoders over lake events (validate gates, served-PK dedup,
  Phoenix 8-events-to-1-trade grouping, factory gating). A ClickHouse
  materialized view cannot reproduce decoder semantics, so
  `contract_events_daily` (AggregatingMergeTree, per-day
  contract/topic counts — built for `/v1/protocols` analytics) can
  seed *candidate windows* at best, never the reconcile oracle
  itself. And the runtime problem is already solved structurally:
  `compute-completeness -from` (incremental verify) plus
  `-skip-substrate` / `-skip-recognition` let the daily timer
  re-check only new ledgers — minutes, not hours. A preseed table
  would add a second source of truth for no measured win.

Verdict: keep `-from` incrementality as the runtime answer; do the
CH-native factory preseed as a small purity follow-up when
compute-completeness is next touched; do NOT build an
expected-counts materialization.

## Gated / larger items — one-line verify-first verdicts

Each line: what is real in code today, and what the item is actually
gated on. File references verified 2026-07-05.

- **IsRemoval v2**: partially done — removal semantics work where the
  LedgerKey carries the asset (`trustlines`, `sac_balances`;
  `internal/supply/lcm_reader.go` consumes it); genuinely absent for
  `claimable_balances` + `liquidity_pools` (their v1 events document
  `IsRemoval` as reserved — writer-side asset lookup needed); gated
  on the supply Sum overcount becoming measurable in production.
- **rozo v2 token field**: real but correctly deferred — `Payment`
  deliberately omits a token field (contract hardcodes USDC in v1;
  `internal/sources/rozo/events.go`), while `Flush` already carries
  `Token`; gated on Rozo v2 Forwarder/IntentBridge reaching mainnet
  (pre-mainnet today per the package README).
- **Comet reserve tracker**: genuinely absent — `comet_liquidity`
  stores add/remove deltas "surfaced for downstream reserve-tracking"
  but no tracker exists; harder than Soroswap's Sync correlation
  because Comet spot price needs reserves AND per-token weights;
  gated on the requirement emerging (per `comet/README.md`).
- **CMC divergence reference**: absent in `internal/divergence/`
  by documented decision (`doc.go`: deferred until an operator asks
  for a second aggregator behind CoinGecko) — but CMC exists as a
  disabled-by-default external aggregator source
  (`internal/sources/external/coinmarketcap/`); gated on operator
  demand + a paid key with redistribution rights.
- **Aggregation tick frequency**: ALREADY DONE — the VWAP tick is
  `[aggregate] interval_seconds` (default 30s,
  `internal/aggregate/orchestrator`), and the recent work went the
  other way (throttling the divergence sub-pass via
  `divergence_min_interval_seconds`); nothing to build.
- **HA MinIO**: no code component — the erasure-coded/HA design is in
  ADR-0002/ADR-0008 but r1 runs single-drive EC:0; gated purely on
  hardware procurement/provisioning (ops decision).
- **Separate monitoring box (Phase B)**: partially built — a full
  `configs/ansible/roles/prometheus/` role exists targeting a 2-host
  `prometheus_pair` group, but r1 runs monitoring on-box; gated on
  provisioning the dedicated hosts and populating the inventory
  group.
- **Trades composite index (disk-gated)**: ALREADY DONE — the
  `(base_asset, quote_asset, ts DESC)` composite has existed since
  migration 0001, plus `(base, quote, source, ts, ledger)` in 0037;
  the "disk-gated" caveat is only the documented
  `CREATE INDEX CONCURRENTLY`-first rollout procedure on a populated
  node, not a missing index.
- **CH-native supply persistence**: mostly done, claim stale —
  `ch-supply -write` (to `stellar.token_supply`) and `-seed-flows`
  both exist, and `supply_flows` is written live at ingest by the CH
  sink; genuinely remaining: an incremental MV for the token_supply
  rollup and the snapshot-shape integration (classic↔SAC asset_key
  mapping, XLM total_coins) per
  `docs/architecture/clickhouse-supply-from-ch.md`.
- **SEP-1 trust-chain signature verification**: bidirectional
  issuer↔toml `org_verified` is done and enforced; cryptographic
  SIGNING_KEY/trust-chain verification is genuinely absent and
  explicitly reserved for a future ADR
  (`internal/metadata/doc.go`) — needs-ADR, post-launch.
