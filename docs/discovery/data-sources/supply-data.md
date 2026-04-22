# Supply data — circulating / total / max

**Status:** ✅ **Design locked.** Blocks Freighter V2's market-cap /
FDV / circulating-supply fields (see
[rfp-requirements-matrix.md §C1](../rfp-requirements-matrix.md)).

**Scope:** how we derive, for every indexed asset on Stellar
(classic + SEP-41 Soroban):

- `total_supply`        — every unit minted, ever, minus every unit burnt.
- `circulating_supply`  — `total_supply` minus a configurable "locked" set.
- `max_supply`          — cap if one exists, else `null`.

All three are `NUMERIC` in our DB, `*big.Int` in Go, strings on the
wire ([decisions.md](../decisions.md) i128 invariant applies —
Soroban mint/burn amounts are `i128`).

## Stellar has **three** asset domains we must handle

| Domain | Example | Supply source | Data shape |
| ------ | ------- | ------------- | ---------- |
| **Native XLM** | `native` / `CAS3J7…OWMA` as SAC | Inflation pool + lumen genesis supply | Fixed: 50 B lumens, never minted/burned post-2019 |
| **Classic credit assets** | `USDC:GA5Z…`, `yXLM:GDS…` | Issuer account balance + trustlines + claimable balances + LP shares | Ledger-state aware; issuer-balance-centric |
| **SEP-41 Soroban tokens** | custom tokens + SAC-wrapped classics | `mint` / `burn` / `clawback` events per [sep-41 notes](../notes/sep-41-token-events.md) | Event-sum |

These are three different algorithms.

## Algorithm 1 — Native XLM

Fixed fact:

- Total supply: **50,001,806,812 XLM** (50 B genesis + ~1.8 B
  inflation pool) after inflation was disabled by network vote in
  **October 2019**.
- `max_supply = total_supply` (no more will ever be minted).
- `circulating_supply` = `total_supply − (SDF cold-reserve accounts)`.

Implementation:

1. Hard-code `total_supply = 50_001_806_812 * 10_000_000` stroops
   at init.
2. `max_supply = total_supply`.
3. `circulating_supply` = `total_supply − sum(balances of
   SDF-held reserve accounts)`. SDF publishes the reserve account
   list. We maintain this as a config file, not derive from the
   chain, because there's no on-chain flag saying "this account is
   locked."

**No event-stream tracking needed** for XLM — the numbers don't move.

## Algorithm 2 — Classic credit assets (`CODE:ISSUER`)

This is the non-trivial case.

### Total supply (classic)

Stellar makes this easier than most chains: **the issuer is
authoritative**. Total supply is defined as every unit the issuer
has ever sent out that hasn't come back.

Two ways to compute, both correct:

**Method A — issuer balance inverse:**

```
total_supply = SUM(all trustline balances for this asset)
             + SUM(all claimable balances holding this asset)
             + SUM(all LP reserves involving this asset, pro-rata
                   of pool-share ownership)
             + SUM(all Soroban contract balances if the asset has
                   been SAC-wrapped)
```

**Method B — `balance_authorized + balance_authorized_to_maintain_liabilities`:**

Horizon exposed this directly as `balance_authorized` on the
`assets/` endpoint. We don't use Horizon, but the underlying data
is in `TrustLineEntry` ledger entries — we can materialise the
same view from Galexie ledger meta.

**Chosen approach: Method A**, reconstructed from ledger meta
(trustlines + claimable balances + LP + SAC contract balances).
Updated on every ledger that touches any of these entry types for
the asset. We store a `classic_asset_supply` hypertable row per
(asset, ledger).

### Circulating supply (classic)

`total_supply` − `locked_set`, where `locked_set` is operator-
configurable per asset. Default rules:

1. **Exclude the issuer account's own balance** — tokens held by
   the issuer haven't been distributed.
2. **Exclude known reserve / treasury multisigs** — configured per
   asset in a YAML file. Source of truth is the asset's issuer via
   stellar.toml SEP-1 `[[CURRENCIES]]` entries or an operator
   override.
3. **Exclude vesting contracts** where known (e.g. tokens still in
   a time-lock contract).
4. **Do NOT exclude liquidity-pool reserves** — LP tokens are
   fungible and circulating-held; the underlying asset is still
   "out there."

If the operator hasn't configured overrides, we default to
**issuer-balance-exclusion only**. Document this clearly in the
API response as `circulating_supply_basis: "issuer_balance_only"`
so consumers know the policy.

### Max supply (classic)

No on-chain max exists for classic assets — the issuer can always
issue more. `max_supply = null` unless:

- The issuer account's `auth_immutable` and `auth_revocable` flags
  plus known burn-signer patterns indicate issuance is locked. This
  is rare and hard to detect automatically; treat as operator
  override.
- SEP-1 `[[CURRENCIES]].max_supply` field is set on the issuer's
  stellar.toml — respect it as a display value but flag it
  `self_declared: true` because it's not on-chain-enforced.

## Algorithm 3 — SEP-41 Soroban tokens

Per [notes/sep-41-token-events.md](../notes/sep-41-token-events.md),
SEP-41 locks down the supply semantics:

```
total_supply = Σ mint.amount − Σ burn.amount − Σ clawback.amount
```

Implementation:

1. Index every `mint` / `burn` / `clawback` event emitted by the
   token contract since its deployment.
2. Maintain a running running per-token total in a hypertable.
3. `max_supply` has **no canonical on-chain source for SEP-41**. We
   source from:
   - The token's `[[CURRENCIES]].max_supply` in stellar.toml if the
     token issuer publishes one.
   - Operator override.
   - Otherwise `null`.

### Circulating supply (SEP-41)

Same pattern as classic: `total − locked_set`, with operator-
configurable exclusions. Defaults:

1. **Token admin account / contract balance** — if the token has
   an admin, exclude the admin's held balance.
2. **Vesting contracts** — by config.
3. **Known burn-address analogues** (e.g. an address that's been
   signalled as "dead").

SAC-wrapped classic assets follow **both** algorithms — they're
classic assets *and* have SEP-41 events (mint/burn). The algorithms
must agree. Cross-check: we compute both and alert if they
disagree by > 1 stroop.

## Schema

```sql
CREATE TABLE asset_supply_history (
  time                 TIMESTAMPTZ NOT NULL,
  asset_key            TEXT NOT NULL,     -- "XLM" | "CODE:G…" | "C…"
  total_supply         NUMERIC NOT NULL,  -- base units
  circulating_supply   NUMERIC NOT NULL,
  max_supply           NUMERIC,            -- NULL when uncapped
  basis                TEXT NOT NULL,      -- "issuer_exclusion" | "admin_exclusion" | "override" | ...
  ledger_sequence      BIGINT NOT NULL
);
SELECT create_hypertable('asset_supply_history', 'time');
CREATE UNIQUE INDEX ON asset_supply_history (asset_key, ledger_sequence);
```

Updates are append-only; latest row per `asset_key` is the
queryable current state. Historical queries via `time`-bucket.

## API response shape

For the Freighter V2 asset detail endpoint, each supply field is a
string (i128 safety):

```json
{
  "total_supply":      "50001806812000000",
  "circulating_supply": "45000000000000000",
  "max_supply":        "50001806812000000",
  "supply_basis":      "xlm_sdf_reserve_exclusion",
  "market_cap_usd":    "…",
  "fdv_usd":           "…"
}
```

When any field is `null` or the policy is "unknown-max" we return
`null` and document it.

## Operator config file

`config/asset_supply_policy.yaml`:

```yaml
assets:
  XLM:
    locked_accounts:
      - GAXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX   # SDF reserve 1
      - GAYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYY   # SDF reserve 2
    max_supply: "50001806812000000"

  "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN":
    exclude_issuer_balance: true
    max_supply: null  # uncapped — Circle mints on demand

  "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA":
    # XLM SAC — defer to XLM rules above
    alias_of: "XLM"
```

File is version-controlled in the deployment repo. Operators of
self-hosted CTX Rates instances can override locally.

## Data sources we ingest

Already covered by our existing audit:

- **stellar-extract** for classic asset supply-relevant events
  (`ExtractTrustlines`, `ExtractClaimableBalances`, `ExtractLiquidityPools`,
  `ExtractNativeBalances`) — see
  [data-sources/withobsrvr-stellar-extract.md](withobsrvr-stellar-extract.md).
- **Soroban `mint` / `burn` / `clawback` events** per
  [notes/sep-41-token-events.md](../notes/sep-41-token-events.md).
- **Post-P23 unified events** emit `mint` / `burn` for classic
  issuer-side flows ([notes/cap-67-unified-events.md](../notes/cap-67-unified-events.md))
  — cross-check against ledger-entry deltas.

No new ingestion surface. Pure materialisation logic on top of what
we already capture.

## Open items

- [ ] Ingest SDF reserve account list (publish source TBC — their
      stellar.toml, foundation public docs, or manual curation).
- [ ] Decide default policy for tokens where no stellar.toml
      exists and no operator override: return `max_supply: null`,
      `basis: "no_metadata"`. Confirm acceptable per Freighter RFP.
- [ ] Cross-check algorithm: for every SAC-wrapped classic asset,
      verify that `Algorithm 2` and `Algorithm 3` agree. Alert on
      divergence.
- [ ] Historical backfill: when we backfill `asset_supply_history`
      from genesis, the pre-P20 world has no Soroban tokens and
      no SAC wrappers — only classic. After P20, both paths exist.
      Fixture this.

## Related

- [../rfp-requirements-matrix.md §C1](../rfp-requirements-matrix.md) — this doc closes the gap.
- [../notes/sep-41-token-events.md](../notes/sep-41-token-events.md) — SEP-41 supply math.
- [../notes/cap-67-unified-events.md](../notes/cap-67-unified-events.md) — classic issuer mint/burn post-P23.
- [sep1-home-domain.md](sep1-home-domain.md) — companion doc for
  `max_supply` metadata via stellar.toml.
- [../decisions.md](../decisions.md) — i128 invariant.
