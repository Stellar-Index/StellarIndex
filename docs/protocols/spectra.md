---
title: Spectra — contract & event scoping
last_verified: 2026-10-05
status: scoping
---

# Spectra — contract & event scoping

> **Status: not integrated.** No decoder, table or source exists yet
> (INV-2044). This page records what is confirmed, what is inferred and
> what is unknown, so the Spectra team can correct it before code lands.
> Nothing here is lake-verified. Do not read the sets below as a gate.

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

Not used: Horizon (invariant 2). stellar-rpc is for fixture capture only
(invariant 6).

## Confirmed contracts (18)

Addresses are shortened here. Full strkeys are taken from the API response
when `internal/sources/spectra/events.go` is written.

| # | Role | Address | Source |
|---|---|---|---|
| 1 | Registry | `CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V` | DL |
| 2 | PT sw-USDC 2026-11-01 | `CAAOR5F4…UCMK` | API |
| 3 | YT sw-USDC 2026-11-01 | `CBDQZFWY…TORX` | API |
| 4 | PT sw-EURC 2026-11-01 | `CA7KTCVD…J6AS` | API |
| 5 | YT sw-EURC 2026-11-01 | `CDNNGBZO…UPAR` | API |
| 6 | PT earnXLM 2026-11-03 | `CD5YZRFQ…7H7W` | API |
| 7 | YT earnXLM 2026-11-03 | `CAUKKZSH…RBYP` | API |
| 8 | PT earnUSDC 2026-11-03 | `CCJ43PID…LVPN` | API |
| 9 | YT earnUSDC 2026-11-03 | `CCZVSODQ…PBFL` | API |
| 10 | PT sw-deJTRSY 2026-10-16 | `CDRK5SWZ…VLJJ` | API |
| 11 | YT sw-deJTRSY 2026-10-16 | `CC3MKDR6…QR5G` | API |
| 12 | PT sw-deJTRSY 2026-12-15 | `CAAQJ6CN…ELMBI` | API |
| 13 | YT sw-deJTRSY 2026-12-15 | `CBRI2RSG…72V7` | API |
| 14 | PT sw-deJTRSY 2027-03-15 | `CDHIBKKS…ABMU` | API |
| 15 | YT sw-deJTRSY 2027-03-15 | `CAQPNK37…COT5L` | API |
| 16 | IBT sw-deJTRSY (`spectra-fungible-vault-wrapper`) | `CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO` | TL |
| 17 | IBT sw-USDC | `CBRT4E5A…MYBC` | API |
| 18 | IBT sw-EURC | `CCECATRP…WU3` | API |

Confirmed: the ids and their role as PT, YT or IBT. **Inferred:** rows 17
and 18 are `blend-fungible-vault-wrapper` instances (wrapping a Blend
position). The ids are confirmed, the wrapper type is not.

The earnXLM and earnUSDC IBTs are Upshift vaults and are already indexed
by the `upshift` source; they are not counted above.

## Unknown contracts (4)

Each exists in the public code. No mainnet id appears in any public
source found.

| Role | Why it matters |
|---|---|
| Factory | Emits `pt_deployed{deployer,pt,ibt,duration}`. Without its id, PT-to-IBT discovery stays seed-only. |
| LimitOrderEngine | The trading venue. Order and fill events come from here. |
| Router | Emits `execute_completed`. Log-only, out of scope. |
| Bridge / Messenger | Admin-level cross-chain plumbing. Out of scope. |

## Events (inferred)

Source: `spectra-core-stellar-public`, `#[contractevent]` structs. The
SDK default is topic[0] = snake_case struct name and a map body of the
non-topic fields. **Marked inferred until fixtures prove the map
encoding on mainnet builds.**

| Contract | Event: body fields |
|---|---|
| PT | `pt_minted{caller,receiver,shares:i128}` (deposit; IBT-in is not in the body) |
| PT | `redeem{owner,receiver,shares:i128}` (burn; assets-out is not in the body) |
| PT | `yield_updated{user,yield_in_ibt:i128}`, `yield_claimed{user,receiver,yield_in_ibt:i128}`, `fee_claimed{fee_collector,ibts:i128}` |
| PT | `rates_stored_at_expiry{ibt_rate:i128,pt_rate:i128}`, `yt_deployed{address}` |
| PT | `rewards_proxy_change{old,new}`, `rewards_claimed{caller}`, `pt_upgraded[topic caller]{new_wasm_hash}` |
| PT, YT | OZ fungible `transfer` / `mint` / `burn` (OZ 0.7 shape; check against fixtures and the SEP-41 decoder) |
| Registry | `pt_added{pt}`, `pt_removed{pt}`, `fee_reduced[topic pt,user]{reduction:i128}`, `registry_upgraded` |
| Registry | `tokenization_fee_change`, `yield_fee_change`, `pt_flash_loan_fee_change` `{old:i128,new:i128}` |
| Registry | `factory_change`, `router_change`, `fee_collector_change`, `bridge_change`, `limit_order_engine_change` `{old,new}`; `pt_wasm_hash_change`, `yt_wasm_hash_change` `{old,new}` |
| Factory | `factory_initialized{admin,registry}`, `pt_deployed{deployer,pt,ibt,duration:u64}`, `factory_upgraded` |
| LimitOrderEngine | `order_registered[topic maker,order_id]{making_amount:i128}`, `order_cancelled[topic maker,order_id]`, `order_filled[topic order_id]{actual_making:i128}` |
| LimitOrderEngine | `limit_order_fee_change{previous}[topic new]`, `registry_change`, `fee_collector_change` |
| Wrapper (`spectra-fungible-vault-wrapper`) | `wrap[topic caller,receiver]{vault_shares,shares}`, `unwrap[topic caller,receiver,owner]{shares,vault_shares}`, `rewards_proxy_updated`, `upgraded` |
| Wrapper (`blend-fungible-vault-wrapper`) | None. No `contractevent` in source. |
| Router | `execute_completed{caller,command_count:u32}` plus admin changes. Log-only. |

Gating note (ADR-0035): event names collide across contracts
(`fee_collector_change` is on both the registry and the LimitOrderEngine,
and the `upgraded` shape is shared). A decoder must check contract
identity first and topic second. Registry `pt_added{pt}` seeds a new PT,
and the PT's `yt_deployed{address}` seeds its YT.

## Trading: limit-order engine

An order's terms (`pt`, `order_type`, `implied_rate`, `expiry`,
`receiver`) are in the `register_order` call arguments only, not in any
event. `order_registered` carries `making_amount`, and `order_filled`
carries `actual_making`. So fills cannot be priced from events alone.
Options: decode the call (ContractCall-derived, dispatcher path like
`band`) or correlate same-tx PT/IBT transfers by
`(ledger, tx_hash, op_index)`. Not decided; slice 8.

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

1. This page.
2. WASM audit doc and `decoder-wasm-matrix.md` rows. PTs are upgradable,
   so every hash since the first PT (2026-08-03) needs auditing; the hash
   list needs the lake.
3. Fixture capture script and captured fixtures (RPC retention is about
   7 days, so August deploy events need a lake export).
4. `internal/sources/spectra` decoder, not wired.
5. Migration for `spectra_events` and `spectra_markets`, store writer.
6. Wiring (projected source: `buildSource`, `IsProjectedEvent`, replay
   via `projector-replay -source spectra`).
7. PT, YT and sw-* tokens into the SEP-41 watched set.
8. LimitOrderEngine order events and the order-terms decision.
9. API surface.

Money is `canonical.Amount` throughout. Market decimals differ (7, 13,
18); read per-market decimals, never a uniform scale. `BackfillSafe`
stays false until the WASM audit is done.

## Open questions

For the Spectra team:

- Mainnet ids, deploy ledgers and WASM hashes for the factory,
  LimitOrderEngine, router and bridge/messenger.
- Is the `#[contractevent]` body map-encoded on mainnet builds? Do any
  deployed PTs predate the public repo's event set?
- Will order terms ever be emitted in `order_registered`? That would
  keep fills on the projected path.
- Are rows 17 and 18 `blend-fungible-vault-wrapper` instances?

For Stellar Index:

- Is TVL in scope? PT `total_assets` is a view call, not derivable from
  events without an IBT rate path.
- Should PT and YT be assets in `/v1/assets`, or only protocol positions?
