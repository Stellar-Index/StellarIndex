---
title: SAC wrappers + Soroban USD-volume backfill
last_verified: 2026-07-09
status: draft
---

# SAC wrappers + Soroban USD-volume backfill

How to add a Stellar-Asset-Contract mapping and retroactively
price the historical trades that pre-date the addition.

## Why this matters

Soroban DEX sources (Soroswap, Phoenix, Aquarius, Comet) emit
`base_asset` and `quote_asset` as the wrapped-asset SAC contract
ID — not the underlying classic asset. Without an operator-config
mapping from C-strkey to "CODE-ISSUER", three things go wrong:

1. **Explorer shows raw C-strkeys** instead of readable tickers
   (the `/dexes` and `/markets` AssetLabel can't resolve them).
2. **`/v1/sac-wrappers` returns an empty map** — the explorer's
   client-side resolution falls through to the truncated form.
3. **`trades.usd_volume` stays NULL** — the indexer's on-chain
   USD-volume path can't follow `quote_asset = C…` back to a
   USD-pegged classic, so it skips the column.

The fix is two-part: a one-line config addition for the contract
mapping (lights up #1 + #2 + #3 for new trades), plus a
backfill SQL that retroactively prices the historical rows.

## Adding a single SAC mapping

### 1. Resolve the SAC's underlying classic

Use stellar.expert's contract endpoint:

```sh
curl https://api.stellar.expert/explorer/public/contract/<C-strkey> \
  | jq -r '.asset'
```

Returns the underlying asset in `CODE-ISSUER-N` form (the trailing
`-N` is stellar.expert's display ordinal for multiple issuers
per CODE — strip it before adding to config).

Sanity-check by visiting `https://stellar.expert/explorer/public/asset/<asset>`
and confirming the issuer's home_domain matches your expectations.

### 2. Append to `[supply.sac_wrappers]` on r1

```sh
ssh root@136.243.90.96 'cat >> /etc/stellarindex.toml' << 'EOF'
"<C-strkey>" = "<CODE>:<G-strkey>"
EOF
```

Note the **colon** separator (not the dash that the canonical
`/v1/assets` asset_id form uses). This is the form
`[supply].sac_wrappers` parses.

### 3. Restart the api + indexer + aggregator

```sh
ssh root@136.243.90.96 'systemctl restart stellarindex-api stellarindex-indexer stellarindex-aggregator'
```

Verify with:

```sh
curl -s https://api.stellarindex.io/v1/sac-wrappers | jq '.data | length'
```

### 4. Bake into the ansible template

`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`
already has a `[supply.sac_wrappers]` block — append your new
entry there in the same PR so future re-renders don't lose it.

## Backfilling historical USD-volume

After a SAC entry that maps to a USD-pegged classic lands, NEW
trades will populate `trades.usd_volume` correctly. Trades that
landed BEFORE that config addition stay NULL.

To retroactively price them:

```sh
scp scripts/ops/recompute-usd-volume-soroban.sql root@136.243.90.96:/tmp/

ssh root@136.243.90.96 \
  'PGPASSWORD=$(cat /etc/stellarindex/postgres-password.txt) \
   psql -h 127.0.0.1 -U stellarindex -d stellarindex \
        -v ON_ERROR_STOP=1 \
        -c "SET timescaledb.max_tuples_decompressed_per_dml_transaction = 0;" \
        -f /tmp/recompute-usd-volume-soroban.sql'
```

The GUC is necessary because the trades hypertable uses chunk
compression and the default `max_tuples_decompressed_per_dml_transaction`
(100k) is below the typical scope of a backfill (~120k+ rows).

Idempotent — re-running is safe (filters on `usd_volume IS NULL`).

## Adding a new USD-pegged classic

If you add a SAC mapping pointing at a NEW USD-pegged stablecoin
(not just USDC), also extend `[trades].usd_pegged_classic_assets`
in `/etc/stellarindex.toml`:

```toml
[trades]
usd_pegged_classic_assets = [
  "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
  "USDx-GAVH5ZWACAY2PHPUG4FL3LHHJIYIHOFPSIUGM2KHK25CJWXHAV6QKDMN",  # NEW
]
```

Then both new and historical trades quoted in USDx (or its SAC
wrapper) will be priced via `usd_volume = quote_amount / 10^7`.
Update `scripts/ops/recompute-usd-volume-soroban.sql`'s WHERE
clause to include the new SAC.

## Pure-Soroban SEP-41 tokens (no USD-pegged quote at all)

The two paths above ("Adding a single SAC mapping" and "Adding a
new USD-pegged classic") both require the trade's QUOTE asset to
resolve to a USD-pegged classic — either directly or via a SAC
wrapper. A pure-Soroban SEP-41 token whose only liquidity route is
against XLM (no classic counterpart, no fiat-quoted pair) has no
quote-side USD peg to add, so neither path lights it up.

Two further tiers cover this case (ROADMAP #37 / L7.6), both gated
on `[trades].usd_pegged_classic_assets` being non-empty (which also
wires `VWAPUSDFXResolver` — see
`cmd/stellarindex-indexer/main.go`):

- **Tier 3** (existing, L2.2 Phase 2): fires when the trade's QUOTE
  asset — including plain `native` XLM — has a recent VWAP against
  one of the configured USD pegs in `prices_1m`. Covers pools that
  store the pair as `base=TOKEN, quote=XLM`.
- **Tier 4** (L7.6): fires when tier 3 declines (the quote is the
  pure SEP-41 token itself, with no USD-pegged market) AND the
  trade's BASE asset is `native` XLM or its SAC wrapper. Covers the
  mirror-image pool orientation, `base=XLM, quote=TOKEN` — `internal/sources`
  decoders don't re-orient trades to a canonical form, so both
  orientations occur on-chain depending on which pool token ordering
  the venue used. Values the trade off the XLM leg —
  `usd_volume = base_amount/1e7 × XLM/USD` — which needs no
  knowledge of the SEP-41 token's own decimals.

Both tiers are insert-time only (no retroactive backfill for trades
that landed before the resolver was wired — same posture as the
"Backfilling historical USD-volume" section above). A pure
SEP-41/SEP-41 pair (neither leg XLM nor USD-pegged) stays out of
scope on every tier — that needs a per-token oracle, not a
peg-anchor. `internal/storage/timescale/trades.go`'s `tradeUSDVolume`
docstring is the authoritative reference for the exact four-tier
order; `Store.SorobanVolume24hUSDForAsset` is the read-side
equivalent for the one field (`/v1/assets/{id}`'s `volume_24h_usd`)
that also has a query-time fallback for trades that predate tier 3/4
being wired.

## Bulk-resolve helper

When seeding many SACs at once (e.g. adding all top pools for a
new venue), this loop crawls the active pools for a source and
prints the config lines to paste:

```sh
for addr in $(curl -s "https://api.stellarindex.io/v1/pools?source=$SRC&limit=50" \
                | jq -r '.data[] | .base, .quote' \
                | grep '^C[A-Z0-9]' | sort -u); do
  asset=$(curl -s "https://api.stellar.expert/explorer/public/contract/$addr" \
            | jq -r '.asset // ""')
  if [ -n "$asset" ] && [ "$asset" != "null" ]; then
    code=$(echo "$asset" | cut -d- -f1)
    issuer=$(echo "$asset" | cut -d- -f2- | sed 's/-[0-9]*$//')
    [ -n "$issuer" ] && echo "\"$addr\" = \"$code:$issuer\""
  fi
done
```

## Related

- `scripts/ops/recompute-usd-volume-soroban.sql` — the backfill.
- `internal/storage/timescale/usd_volume_quote_spec.go` — the live
  USD-volume path (mirrors what the SQL backfill does).
- `internal/api/v1/known_issuers.go` — curated org-name fallback;
  add an entry alongside the SAC for explorer label parity.
