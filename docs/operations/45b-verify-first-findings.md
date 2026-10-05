---
title: BACKLOG 45b — verify-first findings (code-sweep cluster)
last_verified: 2026-07-05
status: current
---

# BACKLOG 45b — verify-first findings

What a code-first verification found for each claim in the 45b "code-sweep P4
cluster" backlog entry. Read the backlog entry through this.

## Items verified and acted on

### 1. Freeze-state recovery worker — already done

`internal/aggregate/freeze/recovery.go` stamps `recovered_at` on open
`freeze_events` every 60 s; started in `cmd/stellarindex-aggregator/main.go`
under the same condition that allows freezes to be written. Instrumented by
`anomaly_freeze_recovery_sweep*`.

### 2. Resume-stalled SDEX/classic gap detection — closed

`cmd/stellarindex-ops/resume_stalled.go` gates SDEX-only plans against
`FindPerSourceLedgerGaps` (`source = 'sdex'`,
`internal/storage/timescale/per_source_gaps.go`), floored at the oldest served
sdex ledger (below-floor absence is retention, not a gap, per ADR-0034).
Outcomes: actionable, false-positive skip, below-floor skip (`ch-rebuild`
territory), straddling-floor skip (operator review). `--force-classic-cursors`
bypasses the gate.

### 3. Per-venue pair YAML — binance only

Binance loads `internal/sources/external/binance/pairs.yaml` via `//go:embed`,
pinned by `pairs_test.go` (symbol set, base/quote identity, asset class — fiat
EUR vs crypto EUR is load-bearing). Still compile-time; copy the shape to Kraken
when next touched.

### 4. CH-native completeness preseed — design note

- `preseedFactoryChildren` (`cmd/stellarindex-ops/compute_completeness.go`)
  read the Postgres `soroban_events` landing zone to warm ADR-0035 factory-child
  gates; the CH-native version streams creation events from the lake via
  `ReconcileEventStreamer`. A purity fix, not a runtime fix (INV-1163, done).
- **Do NOT build an expected-counts materialization.** Expected counts come from
  running the real decoders (validate gates, served-PK dedup, Phoenix
  8-events-to-1-trade grouping, factory gating). A ClickHouse materialized view
  cannot reproduce decoder semantics, so `contract_events_daily` can seed
  candidate windows at best, never the reconcile oracle itself. Runtime is
  already solved by `compute-completeness -from` plus `-skip-substrate` /
  `-skip-recognition`.

## Gated / larger items

- **IsRemoval v2**: done for all four supply sources. `claimable_balances` and
  `liquidity_pools` resolve a removed entry's asset from the same-ledger STATE
  pre-image (`dispatcher_adapter.go` memo) and emit `IsRemoval` rows, which
  `SumClaimableBalancesAtOrBefore` / `SumLPReservesAtOrBefore` exclude; an
  unattributable removal is a decode error, not a silent drop. Pinned by each
  package's `removal_supply_test.go`.
- **rozo v2 token field**: `Payment` omits a token field (v1 hardcodes USDC;
  `internal/sources/rozo/events.go`), `Flush` carries `Token`. Gated on Rozo v2
  Forwarder/IntentBridge reaching mainnet (INV-1164).
- **Comet reserve tracker**: absent; `comet_liquidity` stores add/remove deltas
  only. Needs reserves AND per-token weights. Gated on the requirement emerging
  (`comet/README.md`).
- **CMC divergence reference**: absent from `internal/divergence/` by decision
  (`doc.go`); CMC exists as a disabled-by-default aggregator source
  (`internal/sources/external/coinmarketcap/`). Gated on operator demand + a
  paid key with redistribution rights (INV-0958).
- **HA MinIO**: no code component; r1 runs single-drive EC:0. Gated on hardware.
- **Separate monitoring box**: `configs/ansible/roles/prometheus/` targets a
  2-host `prometheus_pair` group; r1 monitors on-box. Gated on provisioning.
- **CH-native supply persistence**: `ch-supply -write` and `-seed-flows` exist,
  `supply_flows` is written live. Remaining: an incremental MV for the
  token_supply rollup and the snapshot-shape integration (classic↔SAC
  asset_key mapping, XLM total_coins) per
  `docs/architecture/storage-considerations.md#supply-flows-in-the-lake`.
- **SEP-1 trust-chain signature verification**: issuer↔toml `org_verified` is
  enforced; SIGNING_KEY verification is absent, reserved for a future ADR
  (`internal/metadata/doc.go`), post-launch.
- Already done, nothing to build: aggregation tick (`[aggregate]
  interval_seconds`, default 30 s); trades `(base_asset, quote_asset, ts DESC)`
  composite (migration 0001) plus `(base, quote, source, ts, ledger)` (0037).
