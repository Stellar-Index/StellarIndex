---
title: Classic supply observers — verification
last_verified: 2026-07-06
status: current
---

# Classic supply observers — verification

> **What this page is:** the classic (non-Soroban) supply observers are
> internal Stellar Index components, not third-party protocols — there is
> no team to confirm a contract set with. This page documents which
> `LedgerEntry` mutations each observer watches, the operator watched-set
> gating, and the known-limitation caveats that bound a circulating-supply
> figure.
>
> - **Enumeration method:** operator watched-set (`[supply]
>   sdf_reserve_accounts` / `watched_classic_assets` / `sac_wrappers`). No
>   "watch every account" mode at v1 — the 50M+ network-account table-size
>   implications need their own ADR.
> - **Last verified:** 2026-07-06 (sources under `internal/sources/*`
>   `doc.go`; ADR-0021 / ADR-0022).
> - **Gate status:** ✅ Gated (watched-set): each observer's `Matches`
>   fast-path is a type discriminator + a watched-set map lookup before
>   any decode work.

## The three-domain supply split

Supply derivation is split by asset domain (see
`docs/architecture/supply-pipeline.md`):

| Algorithm | Domain | Observers |
|---|---|---|
| Algorithm 1 | native XLM | `accounts` (AccountEntry) |
| Algorithm 2 | classic credit assets | `trustlines`, `claimable_balances`, `liquidity_pools`, `sac_balances` |
| Algorithm 3 | SEP-41 Soroban tokens | `sep41_supply` → [sep41-supply.md](sep41-supply.md) |

This page covers Algorithms 1 + 2. Each observer plugs into a specific
dispatcher hook and writes to a per-class hypertable that the supply
readers aggregate at refresh time.

## The observers

| Observer | ADR | Dispatcher hook | Watches | Hypertable |
|---|---|---|---|---|
| `accounts` | 0021 | `LedgerEntryChangeDecoder` | `AccountEntry` deltas on watched G-strkeys (reserve list, issuers, validators) | `account_observations` |
| `trustlines` | 0022 | `LedgerEntryChangeDecoder` | `TrustlineEntry` changes for watched classic credit assets (CODE:ISSUER) | `trustline_observations` |
| `claimable_balances` | 0022 | `LedgerEntryChangeDecoder` | `ClaimableBalanceEntry` changes for watched assets | `claimable_observations` |
| `liquidity_pools` | 0022 | `LedgerEntryChangeDecoder` | `LiquidityPoolEntry` reserve changes (up to 2 observations/change — one per watched side) | LP-reserve table |
| `sac_balances` | 0022 | `LedgerEntryChangeDecoder` | `ContractData` deltas matching the SEP-41 balance key `Vec(Symbol("Balance"), Address(holder))` on watched SAC wrappers | `sac_balance_observations` |

The `accounts` observer is dual-purpose: `supply.LCMReserveBalanceReader`
reads reserve balances for circulating-supply, and
`metadata.LCMHomeDomainResolver` reads issuer home-domains for the
metadata overlay (both replace the old operator-static config maps).

## Known limitations (material to a supply figure)

These are honest caveats, not bugs — read before citing a
circulating-supply number as exact:

- **Removed-variant handling (claimable balances + LPs) — resolved.**
  The XDR `LedgerKey` for a claimable balance carries only the
  `BalanceId`, and for an LP only the `PoolId` — neither carries the
  asset, so a *Removed* change cannot be asset-key-filtered on its own.
  stellar-core emits the full pre-image as a `LEDGER_ENTRY_STATE` change
  immediately before the `LEDGER_ENTRY_REMOVED` for the same entry, so
  each observer memoizes the State pre-image for the duration of the
  ledger walk and emits one removal Observation (balance zero,
  `IsRemoval`) per watched side. A removal whose pre-image is not in the
  same ledger is **silently dropped** — `dispatchEntryChange` does not
  call `bumpUnmatched()`, and neither observer package emits a metric or
  a log line for it (`internal/dispatcher/dispatcher.go`). The effect is
  a CONSERVATIVE error on the money path: the removal is not applied, so
  circulating supply — and therefore market cap — is UNDER-reported
  rather than over-reported. That direction is deliberate; the silence
  is not, and it is why a drop cannot currently be detected.
- **SAC balance value shape varies by contract.** Native (host)
  SACs store `Map({amount, authorized, clawback})`; some custom token
  contracts store a bare `i128`. `scval.SEP41BalanceAmount` tries both;
  any other shape is dropped and counted as a dispatcher decode error.
- **Watched-set, not network-wide.** Every observer only sees the
  operator-configured watched set. A circulating-supply figure is
  complete only to the extent the watched set is complete for that asset.
- **LP variant scope.** Only `ConstantProduct` LPs exist on Stellar
  today; a future variant would need the ConstantProduct branch in the
  LP observer's `Matches` / `Decode` extended (it is an inline check in
  `liquidity_pools/dispatcher_adapter.go`, not a shared helper).

## Amount precision

All balances flow as `i128` / native stroops → `*big.Int` → `NUMERIC`
(ADR-0003); the readers sum with exact arithmetic, never int64. The
integration tests exercise `> int64`-max values.

## References

- ADR-0021 (AccountEntry observer); ADR-0022 (classic supply observers)
- `docs/architecture/supply-pipeline.md` (the three-algorithm split +
  which hook each observer uses)
- SEP-41 Soroban supply (Algorithm 3): [sep41-supply.md](sep41-supply.md)
