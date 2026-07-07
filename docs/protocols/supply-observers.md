# Classic supply observers — verification

> **What this page is:** the classic (non-Soroban) supply observers are
> internal Stellar Index components, not third-party protocols — there is
> no team to confirm a contract set with. This page documents which
> `LedgerEntry` mutations each observer watches, the operator watched-set
> gating, and the known-limitation caveats that bound a circulating-supply
> figure.
>
> - **Enumeration method:** operator watched-set (`[supply]
>   watched_accounts` / `watched_classic_assets` / `sac_wrappers`). No
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

- **Removed-variant overcount (claimable balances + LPs).** The XDR
  `LedgerKey` for a claimable balance carries only the `BalanceId`, and
  for an LP only the `PoolId` — neither carries the asset. So a *Removed*
  change can't be asset-key-filtered at the observer without a
  prior-row lookup. Current handling: `Matches` returns **false** for all
  Removed changes, so the Sum overcounts by the claimed-but-not-recorded
  amount. For **circulating supply this is a conservative error** — we
  under-report circulating (treating claimed-and-gone as "still in
  claimable / still in pool") rather than over-report. A writer-side
  prior-asset lookup is the planned fix, gated on the overcount being
  measurable in production.
- **SAC balance value shape varies by contract.** Native (host)
  SACs store `Map({amount, authorized, clawback})`; some custom token
  contracts store a bare `i128`. `extractBalanceAmount` tries both; any
  other shape is dropped and counted as a dispatcher decode error.
- **Watched-set, not network-wide.** Every observer only sees the
  operator-configured watched set. A circulating-supply figure is
  complete only to the extent the watched set is complete for that asset.
- **LP variant scope.** Only `ConstantProduct` LPs exist on Stellar
  today; a future variant would need a `type` switch extension in
  `extractFromChange`.

## Amount precision

All balances flow as `i128` / native stroops → `*big.Int` → `NUMERIC`
(ADR-0003); the readers sum with exact arithmetic, never int64. The
integration tests exercise `> int64`-max values.

## References

- ADR-0021 (AccountEntry observer); ADR-0022 (classic supply observers)
- `docs/architecture/supply-pipeline.md` (the three-algorithm split +
  which hook each observer uses)
- SEP-41 Soroban supply (Algorithm 3): [sep41-supply.md](sep41-supply.md)
