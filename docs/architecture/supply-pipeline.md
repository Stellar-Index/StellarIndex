---
title: Supply pipeline — three-algorithm derivation, per-asset refresh
last_verified: 2026-07-08
status: binding
---

# Supply pipeline

**Every supply value on `/v1/assets/{id}` flows through one path,
parameterised by one of three algorithms keyed on the asset class:**

```
operator config: [supply] sdf_reserve_accounts /
                          watched_classic_assets /
                          watched_sep41_contracts /
                          sac_wrappers
                                │
                                ▼
                     one supply.Refresher per asset
                                │
                                ▼
                  (Algorithm 1)  (Algorithm 2)  (Algorithm 3)
                  XLMComputer    ClassicComputer SEP41Computer
                  ▼              ▼               ▼
                  (reads)        (reads)         (reads)
   ┌──────────────────────┐  ┌────────────────┐ ┌─────────────────┐
   │ account_observations │  │ trustline_obs  │ │ sep41_supply_   │
   │  (XLM balances of    │  │ claimable_obs  │ │  events         │
   │   SDF reserves)      │  │ lp_reserve_obs │ │  (mint / burn / │
   │                      │  │ sac_balance_obs│ │   clawback)     │
   └──────────────────────┘  └────────────────┘ └─────────────────┘
                                │
                                ▼
                     supply.Supply struct
                                │
                                ▼
                     Store.InsertSupply
                                │
                                ▼
                  asset_supply_history (hypertable)
                                │
                                ▼
                     Store.LatestSupply
                                │
                                ▼
                     /v1/assets/{id} F2 fields:
                       total_supply
                       circulating_supply
                       max_supply
                       market_cap_usd  (× current price)
                       fdv_usd         (× current price)
                       supply_basis
```

## The three algorithms (per ADR-0011)

| Algorithm | Asset class       | Total derivation                              | ADR        |
|-----------|-------------------|-----------------------------------------------|------------|
| 1         | Native XLM        | frozen 50,001,806,812 × 10⁷ stroops          | ADR-0011 §1 |
| 2         | Classic credit    | Σ trustline + Σ claimable + Σ LP + Σ SAC     | ADR-0011 §2 |
| 3         | SEP-41 Soroban    | Σ mint − Σ burn − Σ clawback over lifetime   | ADR-0011 §3 |

**Circulating** (per ADR-0011) is `total − issuer/admin balance −
Σ operator-locked-set balances` for all three. The locked-set is
operator-curated via `supply.Policy.PerAsset`.

**Max supply** is `total` for hard-capped assets (XLM), nil
otherwise unless the operator supplies an override or the SEP-1
declaration overlay populates it. The overlay (`supply.Overlay`,
wired 2026-07-05) applies at the API **serving** layer, not at
snapshot-compute time: when the stored snapshot has no max, the
`/v1/assets/{id}` handler scales the issuer's stellar.toml
`max_number` / `fixed_number` (display units → raw units by asset
decimals; blocked by `is_unlimited = true`) and labels the result
`supply_basis: "sep1_declared_max"` so consumers can see the cap is
issuer-self-declared. `asset_supply_history` rows never carry
declared values.

## The six observers

Every component the algorithms read is sourced from one of six
LCM-stream observers. Each plugs into the dispatcher's hooks
without changing dispatcher source — they're pure additive
sources per the ingest-pipeline contract:

| Observer | Hook | Watched-set config | Backs |
|----------|------|--------------------|-------|
| `internal/sources/accounts` | `LedgerEntryChangeDecoder` | `[supply] sdf_reserve_accounts` (XLM) + per-issuer for metadata | Algorithm 1 + metadata overlay |
| `internal/sources/trustlines` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Algorithm 2 trustline component |
| `internal/sources/claimable_balances` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Algorithm 2 claimable component |
| `internal/sources/liquidity_pools` | `LedgerEntryChangeDecoder` | `[supply] watched_classic_assets` | Algorithm 2 LP-reserve component |
| `internal/sources/sac_balances` | `LedgerEntryChangeDecoder` | `[supply.sac_wrappers]` (contract→asset_key map) | Algorithm 2 SAC component + Algorithm 3 locked-set lookups |
| `internal/sources/sep41_supply` | `Decoder` (events) | `[supply] watched_sep41_contracts` | Algorithm 3 mint/burn/clawback running sum |

The first five are LCM ledger-entry observers (ADR-0021 +
ADR-0022). The sixth is an event-stream observer (ADR-0023) — it
classifies topics and accumulates amounts rather than reading
state.

All six observers are now wired into the indexer's dispatcher
(L2.12a closed via PRs #411 / #412 / #413). Registration is
opt-in per the corresponding `[supply]` watched-set —
`pipeline.RegisterSupplyEntryDecoders` handles the five
`LedgerEntryChangeDecoder`s (accounts / trustlines /
claimable_balances / liquidity_pools / sac_balances) keyed off
`sdf_reserve_accounts` / `watched_classic_assets` /
`[supply.sac_wrappers]`, and `pipeline.RegisterSupplyEventDecoders`
attaches sep41_supply when `watched_sep41_contracts` is non-empty.
Empty watched-set → observer skipped → no behaviour change. With
any watched-set populated, the corresponding hypertable starts
filling on every matching ledger close.

## The chained-fallback reader pattern

Per ADR-0021, the supply readers compose a "live LCM-derived
reader" with an "operator-static fallback" so the system works
during observer bootstrap:

```
supply.Refresher.Tick()
    │
    ▼
supply.<Algorithm>Computer.Compute(asset, ledger, observedAt)
    │
    ▼
supply.<Algorithm>SupplyReader.Read(asset, locked, ledger)
    │
    ▼
chain reader:
    1. try live: query account_observations / trustline_observations / etc.
    2. on ErrNoObservation: fall through to operator-static config
       (reserve_balances_stroops / per-asset locked-set / etc.)
    3. otherwise: bubble error
```

For Algorithm 1 (XLM) specifically: `supplyAggregatorChainReader`
in `cmd/stellarindex-aggregator/main.go` wraps
`supply.LCMReserveBalanceReader` (live) with
`supply.ConfigReserveBalanceReader` (static). When the
AccountEntry observer hasn't backfilled the SDF reserves yet,
the static config produces the answer; once the observer covers
the live set, the static config can be left stale (the live
reader wins).

Bootstrap caveat: the live observer only writes when an account
CHANGES — a dormant reserve account never emits a
`LedgerEntryChange`, so the chain would stay on the static map
indefinitely. `stellarindex-ops supply seed-observations` closes
that: a one-shot, idempotent seed of each
`[supply] sdf_reserve_accounts` entry's latest AccountEntry from
the lake's `ledger_entries_current` projection (ADR-0034), at the
account's true last-modified ledger. Later live observations
supersede seeded rows via the reader's at-or-before-ledger
ordering.

For Algorithms 2 + 3: similar pattern, but the static fallback
is per-component (operators populate
`reserve_balances_stroops` for XLM analogues; they DON'T
typically maintain manual trustline-component snapshots, so the
classic / SEP-41 paths require the observer to be backfilled).

## Two refresh paths (operator choice)

Per ADR-0011 / ADR-0021 / Task #57, operators have two paths to
write `asset_supply_history` rows:

### A. systemd-timer driven

`stellarindex-ops supply snapshot` subcommand, fired by
`deploy/systemd/supply-snapshot.timer` daily at 04:42 UTC. Per
[supply-snapshot runbook](../operations/supply-snapshot.md).

XLM only at v1; the CLI rejects classic + SEP-41 with a "use
the goroutine path" message.

Metrics: `stellarindex_supply_snapshot_*` textfile-emitted via
`internal/supply/textfile.go`. Alerts in
`deploy/monitoring/rules/supply-snapshot.yml`.

### B. Aggregator-resident goroutine

`[supply] aggregator_refresh_enabled = true` runs a
`supply.Refresher` goroutine per watched asset inside
`stellarindex-aggregator`. One goroutine per
`(XLM | classic asset | SEP-41 contract)` on the
`aggregator_refresh_cadence` (default 5m).

Covers all three algorithms. Per-tick outcome counter
`stellarindex_aggregator_supply_refresh_total{asset_key, outcome}`
labels by both asset and outcome so operators can chart per-
asset bootstrap progress + isolate failure modes per asset.
Alerts in `deploy/monitoring/rules/supply-refresh.yml`.

### Choice rules

- Classic + SEP-41 supply requires path B (the CLI doesn't
  support those assets).
- XLM supply works on either path. Path A is simpler (no
  aggregator dependency); path B is preferred when the LCM
  observer has backfilled (per-cadence freshness vs. per-day).

The two paths are mutually exclusive at the operator level —
write idempotency makes a double-fire correctness-safe (the
hypertable's `(asset_key, ledger_sequence)` PK and `ON CONFLICT
DO NOTHING` dedupe), but operators should disable one when
flipping to the other to avoid redundant work.

## Cross-check between Algorithm 2 and Algorithm 3

A SAC-wrapped classic asset's supply is observable two ways: as a
classic credit (Algorithm 2 — sums trustline + claimable + LP-reserve
+ SAC-wrapped contract balances) and as a SEP-41 token (Algorithm 3 —
sums mint − burn − clawback events on the SAC contract). Both observe
the same underlying ledger state through different lenses, but they
are NOT the same quantity for a partially-wrapped classic asset —
Algorithm 2's total includes classic-held supply that never touched
the SAC at all (e.g. AQUA: Algorithm 2 ≈ 86.4B, Algorithm 3 ≈ 0).

**2026-07-08 fix (BACKLOG #59):** applying ADR-0011's original
"agree within 1 stroop" equality compare unconditionally produced 8
standing false positives on exactly this shape of asset — a
monitoring category error, not indexer corruption (served supply was
always correct). The compare is now `internal/supply.WrapClass`-aware
per pair:

- **`WrapClassPartial`** (the default): checks a subset bound instead
  of equality — a SAC's `total_supply` (the wrapped amount) can never
  legitimately exceed its classic asset's `total_supply`, because
  `SACWrapped` is one of Algorithm 2's own non-negative addends
  (`total = Trustline + Claimable + LPReserve + SACWrapped`).
  `sac_total > classic_total` is impossible under correct accounting
  and fires; `sac_total ≤ classic_total` (the normal partially-wrapped
  state) does not.
- **`WrapClassFull`** (operator-attested via
  `[supply].fully_wrapped_sacs`; none configured as of 2026-07-08):
  keeps the ORIGINAL ADR-0011 equality compare, for a pair the
  operator has confirmed is 100% SAC-represented.

The REAL subset compare — Algorithm 2's `SACWrapped` component
(`ClassicSupplyComponents.SACWrapped`) vs Algorithm 3's `total_supply`,
which per ADR-0011/0022's own math IS a true equality (both measure
the same wrapped amount via independent data paths) — is not available
at the cross-check compare site today: `ClassicComputer.Compute` folds
`SACWrapped` into the classic `TotalSupply` before returning a
`Supply`, and only that folded total reaches `asset_supply_history`.
Wiring the real subset compare is a follow-up (BACKLOG #59), tracked
as needing either a persisted `sac_wrapped_stroops` column/`Supply`
field, or the refresher querying
`ClassicSupplyStore.SumSACBalancesAtOrBefore` directly instead of the
persisted snapshot.

The aggregator's `supply.CrossCheckRefresher`
(`internal/supply/crosscheck_refresher.go`, wired in
`cmd/stellarindex-aggregator/main.go::buildCrossCheckRefresher`) ticks
on the same `aggregator_refresh_cadence` as the per-asset supply
refreshers above. Pairs are derived at boot from the ∩ of:

- `[supply].sac_wrappers` (operator-declared classic↔SAC mapping)
- `[supply].watched_classic_assets` (Algorithm 2 watched-set)
- `[supply].watched_sep41_contracts` (Algorithm 3 watched-set)

...with each pair's `WrapClass` set to `WrapClassFull` when its SAC
contract id is also listed in `[supply].fully_wrapped_sacs`, else the
safe default `WrapClassPartial`.

Per tick, for each pair the refresher reads the latest snapshot for
both the classic and the SAC sides via `Store.LatestSupply`, runs
`supply.CrossCheckForClass` (dispatches to `supply.CrossCheck` for
`WrapClassFull` or `supply.CrossCheckSubsetBound` for
`WrapClassPartial`), and emits:

- `stellarindex_supply_cross_check_divergence_stroops{classic_key,wrap_class}` —
  gauge holding the stroop divergence on within/over outcomes (meaning
  depends on `wrap_class` — see above).
- `stellarindex_supply_cross_check_total{outcome,wrap_class}` —
  counter labelled by `within | over | missing_snapshot | read_error`.

The supply.yml alert (`stellarindex_supply_cross_check_divergence`)
fires when the gauge stays > 1 for ≥ 5 min — unchanged expression;
the false positives are fixed in what the gauge value MEANS, not in
the alert condition. Runbook:
[`supply-cross-check-divergence`](../operations/runbooks/supply-cross-check-divergence.md).

Empty pair-set is a no-op — operators that haven't declared any
SAC-wrapper pairs (e.g. an SEP-41-only deployment with no classic
side) get no gauge updates and no alerting noise.

## Per-class storage tables (live-data side)

| Table | Migration | Identity | Holders columns |
|-------|-----------|----------|-----------------|
| `asset_supply_history` | 0005 | `(asset_key, ledger_sequence)` | total / circulating / max / basis |
| `account_observations` | 0010 | `(account_id, ledger, observed_at)` | balance_stroops / home_domain / flags / seq_num / is_removal |
| `trustline_observations` | 0011 | `(account_id, asset_key, ledger, observed_at)` | balance_stroops / is_removal |
| `claimable_observations` | 0012 | `(claimable_id, ledger, observed_at)` | asset_key / balance_stroops / is_removal |
| `lp_reserve_observations` | 0013 | `(pool_id, asset_key, ledger, observed_at)` | balance_stroops / is_removal |
| `sac_balance_observations` | 0014 | `(contract_id, holder, ledger, observed_at)` | asset_key / balance_stroops / is_removal |
| `sep41_supply_events` | 0015 | `(contract_id, ledger, tx_hash, op_index, observed_at)` | event_kind / amount / counterparty |

All hypertables on `observed_at`, 7-day chunks, compression
segment-by the most-common reader-query column. PK convention
drags `observed_at` into the key per Timescale's partition-
column-in-PK rule.

## Reader contracts

Each algorithm has a `<X>SupplyReader` interface in
`internal/supply/`; the production impl is `Storage<X>SupplyReader`
composing the storage primitives:

| Reader | Composes |
|--------|----------|
| `XLMComputer.reader` (`ReserveBalanceReader`) | `LCMReserveBalanceReader` (account_observations) + `ConfigReserveBalanceReader` (operator-static) |
| `StorageClassicSupplyReader` | 4 × `Sum*BalancesAtOrBefore` + 2 × per-entity lookups (`TrustlineBalanceForAccountAtOrBefore`, `SACBalanceForContractAtOrBefore`) |
| `StorageSEP41SupplyReader` | `SEP41KindTotalsAtOrBefore` + `SACBalanceForContractAtOrBefore` (for locked-set lookups via shared SAC observer storage) |

Each reader returns a `<X>SupplyComponents` struct that the
matching `<X>Computer` reduces to a `Supply` snapshot.

## API surface

`/v1/assets/{id}` reads from `asset_supply_history` via
`Store.LatestSupply`; the F2 fields (`total_supply` /
`circulating_supply` / `max_supply` / `market_cap_usd` /
`fdv_usd` / `supply_basis`) populate when a row exists, stay
JSON null when no snapshot has been written (per ADR-0011 "we
don't fabricate"). The handler does NOT consult observer state
directly — the snapshot table is the API source of truth. Two
serving-time refinements sit on top of it:

- **SEP-41 lake fallback** — a Soroban token with no observer
  snapshot (i.e. not on the watched-set, the common case) serves
  the lake-derived Σmint−Σburn−Σclawback total
  (`supply_basis: "sep41_lake_flows"`, total == circulating).
- **SEP-1 max_supply overlay** — a snapshot with no max picks up
  the issuer's stellar.toml declaration
  (`supply_basis: "sep1_declared_max"`, see "Max supply" above).

## Failure modes (per outcome label)

The aggregator-refresh metric labels each tick with one of:

| Outcome | Means | Operator action |
|---------|-------|-----------------|
| `ok` | Snapshot written | none — steady state |
| `no_ledger` | `ListCursors` returned no max_ledger | wait for indexer's first cursor; check ingestion is alive |
| `no_observation` | Live reader has no row + static fallback empty | bootstrap window — wait for backfill OR populate static config |
| `missing_baseline` | SEP-41 total went negative AND the contract's pre-Soroban genesis baseline hasn't been seeded (a SAC-wrapper issued before Soroban, reading Σburn > Σmint over the Soroban-era-only window) | run `stellarindex-ops supply seed-sep41-genesis` once (idempotent). Benign — excluded from `error_dominant` |
| `compute_error` | Algorithm returned non-OK for a genuine reason (e.g. SEP-41 total negative **after** the genesis baseline is seeded — physically impossible) | code bug or upstream data inconsistency; check logs + roll back if recent deploy |
| `write_error` | `InsertSupply` failed | storage layer down; route to `pg-conns-saturated` runbook |

Sustained non-`ok` (excluding the benign `dormant` + `missing_baseline`)
for ≥ 30 min triggers
`stellarindex_aggregator_supply_refresh_error_dominant`; no `ok`
in ≥ 30 min triggers `_stalled`.

### SEP-41 / SAC-wrapper lifetime supply (pre-Soroban genesis baseline)

Algorithm 3 sums `sep41_supply_events` (Postgres), which the supply
observer fills only over the **Soroban era** `[50457424, tip]` —
contract events don't exist below the protocol-20 activation ledger.
A classic asset's SAC wrapper (VELO, AQUA, yXLM, LIBRE, ACT, MBC,
XAU, BTC, GQX, …) was largely **issued before Soroban existed**, so
over the Soroban-era-only window it reads `Σburn > Σmint` → negative
derived total → the negative-total guard rejects it (incident
2026-07-06).

The fix (migration 0088) seeds each watched contract's pre-Soroban
per-kind **opening balance** into `sep41_supply_rollup`
(`genesis_mint_total` / `genesis_burn_total` / `genesis_clawback_total`,
bounded by `genesis_baseline_ledger`) from the certified ClickHouse
lake (`stellar.supply_flows`, ADR-0034). The reader serves
`genesis(ledger < 50457424, CH) ⊕ soroban(ledger ≥ 50457424, PG)`, a
**disjoint ledger partition** — so lifetime total comes out correct +
positive, and a Soroban-only token (no pre-genesis flows) gets a zero
baseline and its served number is **unchanged** (no double-count). We
deliberately do **not** re-point the per-tick read at ClickHouse
(migration 0085's rationale): the CH lake is network-wide +
map/muxed-variant aware while the PG observer is watched-set-gated +
bare-i128, so their Soroban-era per-contract totals can legitimately
differ — CH is used only for the pre-Soroban slice PG has no data for.

Operator step: `stellarindex-ops supply seed-sep41-genesis -config PATH`
(idempotent; re-run after any lake re-derive below the boundary).

**Provenance (ADR-0033 substrate reproducibility).** The pre-Soroban
`contract_events` / `supply_flows` rows are **replay-derived**: a
post-P23 captive core synthesized the CAP-67 unified asset events for
classic history that predates them. They are legitimate,
on-chain-faithful data — but the exact event enumeration is
**core-version-dependent**, so the seeded baseline is a point-in-time
capture. `genesis_baseline_ledger` + `genesis_seeded_at` record the
boundary and capture time so a re-seed is auditable.

The cross-check refresher emits its own per-outcome counter, also
labelled by `wrap_class` (`partial_wrap` | `full_wrap` — 2026-07-08,
BACKLOG #59; see "Cross-check between Algorithm 2 and Algorithm 3"
above for what each class checks):

| Outcome | Means | Operator action |
|---------|-------|-----------------|
| `within` | Both snapshots loaded; divergence ≤ 1 stroop (`partial_wrap`: `sac_total ≤ classic_total + 1`; `full_wrap`: `\|classic_total − sac_total\| ≤ 1`) | none — steady state |
| `over` | Both snapshots loaded; divergence > 1 stroop | follow `supply-cross-check-divergence` runbook |
| `missing_snapshot` | One/both sides have no row in `asset_supply_history` yet | bootstrap window — no action unless sustained past first refresh of each side |
| `read_error` | Transient storage read failure | check `pg-conns-saturated` / `timescale-primary-down` runbooks |

Bootstrap-state (`missing_snapshot`) is intentionally NOT escalated
— it's the normal state during first-tick warmup and the first
moments after a new operator-watched asset is added. Sustained
`read_error` would surface via the same storage-layer alerts the
per-asset refreshers ride.

## ADR map

- [ADR-0011](../adr/0011-supply-algorithm.md) — three-algorithm
  spec
- [ADR-0021](../adr/0021-account-entry-observer.md) —
  AccountEntry observer for live home-domain + reserve balances
- [ADR-0022](../adr/0022-classic-supply-observers.md) —
  Trustline / Claimable / LP / SAC observers
- [ADR-0023](../adr/0023-sep41-supply-observer.md) — SEP-41
  supply event observer
- [ADR-0003](../adr/0003-i128-no-truncation.md) — i128 / NUMERIC
  end-to-end (every amount in this pipeline)
- [ADR-0006](../adr/0006-timescaledb-for-price-time-series.md) —
  TimescaleDB storage, the hypertable convention
- [ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md) —
  why the API serves CLOSED snapshots only

## Repo map

```
internal/sources/{accounts,trustlines,claimable_balances,liquidity_pools,sac_balances,sep41_supply}/
        ↓ (LedgerEntryChange or events.Event hooks)
internal/dispatcher/             (4 hooks: Decoder, OpDecoder, ContractCallDecoder, LedgerEntryChangeDecoder)
        ↓ (consumer.Event)
internal/pipeline/sink.go        (type-switch routing)
        ↓
internal/storage/timescale/      (Insert{Supply, AccountObservation, TrustlineObservation, …}, Sum*, Latest*)
        ↓
internal/supply/                 (XLMComputer, ClassicComputer, SEP41Computer, Refresher, CrossCheckRefresher, chained readers)
        ↓
cmd/stellarindex-aggregator/      (buildSupplyRefreshers + buildCrossCheckRefresher; runSupplyRefresh + runCrossCheckRefresh — one goroutine per asset, plus one for cross-check)
        ↓
internal/api/v1/assets_f2.go     (populateMarketCap, F2 field rendering)
        ↓
GET /v1/assets/{id}              (asset_supply_history via Store.LatestSupply)
```

## When to update this doc

Add a row, update a table, or extend the diagram when:

- A new algorithm class lands (no current candidates; the
  three above cover all on-chain Stellar supply types).
- A new observer plugs in (e.g. operator-watched-set expansion
  to issuer accounts triggering SEP-1 metadata refresh).
- A new operator-config knob materially changes the data flow.
- An ADR in the ADR map above supersedes another.

The matrix in `coverage-matrix.md` is the row-by-row tracker;
this doc is the architecture-level overview.
