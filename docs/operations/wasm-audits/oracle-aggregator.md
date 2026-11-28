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
function or imports `update_current_contract_wasm`, so a contract's program is fixed for its life. The read path
takes the hash from the same instance entry it decodes the config from, so the gate and the config are one read.

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
| feed `price(asset, T)`: `T` normalised down to the feed's live resolution; no price after the last round or more than 255 periods before it; priced iff the history mask bit is set | Reflector v7.1.0 `price_oracle.rs:211-220`, `prices.rs:32-58`, `mapping.rs::check_history_updated` |

### Feed state

The feed's own instance entry (Reflector v7.1.0, `oracleagg.DecodeFeedState`; an unknown String key fails the
decode) carries everything the recipe needs from it: `last_timestamp` (ms), `resolution` (ms), `decimals`,
`protocol` (only 2 is modelled), `base_asset`, `assets`, `expiration`, `history` (32 bytes per asset, bit `p` = priced `p` periods before `last_timestamp`) and `cache` (the
recent rounds' update masks and prices). Our stored rounds are cross-checked against it, and any disagreement is
`oracle_feed_incomplete`, never a price:

- our newest stored round at or after `last_timestamp` must be `last_timestamp`, on the resolution boundary;
  fewer is allowed only when the history mask says that round priced nothing (an all-zero round writes no row);
- for every round the recipe touches, a stored row exists iff the asset's history bit is set;
- where the round is in `cache`, the stored integer equals the cached one.

An asset's index is `assets.rs::resolve_asset_index`, `load_all_assets(e).first_index_of(asset)`: its first
position in `assets`. The instance also holds `Symbol`/`Address` → `u32` entries that v7.1.0 neither reads nor
writes in the source copy; they are decoded as `LegacyIndex` and never used to resolve. The real DEX feed lists 52 assets with 49
such entries (positions 49-51, added after the upgrade, have none), and every entry present equals its list
position. Any other String key is an error. The allowed set is what `settings.rs`, `assets.rs` and `prices.rs`
define (`cache_size` included; absent on both feeds, default 2), plus `admin`, `protocol` and `protocol_update`,
whose modules are not in the source copy: they are accepted by name from the real entries, and only `protocol` is
read.

Both real feeds (`test/fixtures/lending_oracle/reflector-instances.tsv`) decode, all 52 DEX assets resolve, and
their history mask agrees with the cached last round's update mask asset by asset. Source files:
`oracle_src_assets.rs`, `oracle_src_settings.rs` (sha256 `dc482728…`, fetched from
`reflector-network/reflector-contract` tag `v7.1.0`, `oracle/src/settings.rs`).

Verdicts the contract itself would give: `oracle_asset_missing`, `oracle_asset_blocked`, `oracle_contract_panics`
(the call traps on underflow), `oracle_feed_asset_unknown` (the feed does not list the asset), `oracle_price_stale`,
`oracle_price_absent` (v1: the asset sat out the last round), `oracle_deviation`, `oracle_deviation_unverifiable`
(v2: no earlier period). Where we cannot reproduce it: `oracle_unsupported`, `oracle_read_failed` (our read
failed), `oracle_feed_incomplete`, `oracle_state_newer` (an entry changed after the evaluation ledger).

## Read path

`PoolOraclePrices` (`internal/api/v1/lending_oracle_price.go`), per pool, at one evaluation ledger `L` = the lake
watermark, whose close time is `t`. Nothing is held back to a projector cursor: a round the projector has not
written yet is caught by the feed-state cross-check above and reported, not replaced by an older one.

1. The oracle's instance entry gives the WASM hash (one of the two builds, else `oracle_unsupported`) and the
   config; v1 reads its persistent entries by exact key. Each feed's instance entry gives its build and state.
   Every entry's last-modified ledger must be `<= L`, else `oracle_state_newer`; a v1 entry removed after `L`
   counts too. The adapter and its feeds' state are cached per (server, oracle, `L`), with concurrent misses
   sharing one read; an adapter read failure and a feed read failure on our side are not cached.
2. Config failures fail the whole pool only when the instance itself cannot be read or decoded; one malformed
   or newer `AssetConfig` / `Blocked` entry fails that reserve alone.
3. Feed rows come from `Store.OracleFeedRounds`, keyed on the feed CONTRACT from the adapter config, every stored
   identity of the feed asset (`canonical.AssetAliasStrings`) and `ledger <= L`, over exactly the rounds the
   program can touch: v1 `[last_timestamp, t]`; v2 `t − (MaxAge + res + MaxAge/res·res + feed resolution)`. A raw
   row marks a round but never prices, and a feed symbol that maps only to `raw:` is `oracle_unsupported`.
4. The quote is the adapter's base, served verbatim: `fiat:USD`, or the USDC SAC for `CDN7JD6S`. The pool's own
   24 h age bound is not applied here; blend's health math applies `pool.rs::load_price` to the price timestamp.

Verdicts per pool and reason: `stellarindex_lending_oracle_verdicts_total{pool,reason}`.

## Not verified, and modelled as explicit inputs

- **Asset expiry.** `assets.rs` keeps an `expiration` vector per asset; the price read path (`price`,
  `lastprice`) does not consult it, so it is decoded for the key check only.
- **Reflector build.** The v6.0.x hashes are recorded only by prefix in [`reflector.md`](reflector.md), so only a
  feed running the attested v7.1.0 build (`25fc3060…`) is modelled; the v6 rule is implemented and tested but
  reachable only by explicit input.
- **Reflector base asset.** v7.1.0 quotes its own base asset as 1 at the last round; that branch is not modelled
  and returns `oracle_unsupported`. No deployed adapter's feed asset is its feed's base.
- **i128 overflow.** The adapters compute in `i128` and would trap on overflow (`max_dev` product, rescaling);
  the recipe computes in `big.Int` and does not model the trap, a non-goal: the largest cached feed price is
  ~6·10^18 and decimal shifts are at most 10^14, so the worst product (~6·10^32) is five orders of magnitude below
  2^127. An input that did overflow would trap on chain and still price here.
