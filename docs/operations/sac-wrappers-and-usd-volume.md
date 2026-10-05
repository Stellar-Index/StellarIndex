---
title: SAC wrappers + Soroban USD-volume backfill
last_verified: 2026-07-09
status: draft
---

# SAC wrappers + Soroban USD-volume backfill

Add a Stellar-Asset-Contract mapping and retroactively price the historical
trades that pre-date it.

## Why this matters

Soroban DEX sources (Soroswap, Phoenix, Aquarius, Comet) emit `base_asset` /
`quote_asset` as the SAC contract ID, not the classic asset. Without an
operator-config mapping from C-strkey to "CODE-ISSUER":

1. The explorer (`/dexes`, `/markets` AssetLabel) shows raw C-strkeys.
2. `/v1/sac-wrappers` returns an empty map.
3. `trades.usd_volume` stays NULL: the on-chain USD-volume path can't follow
   `quote_asset = C…` back to a USD-pegged classic.

A config line fixes all three for new trades; a backfill prices historical rows.

## Adding a single SAC mapping

### 1. Resolve the SAC's underlying classic

```sh
curl https://api.stellar.expert/explorer/public/contract/<C-strkey> \
  | jq -r '.asset'
```

Returns `CODE-ISSUER-N`; strip the trailing `-N` (stellar.expert's display
ordinal). Sanity-check the issuer's home_domain at
`https://stellar.expert/explorer/public/asset/<asset>`.

### 2. Append to `[supply.sac_wrappers]` on r1

```sh
ssh root@136.243.90.96 'cat >> /etc/stellarindex.toml' << 'EOF'
"<C-strkey>" = "<CODE>:<G-strkey>"
EOF
```

Separator is a **colon** (not the dash of the canonical `/v1/assets` asset_id);
that is the form `[supply].sac_wrappers` parses.

### 3. Restart the api + indexer + aggregator

```sh
ssh root@136.243.90.96 'systemctl restart stellarindex-api stellarindex-indexer stellarindex-aggregator'
```

Verify:

```sh
curl -s https://api.stellarindex.io/v1/sac-wrappers | jq '.data | length'
```

### 4. Bake into the ansible template

Append the entry to the `[supply.sac_wrappers]` block in
`configs/ansible/roles/archival-node/templates/stellarindex.toml.j2` in the
same PR so re-renders don't lose it.

## Backfilling historical USD-volume

New trades populate `trades.usd_volume` once a USD-pegged SAC entry lands;
earlier trades stay NULL. Price them with `stellarindex-ops usd-volume-restamp`
`-tier exact -fill-null`. A SAC-quoted trade whose wrapper resolves to a
USD-pegged classic is exact-tier (`quote_amount / 10^7`); the tool classifies
from the same `[trades].usd_pegged_classic_assets` and `[supply.sac_wrappers]`
the insert path reads, so there is no hand-kept `IN (…)` list.

```sh
# dry run: per-day candidate counts, nothing written
stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml \
  -tier exact -fill-null -sources aquarius,soroswap,phoenix,comet \
  -from <first-day> -to <last-day>
# apply on r1 under the heavy wrapper; add -chunks for any window older
# than the trades compression policy's 15 days
set -a; . /etc/default/stellarindex; set +a
/usr/local/sbin/run-heavy-job.sh usd-sac-fill \
  /usr/local/bin/stellarindex-ops usd-volume-restamp \
    -config /etc/stellarindex.toml -tier exact -fill-null -chunks \
    -sources aquarius,soroswap,phoenix,comet \
    -from <first-day> -to <last-day> -write
```

Then run the windowed CAGG refreshes the tool prints: it refreshes nothing
itself, and every served volume surface reads a continuous aggregate. Full
procedure and flags: [usd-volume-rederive-2026-08.md, Step
5](usd-volume-rederive-2026-08.md).

Do NOT backfill with a hand-written `UPDATE trades … WHERE usd_volume IS
NULL`. `trades` is a compressed hypertable; an UPDATE with no `ts` predicate
makes every chunk a result relation in one transaction (measured: 260 result
relations, ~270 GB WAL, on a host whose `pg_wal` sits on a 49 GB root fs). It
also leaves `derive_generation` at 0, so a later live re-write reverts it. The
tool slices by `ts`, lifts the decompression cap with `SET LOCAL` per slice and
stamps the run's generation. `scripts/ci/lint-migration-commands.sh` fails an
ops SQL script that runs DML on a compressed hypertable without a time-column
predicate.

## Adding a new USD-pegged classic

For a SAC pointing at a NEW USD-pegged stablecoin (not just USDC), also extend
`[trades].usd_pegged_classic_assets` in `/etc/stellarindex.toml`:

```toml
[trades]
usd_pegged_classic_assets = [
  "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
  "USDx-GAVH5ZWACAY2PHPUG4FL3LHHJIYIHOFPSIUGM2KHK25CJWXHAV6QKDMN",  # NEW
]
```

Trades quoted in USDx (or its SAC) are then priced `usd_volume = quote_amount /
10^7`: new ones at insert, historical ones by the `usd-volume-restamp` run above.

## Pure-Soroban SEP-41 tokens (no USD-pegged quote at all)

The paths above need the QUOTE asset to resolve to a USD-pegged classic. A
pure SEP-41 token whose only route is against XLM has none, so two further
tiers apply (ROADMAP #37 / L7.6), both gated on `[trades].usd_pegged_classic_assets`
being non-empty (which also wires `VWAPUSDFXResolver`, see
`cmd/stellarindex-indexer/main.go`):

- **Tier 3** (L2.2 Phase 2): the quote asset, including plain `native` XLM, has
  a recent VWAP against a configured USD peg in `prices_1m`. Covers
  `base=TOKEN, quote=XLM`.
- **Tier 4** (L7.6): tier 3 declined (quote is the SEP-41 token, no USD-pegged
  market) AND the BASE is `native` XLM or its SAC. Covers `base=XLM,
  quote=TOKEN` (decoders in `internal/sources` don't re-orient trades). Values
  off the XLM leg: `usd_volume = base_amount/1e7 × XLM/USD`, needing no knowledge
  of the token's decimals.

Both tiers are insert-time only (no retroactive backfill). A SEP-41/SEP-41 pair
(neither leg XLM nor USD-pegged) stays out of scope on every tier: it needs a
per-token oracle. `tradeUSDVolume`'s docstring in
`internal/storage/timescale/trades.go` is authoritative for the four-tier
order; `Store.SorobanVolume24hUSDForAsset` is the read-side equivalent for
`/v1/assets/{id}`'s `volume_24h_usd`, which also has a query-time fallback for
trades that predate tier 3/4.

## Bulk-resolve helper

Prints config lines for the active pools of a source:

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

- `internal/ops/chops/usd_volume_restamp.go`: the backfill.
- `internal/storage/timescale/usd_volume_quote_spec.go`: the live USD-volume
  path; restamp classifies through the same spec.
- `internal/api/v1/known_issuers.go`: curated org-name fallback; add an entry
  alongside the SAC for explorer label parity.
