---
title: Blend oracle-aggregator WASM audit
last_verified: 2026-11-28
status: source-checked — 2 immutable builds; config decoded from real entries of all 4 deployed adapters
---

# Blend oracle-aggregator WASM audit

Audit log for the SEP-40 oracle adapters that Blend and BLNT pools point at. Unlike the other logs in this
directory it gates no ingest source: it gates a read path. `internal/sources/blend/oracleagg` reproduces an
adapter's `lastprice(asset)` and `decimals()` only for a contract whose current WASM hash is one of the two
below (`ProgramForHash`); any other hash is `oracle_unsupported`.

## Status

**Source-checked 2026-11-28.** Both builds are Blend Capital's
[`oracle-aggregator`](https://github.com/blend-capital/oracle-aggregator), byte-identical to the upstream release
assets (`shasum -a 256` of the deployed bytes against `gh release download`). Neither exports an upgrade
function or imports `update_current_contract_wasm`, so a contract's program is fixed for its life; the read path
still checks the hash on every (cached) config read.

## Builds

| WASM hash | version | upstream release | SDK | interface |
| --- | --- | --- | --- | --- |
| `41df04894bf2d8291492326f7a6ab9c90a90cc9828e31990ce8aeeab42680bfd` | v2.0.0 | `oracle-aggregator_v2.0.0.wasm` (`v2.0.0_oracle-aggregator_cli22.0.1`) | soroban-sdk 22.0.7 | [`evidence/oracle-aggregator/oracle-41df04894bf2d829.interface.txt`](evidence/oracle-aggregator/oracle-41df04894bf2d829.interface.txt) |
| `a7463634674ca764401e713e4e29aef4e8521877f76ab78b1e91bea9d46ec6bb` | v1.0.0 | `oracle-aggregator_v1.0.0.wasm` (`v1.0.0_oracle-aggregator_v1.0.0.wasm`) | soroban-sdk 20.5.0 | [`evidence/oracle-aggregator/oracle-a7463634674ca764.interface.txt`](evidence/oracle-aggregator/oracle-a7463634674ca764.interface.txt) |

Deployed adapters (real entries pinned in `test/fixtures/lending_oracle/`):

| adapter | build | Base | underlying feed |
| --- | --- | --- | --- |
| `CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS` | v2.0.0 | `Other(USD)` | Reflector CEX `CAFJZQWS…` |
| `CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36` | v2.0.0 | `Stellar(USDC SAC)` | Reflector DEX `CALI2BYU…` |
| `CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y` | v1.0.0 | `Other(USD)` | Reflector CEX |
| `CCPXGHMVZROX7G6OIP4VBN7FJO6KJAMASNQXLISC3QI52EQVX3VKIWO6` | v1.0.0 | `Other(USD)` | Reflector CEX |

## Configuration layout

Read from the adapter's own current entries by exact key; an unknown storage key or struct field fails the
decode (`ErrConfig`), it is never skipped.

- **v2.0.0** (`storage.rs`): instance storage only. `Admin`, `Base: Asset`, `Decimals: u32`, `MaxAge: u64`,
  `Oracles: Vec<OracleConfig{address, decimals, index, resolution}>`, `Assets: Map<Asset, AssetConfig{asset,
  max_dev, oracle_index}>`, and `BaseAssets: Vec<Asset>` once `add_base_asset` runs. An oracle's `decimals` and
  `resolution` are cached at `add_oracle`.
- **v1.0.0** (`storage.rs`): instance `Admin`, `IsInit`; persistent `Base`, `Assets`, `Decimals`,
  `AssetConfig(asset) = OracleConfig{asset, decimals, oracle_id, resolution}` per asset, and `Blocked(asset)`
  once `block` / `unblock` runs (absent reads false, `get_persistent_default`).

## Recipe

`oracleagg.LastPrice` is a pure function of the decoded config, the reserve asset, the ledger close time `t` and
the underlying feed's rounds. Each rule and the upstream line it reproduces:

| rule | upstream |
| --- | --- |
| v2: `asset == Base` or in `BaseAssets` → `10^Decimals` at `t` | v2 `contract.rs:85-93` |
| v2: unconfigured asset panics `AssetNotFound` | v2 `contract.rs:97-105` |
| v2: walk `T = R, R−res, …` while `T >= t − MaxAge` (900 s old is accepted); first period with the asset wins; `R` = feed `last_timestamp()` | v2 `price_data.rs:12-21` |
| v2: `0 < max_dev < 100`: first earlier period within `MaxAge / res` further lookups, not bounded by `t − MaxAge`; none → None; `\|p − old\| > old·max_dev/100` (truncating) → None | v2 `price_data.rs:26-50` |
| rescale to `Decimals` by truncating division (or multiplication) | v2 `price_data.rs:62-74`, v1 `price_data.rs:4-20` |
| v1: unconfigured asset panics `AssetNotFound`; `Blocked(asset)` panics `AssetBlocked` | v1 `contract.rs:38-45` |
| v1: feed `lastprice`: no round, or last round after `t`, is None; the last round only, no walk back | Reflector v7.1.0 `prices.rs:19-58` |
| v1 on Reflector v6.0.x: also None when `t·1000 − R >= 2·resolution` | Reflector v6.0.0 `prices.rs:19-31` (v6.0.1 identical) |
| feed `price(asset, T)`: `T` normalised down to the feed resolution; no price after the last round or more than 255 periods before it | Reflector v7.1.0 `price_oracle.rs:211-220`, `prices.rs:32-58` |

Verdicts, one per None or panic: `oracle_asset_missing`, `oracle_asset_blocked`, `oracle_price_stale`,
`oracle_price_absent` (v1: the asset sat out the last round), `oracle_deviation`,
`oracle_deviation_unverifiable` (v2: no earlier period), plus `oracle_unsupported` and `oracle_read_failed`.

## Read path

`poolOraclePrices` (`internal/api/v1/lending_oracle_price.go`), per pool:

1. The oracle's current WASM hash (`ExplorerReader.ContractWasmHash`) must be one of the two builds; the hash, the
   decoded config and each feed's build are cached per (server, oracle) and re-read whenever the lake watermark
   moves. A read failure is `oracle_read_failed` and is not cached.
2. The evaluation ledger is the lake watermark, held back to each feed's `projector` cursor when that is behind,
   since an unwritten round would make the recipe read an older one; its close time is `t`.
3. Feed rows come from `Store.OracleFeedRounds`, keyed on the feed CONTRACT from the adapter config, every stored
   identity of the feed asset (`canonical.AssetAliasStrings`), a bounded `ts` window and `ledger <= L`. The feed's
   last round counts every row of the contract, raw ones included; a raw row never prices, and a feed symbol that
   maps only to `raw:` is `oracle_unsupported`. A v1 window is 24 h, the pool's own price bound.
4. The quote is the adapter's base, served verbatim: `fiat:USD`, or the USDC SAC for `CDN7JD6S`.

Verdicts per pool and reason: `stellarindex_lending_oracle_verdicts_total{pool,reason}`.

## Not verified, and modelled as explicit inputs

- **Feed resolution.** Reflector normalises by its own live resolution; the adapter steps by the copy it cached.
  Both are 300 s at `add_oracle`; the live value is not ingested, so the feed resolution is the repo's
  `reflector.DefaultResolutionSeconds`, an assumption.
- **Reflector build.** The v6.0.x hashes are recorded only by prefix in [`reflector.md`](reflector.md), so only a
  feed running the attested v7.1.0 build (`25fc3060…`) is modelled; the v6 rule is implemented and tested but
  reachable only by explicit input.
- **Reflector base asset.** v7.1.0 quotes its own base asset as 1 at the last round; that branch is not modelled
  and returns `oracle_unsupported`. No deployed adapter's feed asset is its feed's base.
- **All-zero rounds.** Reflector advances `last_timestamp` on a round whose prices are all zero, but its event then
  carries no prices and `oracle_updates` holds no row for it. v2 is unaffected (no period skipped carries the
  asset); v1 would read the previous round where the contract reads None.
