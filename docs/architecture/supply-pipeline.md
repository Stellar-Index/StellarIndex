---
title: Supply pipeline and asset identity
last_verified: 2026-10-05
status: binding
---

# Supply pipeline

Every supply value on `/v1/assets/{id}` comes from one path, parameterised
by one of three algorithms keyed on asset class. Ingest mechanics (dispatcher
hooks, sink, replay) are in [ingest-pipeline.md](ingest-pipeline.md); the
system map is [overview.md](overview.md). Every amount here is
`canonical.Amount` / `*big.Int` in Go, `NUMERIC` in Postgres and a decimal
string in JSON (ADR-0003, AGENTS.md invariant 1).

```
[supply] sdf_reserve_accounts / watched_classic_assets / watched_sep41_contracts / sac_wrappers
        → one supply.Refresher per asset
        → XLMComputer (alg 1) | ClassicComputer (alg 2) | SEP41Computer (alg 3)
            reads: account_observations | trustline/claimable/lp_reserve/sac_balance obs | sep41_supply_events
        → supply.Supply → Store.InsertSupply → asset_supply_history (hypertable)
        → Store.LatestSupply → /v1/assets/{id} F2 fields: total_supply, circulating_supply,
          max_supply, market_cap_usd (× price), fdv_usd (× price), supply_basis
```

## The three algorithms (ADR-0011)

| Algorithm | Asset class | Total derivation | ADR |
|---|---|---|---|
| 1 | Native XLM | frozen 50,001,806,812 × 10⁷ stroops | ADR-0011 §1 |
| 2 | Classic credit | Σ trustline + Σ claimable + Σ LP + Σ SAC | ADR-0011 §2 |
| 3 | SEP-41 Soroban | Σ mint − Σ burn − Σ clawback over lifetime | ADR-0011 §3 |

**Circulating** = `total − issuer/admin balance − Σ operator-locked-set
balances` for all three; the locked set is operator-curated via
`supply.Policy.PerAsset`.

**Max supply** is `total` for hard-capped assets (XLM), else nil unless the
operator overrides it or the SEP-1 overlay (`supply.Overlay`) fills it. The
overlay applies at the API **serving** layer, not at snapshot time: when the
stored snapshot has no max, the `/v1/assets/{id}` handler scales the issuer's
stellar.toml `max_number` / `fixed_number` (display → raw units by asset
decimals; blocked by `is_unlimited = true`) and labels it
`max_supply_basis: "sep1_declared_max"`; `supply_basis` keeps naming the
circulating policy. `asset_supply_history` rows never carry declared values.

## The six observers

| Observer | Hook | Watched-set config | Backs |
|---|---|---|---|
| `internal/sources/accounts` | `LedgerEntryChangeDecoder` | `[supply] sdf_reserve_accounts` (every entry is subtracted from circulating) | Algorithm 1; the metadata overlay reads the same rows but has no watched-set key |
| `internal/sources/trustlines` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Alg 2 trustline component |
| `internal/sources/claimable_balances` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Alg 2 claimable component |
| `internal/sources/liquidity_pools` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Alg 2 LP-reserve component |
| `internal/sources/sac_balances` | `LedgerEntryChangeDecoder` | `[supply.sac_wrappers]` (contract → asset_key) | Alg 2 SAC component + Alg 3 locked-set lookups |
| `internal/sources/sep41_supply` | `Decoder` (events) | `[supply] watched_sep41_contracts` | Alg 3 mint/burn/clawback running sum |

The first five read ledger-entry state (ADR-0021, ADR-0022); `sep41_supply`
classifies events and accumulates amounts (ADR-0023). Registration is opt-in:
`pipeline.RegisterSupplyEntryDecoders` attaches the five entry observers when
their watched set is non-empty, `pipeline.RegisterSupplyEventDecoders`
attaches `sep41_supply` when `watched_sep41_contracts` is. Empty watched set →
observer skipped → no behaviour change.

### Removals (`IsRemoval`)

A removed ledger entry is written as an `is_removal = true` row with a zero
balance, never dropped. The `Sum*AtOrBefore` readers
(`internal/storage/timescale/classic_supply_observations.go`) take each
entity's latest row at or before the ledger and exclude it when that row is a
removal, so a closed trustline, claimed balance or withdrawn pool stops
counting. A removed claimable balance or LP entry carries no asset in its key;
`claimable_balances` and `liquidity_pools` resolve it from the same-ledger
STATE pre-image (`dispatcher_adapter.go` memo), and an unattributable removal
is a decode error, not a silent drop. Pinned by
`internal/sources/{claimable_balances,liquidity_pools}/removal_supply_test.go`
([45b findings](../operations/45b-verify-first-findings.md#gated--larger-items)).

## The chained-fallback reader pattern

Per ADR-0021, each reader composes a live LCM-derived reader with an
operator-static fallback so the system works during observer bootstrap:

```
supply.Refresher.Tick() → <Algorithm>Computer.Compute(asset, ledger, observedAt)
  → <Algorithm>SupplyReader.Read(asset, locked, ledger) → chain reader:
      1. live: account_observations / trustline_observations / …
      2. on ErrNoObservation: operator-static config (reserve_balances_stroops / per-asset locked set)
      3. otherwise: bubble the error
```

For XLM, `supply.NewChainedReserveBalanceReader` (`cmd/stellarindex-aggregator/main.go`)
wraps `supply.LCMReserveBalanceReader` (live) with
`supply.ConfigReserveBalanceReader` (static); once the observer covers the
reserves the live reader wins and the static map may go stale.

Bootstrap caveat: the live observer writes only when an account CHANGES, so a
dormant reserve account would keep the chain on the static map forever.
`stellarindex-ops supply seed-observations` fixes that: a one-shot, idempotent
seed of each `[supply] sdf_reserve_accounts` entry's latest AccountEntry from
the lake's `ledger_entries_current` (ADR-0034), at the account's true
last-modified ledger. Later live rows supersede it via at-or-before ordering.

For Algorithms 2 and 3 the static fallback exists per component, but operators
do not maintain manual trustline snapshots, so those paths need the observer
backfilled (or seeded: `supply seed-claimable-balances`,
`supply seed-sac-balances`; trustlines are seeded from ledger 31.8M).

## Two refresh paths

### A. systemd timer

`stellarindex-ops supply snapshot`, fired by
`deploy/systemd/supply-snapshot.timer` daily at 04:42 UTC
([runbook](../operations/supply-snapshot.md)). XLM only; the CLI rejects
classic and SEP-41 with a "use the goroutine path" message. Metrics
`stellarindex_supply_snapshot_*` are textfile-emitted via
`internal/supply/textfile.go`; alerts in
`deploy/monitoring/rules/supply-snapshot.yml`.

### B. Aggregator goroutine

`[supply] aggregator_refresh_enabled = true` runs one `supply.Refresher` per
watched asset (XLM | classic asset | SEP-41 contract) inside
`stellarindex-aggregator` on `aggregator_refresh_cadence` (default 5m). Covers
all three algorithms. Per-tick counter
`stellarindex_aggregator_supply_refresh_total{asset_key, outcome}`; alerts in
`deploy/monitoring/rules/supply-refresh.yml`.

### Choice rules

- Classic and SEP-41 supply require path B.
- XLM works on either. A is simpler (no aggregator dependency); B is preferred
  once the observer has backfilled (per-cadence vs per-day freshness).
- Run one path, not both. A double fire is correctness-safe — the
  `asset_supply_history_asset_ledger_idx` unique constraint upserts, and a row
  is replaced only by an equal or higher `derive_generation` (migration
  0109) — but it is redundant work.

### Corrective re-derive and `supply_1d`

`supply snapshot` stamps a positive `derive_generation`, so `-ledger N`
re-derives that ledger's row in place. The market-cap-over-time chart reads the
`supply_1d` continuous aggregate (`DailyCirculatingSupply`), whose refresh
policy (migration 0066) looks back only 7 days. When the snapshot's UTC day is
more than 6 days old, `persistSnapshot` (`internal/ops/supply/supply.go`)
refreshes `supply_1d` over that day ± 1 day. A failed refresh fails the run
non-zero: the row is written but not served and nothing else folds it in, so
re-run the same `-ledger` snapshot (idempotent) until the refresh succeeds.
Path B writes at the latest ledger only, inside the policy window.

## Cross-check between Algorithm 2 and Algorithm 3

A SAC-wrapped classic asset is observable as a classic credit (Alg 2: trustline
+ claimable + LP + SAC balances) and as a SEP-41 token (Alg 3: SAC mint − burn
− clawback). These are NOT the same quantity for a partially wrapped asset: Alg
2 includes classic-held supply that never touched the SAC (AQUA: Alg 2 ≈
86.4B, Alg 3 ≈ 0). ADR-0011's original "agree within 1 stroop" equality,
applied to every pair, produced 8 standing false positives on exactly this
shape — a monitoring category error; served supply was always correct. The
compare is therefore `supply.WrapClass`-aware:

- **`WrapClassPartial`** (default) — `supply.CrossCheckSubsetBound` computes
  two legs:
  - *Over-mint* `max(0, sac_total − classic_total)`, reported as
    `OverMintStroops` for triage only, never pages. It legitimately diverges
    when wrapped units are retired classically (BLND) or a one-time SAC mint is
    distributed classic-side (PHO).
  - *Escrow-exceeds-minted* `max(0, SACWrapped − sac_total)` feeds the
    divergence gauge. Alg 2's `SACWrapped` (ledger-entry sum of SAC balances)
    and Alg 3's `total_supply` (event-derived net mint) see the same escrow
    through independent paths, and every escrowed unit got there by a mint, so
    escrow above net mint means missed mints or double-counted burns.
- **`WrapClassFull`** (operator-attested via `[supply].fully_wrapped_sacs`) —
  keeps the equality compare for a pair confirmed 100% SAC-represented; the
  equality implies the escrow bound, so leg 2 is not evaluated separately.

Leg 2 reads `SACWrapped` from the persisted classic snapshot:
`ClassicComputer.Compute` folds it into `TotalSupply` and also carries it as
`Supply.SACWrappedStroops` → `asset_supply_history.sac_wrapped_stroops`
(migration 0117). Querying `ClassicSupplyStore.SumSACBalancesAtOrBefore` from
the refresher instead would compare a component taken at an unrelated ledger
and open a second read path to the same number. The column is NULL for a
pre-0117 row and for an asset with no `sac_balance_observations` row at or
before the snapshot ledger, and is **never defaulted to zero**: `0 ≤ sac_total`
holds vacuously, so a zero would publish a check that verified nothing. Leg 2
is an upper bound on escrow, not proof escrow was fully observed — an
under-counted `SACWrapped` (the dormant case below) passes quietly.

`supply.CrossCheckRefresher` (`internal/supply/crosscheck_refresher.go`, wired
in `cmd/stellarindex-aggregator/main.go::buildCrossCheckRefresher`) ticks on
`aggregator_refresh_cadence`. Pairs are derived at boot from the intersection
of `[supply].sac_wrappers`, `[supply].watched_classic_assets` and
`[supply].watched_sep41_contracts`; a pair is `WrapClassFull` when its SAC is in
`[supply].fully_wrapped_sacs`, else `WrapClassPartial`. Each tick reads both
sides' `Store.LatestSupply`, runs `supply.CrossCheckForClass` (→
`supply.CrossCheck` or `supply.CrossCheckSubsetBound`) and emits:

- `stellarindex_supply_cross_check_divergence_stroops{classic_key,wrap_class}`
  — the divergence on within/over outcomes; deleted on every other outcome, so
  never stale.
- `stellarindex_supply_cross_check_total{outcome,wrap_class}`.

Alert `stellarindex_supply_cross_check_divergence` (`deploy/monitoring/rules/supply.yml`) fires when the gauge stays
> 1 for ≥ 5 min ([runbook](../operations/runbooks/supply.md#stellarindex_supply_cross_check_divergence)).
An empty pair set is a no-op.

### Dormant contract-held SAC balances (the current-state coverage floor)

Four pairs — **BLND, EURC, KALE, PHO** — kept alerting `over` with
`sac_total > classic_total` after the subset-bound fix, which is impossible
under correct accounting: Alg 2 was under-counting. Alg 3 was verified
stroop-exact against the lake and stellar.expert. The gap: Phoenix / Blend
**pool contracts** received the SAC token via ordinary SEP-41 `transfer`
before the `sac_balances` observer's window opened and before
`ledger_entries_current` existed, and those `Vec(Symbol("Balance"),
Address(pool))` entries on the **SAC's own** storage have been dormant since.
Rollup-vs-lake reconciliation refuted a pool-internal-accounting hypothesis; no pool-specific reader is needed.

The default seed (`supply seed-sac-balances` →
`clickhouse.StreamSACBalanceSeeds`) scans `stellar.ledger_entries_current`, a
materialized view fed by `stellar.ledger_entries_current_mv`. An MV processes
only rows inserted after it was created, so rows ch-backfilled into
`ledger_entry_changes` below ~ledger 62,000,000 never reached it: the
current-state **projection** has a floor even though the append-log substrate
is complete to genesis (ADR-0034).

Fix: `clickhouse.StreamSACBalanceSeedsFullHistory`
(`internal/storage/clickhouse/sac_balance_seed.go`) reads
`stellar.ledger_entry_changes` with
`ORDER BY key_xdr, ledger_seq DESC LIMIT 1 BY key_xdr` and reuses the same row
decoder (`sacBalanceSeedFromRow`), so only the read location differs. It is far
heavier than the default: **run it under `run-heavy-job.sh` on r1**, for the
small `[supply.sac_wrappers]` set only, never as a routine job.

```sh
stellarindex-ops supply seed-sac-balances -config PATH -full-history -dry-run   # preview
stellarindex-ops supply seed-sac-balances -config PATH -full-history -write
# prints: SEED  <contract> <asset_key>  holders=N  sum=<stroops>
```

The next Alg 2 tick picks the recovered balances up; nothing between the
hypertable and the API changes.

**Provenance** (`sac_balance_seed_provenance`, migration 0102; not a
hypertable, PK `contract_id`). Each non-dry-run pass upserts `source`
(`current_state` | `full_history`), `holders_seeded`, `min_ledger_seen` /
`max_ledger_seen` (the holders' own last-modified ledgers) and, since migration
0183, `holders_retracted` and `lake_verified_through`. A `full_history` row is
stamped only after the walk proved `stellar.ledgers` contiguous and hash-linked
through `lake_verified_through`; one with that column NULL predates the check
and the TTL-archival filter and is not evidence. On a verified row,
`min_ledger_seen` well below 62,000,000 proves the floor gap was reached. The
table is audit-only — never read by `ClassicSupplyAt` /
`SumSACBalancesAtOrBefore` / `Supply` — and separates "never full-history
seeded" from "seeded and still diverging".

Post-seed verification (provenance and observation queries, `supply audit -cross-check`) is in the [runbook](../operations/runbooks/supply.md#stellarindex_supply_cross_check_divergence).

### LP reserve history cutoff

`lp_reserve_observations` starts at ledger 63,300,828 (observer deploy) and was
never seeded from history — an accepted limit, not a pending fix. So Alg 2's LP
component is absent for any `as_of` below 63,300,828, and a pool unchanged since
then is missing from the current total. That is small because every swap or
deposit re-observes its pool: on 2026-07-27 AQUA's LP total was 516,524,268
across 1,072 pools vs Horizon's 517,261,343 across 1,303 (−0.14%; the 231
missing pools were dust). Claimable balances differ — written once, never
changed, so the live window missed most — hence `supply seed-claimable-balances`.
If a dormant-pool audit ever shows a material gap, build an LP seed of the same
shape: read the lake's `liquidity_pool` entries and upsert through the live
observer's SQL.

## SEP-41 / SAC-wrapper lifetime supply (pre-Soroban genesis baseline)

`sep41_supply_events` covers only the Soroban era `[50457424, tip]`. A SAC
wrapper whose classic asset was issued before Soroban (VELO, AQUA, yXLM, …)
reads `Σburn > Σmint` over that window → negative total → rejected. Migration
0088 seeds each watched contract's pre-Soroban per-kind opening balance into
`sep41_supply_rollup` (`genesis_mint_total` / `genesis_burn_total` /
`genesis_clawback_total`, bounded by `genesis_baseline_ledger`) from the lake's
`stellar.supply_flows`. The reader serves
`genesis(ledger < 50457424, CH) ⊕ soroban(ledger ≥ 50457424, PG)` — a disjoint
partition, so a Soroban-only token gets a zero baseline and is unchanged.

We deliberately do **not** re-point the per-tick read at ClickHouse (migration
0085's rationale): the lake is network-wide and map/muxed-variant aware while
the PG observer is watched-set-gated and bare-i128, so their Soroban-era
per-contract totals can legitimately differ. CH supplies only the slice PG has
no data for.

```sh
stellarindex-ops supply seed-sep41-genesis -config PATH -write   # no -write = preview; idempotent
```

Re-run after any lake re-derive below the boundary. Each write also rebuilds
the contract's rollup fold under the seeded floor in the same transaction,
which repairs a contract the aggregator folded at floor 0 before its first seed
(that fold would count the pre-boundary band twice). The fold never drops back
to `last_ledger = 0`, so serving stays on its checkpoint path. Rebuilds run one
contract at a time and hold the contract's rollup row; an aggregator pass that
hits it yields after a 10 s `lock_timeout` (an `error` outcome on
`stellarindex_sep41_supply_rollup_advances_total`) and retries next cadence. A
seed that finds the row held fails the same way — re-run it.

The pre-Soroban `contract_events` / `supply_flows` rows are replay-derived (a
post-P23 captive core synthesised CAP-67 events for older classic history):
on-chain-faithful but core-version-dependent, so the baseline is a
point-in-time capture; `genesis_baseline_ledger` + `genesis_seeded_at` make a
re-seed auditable (ADR-0033).

## Lumen conservation

`stellarindex-ops verify-network-state` (daily, `verify-network-state.timer`)
check `lumens`: native XLM in accounts, claimable balances, liquidity pools and
native-SAC balances, plus `fee_pool`, must equal the ledger header's
`total_coins` exactly (`NetworkStateReader.LumenConservation`,
`internal/storage/clickhouse/network_state.go`; residual = held + fee_pool −
total_coins). It reads `ledger_entries_current` at the newest committed ledger
and rolls newer keys back to their pre-image so concurrent ingest cannot tear
the sum. A non-zero residual adds 1 to the exit code; alerts
`stellarindex_network_state_verify_failed` / `_stale`
(`deploy/monitoring/rules/storage.yml`).

## Storage tables

| Table | Migration | Identity | Columns |
|---|---|---|---|
| `asset_supply_history` | 0005 | `(asset_key, ledger_sequence)` | total / circulating / max / basis |
| `account_observations` | 0010 | `(account_id, ledger, observed_at)` | balance_stroops / home_domain / flags / seq_num / is_removal |
| `trustline_observations` | 0011 | `(account_id, asset_key, ledger, observed_at)` | balance_stroops / is_removal |
| `claimable_observations` | 0012 | `(claimable_id, ledger, observed_at)` | asset_key / balance_stroops / is_removal |
| `lp_reserve_observations` | 0013 | `(pool_id, asset_key, ledger, observed_at)` | balance_stroops / is_removal |
| `sac_balance_observations` | 0014 | `(contract_id, holder, ledger, observed_at)` | asset_key / balance_stroops / is_removal |
| `sep41_supply_events` | 0015 | `(contract_id, ledger, tx_hash, op_index, observed_at)` | event_kind / amount / counterparty |

All are hypertables on `observed_at`, 7-day chunks, compression segment-by the
most common reader column; `observed_at` is in the PK per Timescale's
partition-column rule.

## Reader contracts

| Reader | Composes |
|---|---|
| `XLMComputer.reader` (`ReserveBalanceReader`) | `LCMReserveBalanceReader` (account_observations) + `ConfigReserveBalanceReader` (static) |
| `StorageClassicSupplyReader` | 4 × `Sum*BalancesAtOrBefore` + `TrustlineBalanceForAccountAtOrBefore`, `SACBalanceForContractAtOrBefore` |
| `StorageSEP41SupplyReader` | `SEP41KindTotalsAtOrBefore` + `SACBalanceForContractAtOrBefore` (locked-set lookups) |

Each returns a `<X>SupplyComponents` struct that the matching `<X>Computer`
reduces to a `Supply`.

## API surface

`/v1/assets/{id}` reads `asset_supply_history` via `Store.LatestSupply`; the F2
fields are JSON null when no snapshot exists (ADR-0011: we don't fabricate).
The handler never reads observer state directly. Three serving-time refinements:

- **SEP-41 lake fallback** — a Soroban token with no observer snapshot (not on
  the watched set, the common case) serves the lake-derived
  Σmint−Σburn−Σclawback (`supply_basis: "sep41_lake_flows"`, total ==
  circulating).
- **Classic lake fallback** (`classic_lake_supply.go`, `higherClassicSupply`)
  — a classic asset serves the higher of the lake-derived flows
  (`supply_basis: "classic_lake_flows"`) and the trustline sum
  (`classic_trustline_sum`, blind to three holding domains).
- **Contract storage balances** (`asset_supply.go`) — Σ of a contract's
  per-holder balance entries (`contract_storage_balances`).
- `classic_trustline_sum` and `contract_storage_balances` are lower bounds:
  `Basis.LowerBound()` (`internal/supply/supply.go`) sets
  `circulating_supply_lower_bound`.
- **SEP-1 max_supply overlay** — see "Max supply" above.

## Failure modes

Aggregator-refresh outcomes:

| Outcome | Means | Action |
|---|---|---|
| `ok` | snapshot written | none |
| `no_ledger` | no `ledgerstream` cursor, or no `stellar.ledgers` row in the 512 ledgers below it (the lookup window and the clamp's whole range) | wait for the first cursor; else check the CH sink — the normal cursor-ahead-of-lake lead is clamped away, so this means the lake is empty, gapped or stalled |
| `no_observation` | live reader has no row and static fallback is empty | bootstrap — wait for backfill or populate static config |
| `missing_baseline` | SEP-41 total negative and the pre-Soroban genesis baseline is not seeded | `stellarindex-ops supply seed-sep41-genesis -write`; benign, excluded from `error_dominant` |
| `compute_error` | genuine non-OK (e.g. negative SEP-41 total **after** the baseline is seeded) | code bug or upstream inconsistency; check logs, roll back a recent deploy |
| `write_error` | `InsertSupply` failed | storage down; `postgres.md` runbook |
| `stale_component` | a component observation lags the snapshot ledger past the threshold and moved since last tick, or stayed frozen past `DefaultMaxDormantComponentLedgers` | rejected; check the lagging observer |
| `missing_freshness` | strict mode and `MinComponentLedger == 0` (no freshness anchor) | rejected rather than published unanchored; check the observer |
| `dormant` | component lags but is unchanged tick-over-tick; the last observation is re-stamped (snapshot inserted) | benign per asset; many assets dormant together means a producer stalled (`stellarindex_aggregator_supply_refresh_dormant_fleet`) |
| `static_reserve` | snapshot inserted, but reserves came from the static map, not the live observer | not benign: counts toward `error_dominant`; seed with `supply seed-observations` |

Sustained non-`ok` (excluding benign `dormant` and `missing_baseline`) for
≥ 30 min fires `stellarindex_aggregator_supply_refresh_error_dominant`; no `ok`
in ≥ 30 min fires `stellarindex_aggregator_supply_refresh_stalled`.

Cross-check outcomes (`wrap_class` = `partial_wrap` | `full_wrap`):

| Outcome | Means | Action |
|---|---|---|
| `within` | divergence ≤ 1 stroop (`partial_wrap`: `SACWrapped ≤ sac_total + 1`; `full_wrap`: `\|classic_total − sac_total\| ≤ 1`) | none |
| `over` | divergence > 1 stroop | `supply.md` runbook |
| `missing_snapshot` | a side has no `asset_supply_history` row yet | normal on first tick |
| `read_error` | transient storage read failure | `postgres.md` runbook |
| `misaligned` | snapshots > 1000 ledgers apart (one refresher stalled) | `supply-refresh-stalled` for the stale side |
| `unchecked` | `partial_wrap` pair whose classic snapshot has no `sac_wrapped_stroops`; leg 2 skipped, gauge cleared (`supply audit -cross-check` prints `UNCHECKED`) | no alert; to check it, `supply seed-sac-balances` (`-full-history` for dormant holders) then `supply audit -cross-check` |

`missing_snapshot`, `read_error` or `misaligned` sustained over an hour fires
`stellarindex_supply_cross_check_unevaluable`
([runbook](../operations/runbooks/supply.md#stellarindex_supply_cross_check_unevaluable)), because
the divergence alert cannot fire for a pair it is not evaluating.

## Asset identity

- An asset is keyed on `(code, issuer)`, a SAC contract id, or `native` —
  never on code alone, which is an impersonation vector. XLM has three
  identities (`native`, `crypto:XLM`, its SAC); every asset-id read path loops
  `canonical.AssetAliases`. Evidence: [domain-traps.md](domain-traps.md).
- `/v1/assets` is the one asset surface. `/v1/coins` and `/v1/currencies` were
  removed; every served entity is an asset with an `asset_class`
  (`fiat`, `stablecoin`, `crypto`, …) from the verified-currency catalogue.
  Stablecoin is distinct from fiat so a depeg stays visible.
- `/v1/assets/{slug}` returns `GlobalAssetView` for a catalogue slug and
  `AssetDetail` for a canonical asset id (wire shapes in domain-traps.md).
  Friendly slugs resolve **only** for catalogue entries and are never
  generated from observed codes — that is the ticker-collision phishing
  surface.
- The catalogue (`internal/currency/data/seed.yaml`) is hand-vetted and is
  never auto-populated from CoinGecko / CMC; adding a currency is a code
  change. A CoinGecko augmentation worker was planned and dropped for that
  reason. Non-Stellar entries (BTC, ETH, …) are `reference_only`: pricing
  references, not browseable assets. The cross-chain `networks[]` model was
  removed.
  `internal/canonical/asset_fiat.go` stays the source of truth for allowed
  fiat codes; the catalogue refers to fiat by ISO code.
- `CoinGeckoIDs()` feeds the CoinGecko poller's `TickerToID`, and
  `aggregatorPairsFromCatalogue` feeds the aggregator pairs
  (`cmd/stellarindex-indexer/main.go`), so adding a currency with a
  `coingecko_id` widens polling. Aggregator prices live in `oracle_updates`;
  there is no `aggregator_prices` table.
- **Ticker-collision warning.** An asset whose code matches a verified ticker
  but whose issuer is not the verified one gets
  `flags.unverified_ticker_collision: true` and an `unverified_warning`
  pointing at the verified asset. It is independent of the scam-issuer
  registry; both can fire on one asset.
- **Global price** (`aggregate.ComputeGlobalPrice`) walks three tiers and
  reports `price_authority` + `price_sources` (`class` is the wire field;
  `asset_class` is only the query parameter): `vwap_native` (our `prices_1m` VWAP,
  wins at `trade_count >= VWAPMinTradeCount`, default 5), `aggregator_avg`
  (mean of fresh `Class:Aggregator` observations, `MaxAggregatorAge` default
  10m; aggregators never feed VWAP, to avoid double counting), then
  `triangulated` (Redis implied VWAPs). A storage error short-circuits — only
  "no rows" falls to the next tier, so a transient failure never silently
  degrades the authority. A cross-chain ticker-bucketed VWAP was not built: it
  needs per-trade FX-anchor conversion and only means something once
  non-Stellar trades are ingested. `price_authority` has three more values
  outside that walk: `reference_rate` (fiat, via `fiatUSDPriceFor`, not
  `ComputeGlobalPrice`), `identity` (USD) and `onchain_listing` (fallback for
  a Stellar-only token, `assets_global.go`).

## Open items

| Item | Inventory |
|---|---|
| `GlobalAssetView` has only `market_cap_usd` / `circulating_supply` (no ath / atl / change_24h_pct), and fills them for fiat only; crypto and stablecoin rely on the per-asset F2 fields | INV-1119 (blocked) |
| The lake wrote persistent `contract_data` / `contract_code` evictions as `removed`; `emitEvictions` (`internal/storage/clickhouse/extract_entry_changes.go`) now keeps an archived entry current, since it stays restorable from the hot archive. Already-written lake rows still need repair, and current-state reads (seeds, lumen conservation) inherit the error until then | INV-2532 (in progress) |
| Lumen conservation residual +119,100,885,352 stroops at ledger 64,767,268: the lake holds 11,910 XLM the network does not count. Candidate causes (evictions, a non-native SAC leak, the CAP-76 rule) are undiagnosed | INV-2533 (open) |

## ADR map

[ADR-0011](../adr/0011-supply-algorithm.md) three algorithms ·
[ADR-0021](../adr/0021-account-entry-observer.md) AccountEntry observer ·
[ADR-0022](../adr/0022-classic-supply-observers.md) trustline / claimable / LP / SAC observers ·
[ADR-0023](../adr/0023-sep41-supply-observer.md) SEP-41 observer ·
[ADR-0003](../adr/0003-i128-no-truncation.md) i128 / NUMERIC end to end ·
[ADR-0006](../adr/0006-timescaledb-for-price-time-series.md) hypertable convention ·
[ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md) closed snapshots only.

## Code map

`internal/sources/{accounts,trustlines,claimable_balances,liquidity_pools,sac_balances,sep41_supply}/`
→ `internal/dispatcher/` → `internal/pipeline/sink.go` →
`internal/storage/timescale/` → `internal/supply/` (computers, `Refresher`,
`CrossCheckRefresher`, chained readers) → `cmd/stellarindex-aggregator/`
(`buildSupplyRefreshers`, `buildCrossCheckRefresher`) →
`internal/api/v1/assets_f2.go` (`populateMarketCap`) → `GET /v1/assets/{id}`.
Row-by-row coverage lives in [coverage-matrix.md](coverage-matrix.md); disk
and lake trade-offs in [storage-considerations.md](storage-considerations.md).
