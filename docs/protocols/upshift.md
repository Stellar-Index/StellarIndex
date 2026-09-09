---
title: Upshift vaults — contract & event verification
last_verified: 2026-09-09
status: current
---

# Upshift vaults — contract & event verification

> **For the Upshift / Gami Labs team:** this documents how Stellar Index
> identifies the Upshift vaults on Stellar and attributes their events.
> Every contract below was established from on-chain evidence in the
> certified ledger lake, not from an announcement or a third-party
> listing.
>
> - **Enumeration method:** bespoke-symbol census (ADR-0040 mechanism 3,
>   curated set) — the vaults have no factory, so they are enumerated by
>   the five event symbols only this protocol emits, then each one's
>   underlying is proven from the transaction that funded its first
>   deposit.
> - **Last verified:** 2026-09-09 (source: `internal/sources/upshift`;
>   lake sweep over ledgers 62,000,000 → 64,345,4xx).
> - **Gate status:** ✅ **GATED (curated 2-vault allowlist).**

## What the Upshift vaults are

An Upshift vault is an **ERC-4626-shaped tokenized vault**: it accepts
one underlying asset and mints proportional **shares**. The vault
contract *is* the share token — it emits the SEP-41 `transfer` and
`approve` events for its own shares — so a single contract id names both
"the vault" and "the share instrument".

Two vaults run on pubnet, curated by Gami Labs and Stake Capital Group:

| Vault | Contract | Underlying | First lake event |
|---|---|---|---|
| earnUSDC | `CCL3WITWFFXIHV2I52ECV5DPIEOFSTU3PBPR53ILPLF2IP5KHECXRUTY` | native USDC (`USDC:GA5ZSEJY…KZVN`, SAC `CCW67TSZ…`) | `admin_set` @ 62,623,313 |
| earnXLM | `CC6TRAPQD3NK7THUKWPV5SL2JHKQGNXZVB6S6MVYFSLRWAKEFUWZKZ7J` | native XLM (SAC `CAS3J7GY…`) | `admin_set` @ 62,623,319 |

### How those two were established

1. **The set is closed by a bespoke-symbol census.** Five symbols in this
   protocol's vocabulary appear on no other contract in the lake at or
   after ledger 62,000,000: `deployed_assets_changed`,
   `wallet_deployed_updated`, `deposit_to_subaccount`,
   `withdraw_from_subaccount`, `wallet_net_deployed_seeded`. Exactly two
   contracts emit them. This is the enumeration, and re-running it is how
   a third vault would be admitted.
2. **The two are one deployment.** Their `admin_set` events are 6 ledgers
   apart, their `operator_set` 11, their `subaccount_added` 8 — and both
   name the same admin (`GDYNPLQ7…`) and the same operator (`GCDR6QB2…`).
   Each has its own custody subaccount (`GDJ7WZYS…` for earnUSDC,
   `GAPUSI7E…` for earnXLM), which is what a per-vault custody wallet
   looks like.
3. **Each underlying is proven, not assumed.** One event index ahead of
   each vault's first `deposit`, in the SAME transaction, sits a SAC
   `transfer` for exactly the amount the deposit reports as `assets`:

   ```
   ledger 62,938,336  CCW67TSZ… transfer … String("USDC:GA5ZSEJY…KZVN")  2000000
                      CCL3WITW… deposit  { assets: 2000000, shares: 2000000000000 }

   ledger 62,638,654  CAS3J7GY… transfer … String("native")              5000000
                      CC6TRAPQ… deposit  { assets: 5000000, shares: 5000000000000 }
   ```

**What is inferred rather than proven:** the ticker names. No event
carries the share token's symbol, so "earnUSDC" and "earnXLM" are read
off the underlying each vault holds plus the protocol's published pair.
The *contract ids* are what the decoder gates on, and those are proven.

**A third address does not exist.**
`CC2DNHE5EFPPVJ47BJCRIQCEZ7KVHOATVR5QMNBEKKZQF2EZO7U7YTHT` circulated as
this vault's address and carries **zero** events of any kind. It is not
indexed, and a test pins its absence.

## Event vocabulary

Twelve symbols across the two vaults; all twelve are recognised and
gated, four produce rows.

### Decoded

| Symbol | Topics | Body | Row |
|---|---|---|---|
| `deposit` | `[Sym, caller, receiver, owner]` | `Map{assets: i128, shares: i128}` | ✅ |
| `withdraw` | `[Sym, caller, receiver, owner]` | `Map{assets: i128, shares: i128}` | ✅ |
| `transfer` | `[Sym, from, to]` | `i128` or `Map{amount, to_muxed_id}` | ✅ |
| `deployed_assets_changed` | `[Sym, operator]` | `Map{old_amount: i128, new_amount: i128}` | ✅ |

### Recognised, gated, deliberately unserved

`deposit_to_subaccount`, `withdraw_from_subaccount`,
`wallet_deployed_updated`, `wallet_net_deployed_seeded` are the
**custody-side mirror** of the capital movement `deployed_assets_changed`
already reports at vault level; writing them alongside would double-count
deployed capital. `subaccount_added`, `admin_set` and `operator_set` are
governance. `approve` is a SEP-41 allowance, not a balance change.

All eight decode to **zero rows with no error**, so the ADR-0033
re-derive counts their ledgers as expected-zero rather than treating them
as blind spots.

## The address triple

`deposit` and `withdraw` carry three topic addresses. The reading is
**(caller, receiver, owner)** — the OpenZeppelin ERC-4626 `Withdraw`
ordering — and exactly two events in the two vaults' combined history
carry three *different* addresses, one per vault. They corroborate each
other:

```
63,812,795  earnXLM   (C CD5YZRFQ…, G GCNC7GXV…, C CD5YZRFQ…)
63,812,816  earnUSDC  (C CCJ43PID…, G GCNC7GXV…, C CCJ43PID…)
```

The same `G` account sits in the middle slot of both while the outer
contract differs — one holder redeeming both vaults in one session, 21
ledgers apart, through a different router each time. Six ledgers before
the earnUSDC one, that `G` account had approved that `C` contract as a
spender (`approve` @ 63,812,810) and transferred its shares to it
(`transfer` @ 63,812,811). So at burn time the contract both *called*
and *owned* the shares, while the `G` account received the underlying —
and the constant across the pair is the holder, not the caller.

**Caveat, stated because it matters:** all 184 `deposit` events carry the
three addresses identical, so deposit's reading is carried over from
withdraw (same arity, same body schema) and is not independently proven.

## Amounts: two different scales

`assets` and `shares` are **not** on the same scale. Every genesis-era
deposit mints `shares == assets × 1,000,000` exactly — the OpenZeppelin
ERC-4626 **decimals offset of 6** that hardens a vault against the
inflation attack — and the ratio drifts as the share price accrues. The
earnUSDC withdraw at ledger 63,812,816 redeems 9,919,211,002,150 shares
for 10,000,000 assets, ~0.8% above par.

Stellar Index therefore stores both as **raw i128** and neither divides
one by the other nor applies a decimals assumption anywhere in the
pipeline. The offset is recorded as evidence, not applied as a constant.

## What is NOT derived, and why

- **TVL is not served for these vaults.** Total assets are not derivable
  from the event stream at all. Deposits and withdrawals give net
  *principal* flow, but yield accrues to the vault without emitting any
  event, and the only on-event total —
  `deployed_assets_changed.new_amount` — is the **deployed** leg alone;
  the idle balance is unobservable on-chain. Publishing the deployed leg
  as TVL would be zero-filling the missing leg, which this project
  forbids: an unpriceable leg is reported as unpriceable. A real TVL
  needs a contract-state read of the vault's own `total_assets()`
  (ADR-0039), which is a separate change.
- **Share supply IS exactly derivable** — `SUM(shares)` over deposits
  minus `SUM(shares)` over withdrawals — because neither vault has ever
  emitted a mint or burn outside those two events, and a share transfer
  reassigns the claim without changing the count.
  `Store.UpshiftVaultShareSupplies` computes it.
- **No price, and no VWAP contribution.** The vaults publish nothing. A
  price for the earnUSDC *share* is published by RedStone
  (`earnUSDC_FUNDAMENTAL`) and reaches the index through the oracle path;
  that feed's base asset is this same vault contract id, held as a single
  constant (`upshift.MainnetVaultEarnUSDC`) that
  `internal/sources/redstone` imports, so the price and the activity
  underneath it cannot drift onto different asset ids.

## Gate

Contract-identity gated per ADR-0035, using the ADR-0040 **curated-set**
mechanism: there is no factory to fan out from — neither vault has a
creation event anywhere in the lake, each one's first event being its own
`admin_set`.

The gate is not a formality. `deposit`, `withdraw` and `transfer` are
generic Soroban symbols with no protocol namespace: a bounded
20,000-ledger census (63,000,000–63,020,000) found **four distinct
contracts** emitting `deposit` across 77 events, the vaults a minority of
them, and `transfer` is the single most common event on the network. A
topic-only decoder would record strangers' vault flows under this source
and let any contract publish share and asset figures in this protocol's
name.

The trust root is the in-code curated set plus the `protocol_contracts`
warm, which is the operator seam for admitting a third vault without a
redeploy. It fails **closed**: an un-admitted vault's events surface as an
ADR-0033 recognition gap, never as a silent mis-attribution.

## Related

- `internal/sources/upshift` — decoder, curated set, golden tests over
  real lake bytes.
- `migrations/0157_create_upshift_vault_events.up.sql` — the served table.
- [ADR-0035](../adr/0035-factory-anchored-contract-gating.md),
  [ADR-0040](../adr/0040-completing-contract-gating.md) — the gating model.
- [ADR-0003](../adr/0003-i128-no-truncation.md) — why amounts are NUMERIC.
