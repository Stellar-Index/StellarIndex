---
title: Spectra — contract & event scoping
last_verified: 2026-10-06
status: scoping — lake-verified
---

# Spectra — contract & event scoping

> **Status: not integrated.** No decoder, table or source exists yet
> (INV-2044). This page records what is confirmed, what is inferred and
> what is unknown, so the Spectra team can correct it before code lands.
> Contract roles, hashes and event shapes below were checked against r1's
> ClickHouse lake (ledgers 63,778,000 to 64,800,000, tip about 64,798,584).
> Do not read the sets below as a gate.

## What Spectra is

Spectra (Perspective Finance) tokenises yield. An interest-bearing token
(IBT, e.g. a vault share) is split into a Principal Token (PT), redeemable
for the underlying at maturity, and a Yield Token (YT), which accrues the
IBT's yield until then. Each market is one PT/YT pair per IBT per
maturity date.

Trading is **not an AMM**. PT and YT trade through a `LimitOrderEngine`
contract; there is no pool contract and no reserves. The `pool` field in
the operator API equals the PT id.

## Sources

| Tag | Source |
|---|---|
| API | Spectra operator API, `app.spectra.finance/api/v1/stellar/pools` (7 markets) |
| DL | DefiLlama adapter `projects/spectra/stellar.js`, PR #21117, authored by perspectivefi, merged 2026-09-18 |
| TL | Token-list PR #421 |
| Code | `github.com/perspectivefi/spectra-core-stellar-public` @ `28d955f` (2026-08-10), soroban-sdk 25.3.0, OZ stellar-tokens 0.7.0 |
| Lake | r1 `stellar.contract_events`, `contract_instance_changes`, `ledger_entry_changes`; bodies decoded with `stellar xdr decode --type ScVal` |

Not used: Horizon (invariant 2). stellar-rpc is for fixture capture only
(invariant 6).

## Contracts (27 on chain)

Source genesis is ledger **63,778,088**: the registry's first `role_granted`.

| Role | Contract | WASM hash | First ledger / notes |
|---|---|---|---|
| Registry | `CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V` | `54c79aac…` | 63,778,088 |
| Factory | `CC4ZVRIYM33M5FVAUDFWK7JXO3PWIVSKKEBXVEEC5E6KPYISXIMLUJCP` | `763ea32c…` | `factory_initialized` 63,778,145; registry `factory_change` 63,778,155 |
| Router | `CB56R3NGNN7KNBGEH3CWK7SQIAR7SFAS3PKDQEJX7Y3U6TEFDJBVPY7F` | `1452052c…` | 63,778,172; admin events only, `execute_completed` never emitted |
| Order engine v0 | `CC2CEV23OQVGALHWQTKA26DYQTDNS7XJSL75EHTLHKZH6W3HJAEUKKB7` | `20ec1744…` | initialised 63,778,288, replaced at 63,780,164 (registry `limit_order_engine_change`), no orders |
| Order engine | `CCKNOCLH6QILGS6GYZWMQ6JCHWC2D75OCI5RLBPCUF7FJTONNSCZZAC5` | `20ec1744…` | 63,780,161; 49 orders registered, 25 filled, 8 cancelled |
| PT x7 | rows below | `3bcf316f…` | the registry's `pt_wasm_hash_change` value |
| YT x7 | rows below | `daeb931b…` | the registry's `yt_wasm_hash_change` value |
| IBT sw-USDC | `CBRT4E5AH23GMRQI7H6HQW54HMDMK4C23OO2CEN5OHWEOSRYBQZCMYBC` | `11b4bacc…` | 63,780,144; Blend wrapper |
| IBT sw-EURC | `CCECATRPUHLMFTIUQDQQPOU5GLXQGJYGBJHKSFMWJYOFEIFNN3MOQWU3` | `11b4bacc…` | 63,780,149; Blend wrapper |
| IBT sw-deJTRSY | `CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO` | `0e8d68e2…` | `dejtrsy_wrapper_initialized` 64,452,971 |

Eight WASM hashes cover all 27 contracts; no contract was ever upgraded in
place. Audit: [`wasm-audits/spectra.md`](../operations/wasm-audits/spectra.md).

The Blend wrappers (sw-USDC, sw-EURC) DO emit events: OpenZeppelin ERC-4626
style `deposit` and `withdraw`. The deJTRSY wrapper (`spectra-fungible-vault-wrapper`) emits `wrap`.

Markets (PT and YT per market; addresses shortened, full strkeys are in
`internal/sources/spectra/events.go`):

| # | Market | PT | YT |
|---|---|---|---|
| 1 | sw-USDC 2026-11-01 | `CAAOR5F4…UCMK` | `CBDQZFWY…TORX` |
| 2 | sw-EURC 2026-11-01 | `CA7KTCVD…J6AS` | `CDNNGBZO…UPAR` |
| 3 | earnXLM 2026-11-03 | `CD5YZRFQ…7H7W` | `CAUKKZSH…RBYP` |
| 4 | earnUSDC 2026-11-03 | `CCJ43PID…LVPN` | `CCZVSODQ…PBFL` |
| 5 | sw-deJTRSY 2026-10-16 | `CDRK5SWZ…VLJJ` | `CC3MKDR6…QR5G` |
| 6 | sw-deJTRSY 2026-12-15 | `CAAQJ6CN…ELMBI` | `CBRI2RSG…72V7` |
| 7 | sw-deJTRSY 2027-03-15 | `CDHIBKKS…ABMU` | `CAQPNK37…COT5L` |

The earnXLM and earnUSDC IBTs are Upshift vaults, indexed by the `upshift`
source. They must never be admitted to a Spectra gate; the IBT is a market
attribute only.

## Discovery chain

1. The factory emits `pt_deployed` (one topic; body Map `{deployer,
   duration:u64, ibt, pt}`). All 7 deploys came from the admin account
   `GCNC7GXV…`.
2. In the same ledger the registry emits `pt_added{pt}` and the new PT emits
   `yt_deployed{address}`. So one ledger carries the PT, the YT and the IBT.
3. Registry `factory_change`, `router_change`, `limit_order_engine_change` and
   `*_wasm_hash_change` name new infrastructure but must not admit anything
   automatically: a new id is a code change. The factory and order engines
   stay hand-kept.

Discovery is factory-anchored, like `sushiswap_v3`, plus a hand-kept list.
A genesis walk cannot find YTs, so they stay on the list as well.

## Events (shapes verified on lake bytes)

Topic[0] is the snake_case struct name; every `(contract, topic[0])` pair
observed matches the lake's `topics_xdr` byte for byte. Bodies are Maps.

| Contract | Event: topics, body |
|---|---|
| PT | `pt_minted` [sym] `{caller,receiver,shares:i128}`; `redeem` [sym] `{owner,receiver,shares:i128}`; `yield_updated` [sym] `{user,yield_in_ibt:i128}`; `yt_deployed` [sym] `{address}` |
| PT, YT | OZ 0.7 token events: `mint` and `burn` [sym, addr] body Map `{amount:i128}`; `transfer` [sym, from, to] body a bare `i128`; `approve` |
| Factory | `factory_initialized`, `pt_deployed` |
| Registry | `pt_added{pt}`, `factory_change`, `router_change`, `limit_order_engine_change`, `fee_collector_change`, `pt_wasm_hash_change`, `yt_wasm_hash_change` |
| Order engine | `order_registered` [sym, maker, order_id:bytes32] `{making_amount:i128}`; `order_filled` [sym, order_id] `{actual_making:i128}`; `order_cancelled` [sym, maker, order_id] `{}`; `limit_order_engine_initialized` `{fee_collector, fee_rate:i128, registry}` |
| deJTRSY wrapper | `wrap` [sym, caller, receiver] `{shares, vault_shares}`; `dejtrsy_wrapper_initialized` |
| Blend wrappers | `deposit`, `withdraw` [sym, caller, receiver, owner] `{assets, shares}` |
| Every contract | `role_granted`, `role_revoked`, `admin_transfer_initiated`, `admin_transfer_completed` (governance) |

`pt_minted.shares` equals both the PT and the YT `mint.amount` in the three
transactions sampled (63,812,811; 64,347,726; and the matching burns at the
`redeem` 63,812,816), so PT/YT `mint` and `burn` mirror `pt_minted` and
`redeem`.

**Not yet seen on chain** (shapes from the public source only): `unwrap`,
`yield_claimed`, `fee_claimed`, `rates_stored_at_expiry`, `pt_removed`,
`fee_reduced`, `pt_upgraded`, `execute_completed`, `rewards_proxy_change`,
`rewards_claimed`. The WASM carries their literals. The first maturity is PT
`CDRK5SWZ…` on 2026-10-16 (duration 2,592,000 s), so expiry and claim events
should start appearing from then.

Gating note (ADR-0035): event names collide across contracts
(`fee_collector_change` is on both the registry and the order engine;
`role_granted` is also emitted by unrelated contracts with a different body
shape). A decoder must check contract identity first and topic second.

## Trading: limit-order engine

An order's terms (`pt`, `order_type`, `implied_rate`, `expiry`,
`receiver`) are in the `register_order` call arguments only, not in any
event. `order_registered` carries `making_amount`, and `order_filled`
carries `actual_making`. So fills cannot be priced from events alone.
Options: decode the call (ContractCall-derived, dispatcher path like
`band`) or correlate same-tx PT/IBT transfers by
`(ledger, tx_hash, op_index)`; ledger 64,347,726 shows a fill that mints PT
in the same transaction. Not decided; slice 8.

## Overlap with other sources

- **Upshift.** earnUSDC and earnXLM are Spectra IBTs and Upshift vaults
  (`internal/sources/upshift/events.go`). Upshift keeps attributing the
  vault events. Spectra attributes only its own PT, YT, registry and
  wrapper contracts. No Spectra row may restate an Upshift vault event.
- **RWA (INV-2045).** sw-deJTRSY wraps deJTRSY, so the wrapper is a
  deJTRSY holder and deJTRSY's supply already counts what is wrapped.
  **Spectra figures are a protocol view and must never be added to RWA
  totals.** This work does not touch `internal/rwa` or deJTRSY's
  classification. One fact to share with INV-2045: the sw-deJTRSY wrapper
  id (`CAHPZLEH…`) is a deJTRSY holder it may want to label.

## Status and slice order

Not integrated. Planned PRs, smallest first:

1. This page, done.
2. WASM audit doc and `decoder-wasm-matrix.md` rows, done: 8 hashes, no
   upgrade seen.
3. Lake fixtures, done: `capture-spectra-fixtures.sh` has a lake mode
   (`LAKE_SSH`), one fixture per (hash, kind) observed on chain. Expiry kinds
   are re-captured after 2026-10-16.
4. `internal/sources/spectra` decoder, not wired.
5. Migration for `spectra_events` and `spectra_markets`, store writer.
6. Wiring (projected source: `buildSource`, `IsProjectedEvent`, replay
   via `projector-replay -source spectra`).
7. PT, YT and sw-* tokens into the SEP-41 watched set.
8. LimitOrderEngine order events and the order-terms decision.
9. API surface.

Money is `canonical.Amount` throughout. Market decimals differ (7, 13,
18); read per-market decimals, never a uniform scale. `BackfillSafe`
stays false until a separate change decides, after the WASM audit.

## Open questions

- Can anyone call the factory's `deploy_pt`? All 7 deploys came from the
  admin. If it is open, serve a market only once the registry has emitted its
  `pt_added`.
- Fills carry no price (see Trading). Decide the correlation or call-decode
  route before slice 8.
- The 7 / 13 / 18 market decimals in `events.go` appear in no event; verify
  them from instance storage or the operator API before any display scaling.
- Maturity is inferred as deploy close_time + `duration`; check it against the
  operator API dates.
- Is TVL in scope? PT `total_assets` is a view call, not derivable from
  events without an IBT rate path.
- Should PT and YT be assets in `/v1/assets`, or only protocol positions?
