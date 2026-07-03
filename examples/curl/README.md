# curl examples

Each script is self-contained and uses `${API_BASE_URL:-https://api.stellarindex.io}`
plus optional `${STELLARINDEX_API_KEY}` for authenticated endpoints.

```sh
# Run a single example
bash examples/curl/01-healthz.sh

# Run them all (smoke test)
for f in examples/curl/*.sh; do bash "$f"; done

# Hit a local indexer
API_BASE_URL=http://localhost:3000 bash examples/curl/01-healthz.sh
```

## Index

| # | Script | Endpoint |
|---|--------|----------|
| 01 | [`01-healthz.sh`](01-healthz.sh) | `GET /v1/healthz` — liveness probe |
| 02 | [`02-signup.sh`](02-signup.sh) | `POST /v1/signup` — get a free-tier key |
| 03 | [`03-account-me.sh`](03-account-me.sh) | `GET /v1/account/me` — your tier + rate limit |
| 04 | [`04-assets.sh`](04-assets.sh) | `GET /v1/assets?limit=N&order=volume_24h_usd:desc` — top assets by volume |
| 05 | [`05-price.sh`](05-price.sh) | `GET /v1/price?asset=…&quote=fiat:USD` — VWAP price |
| 06 | [`06-price-stream.sh`](06-price-stream.sh) | `GET /v1/price/stream` — SSE closed-bucket price ticks |
| 07 | [`07-ohlc.sh`](07-ohlc.sh) | `GET /v1/ohlc?base=…&quote=…` — single OHLC bar |
| 08 | [`08-history.sh`](08-history.sh) | `GET /v1/history?base=…&quote=…` — per-trade records |
| 09 | [`09-oracle-latest.sh`](09-oracle-latest.sh) | `GET /v1/oracle/latest?asset=…` — Reflector/Band/Redstone last update |
| 10 | [`10-markets.sh`](10-markets.sh) | `GET /v1/markets` — distinct (base, quote) pairs with activity |
| 11 | [`11-price-at.sh`](11-price-at.sh) | `GET /v1/price/at?asset=…&quote=fiat:USD&ts=…` — point-in-time price (cost basis / PnL) |
| 12 | [`12-history-since-inception.sh`](12-history-since-inception.sh) | `GET /v1/history/since-inception?asset=…` — full bucketed VWAP series |
| 13 | [`13-sac-wrappers.sh`](13-sac-wrappers.sh) | `GET /v1/sac-wrappers` — SAC contract → classic-asset registry |
| 14 | [`14-asset-detail.sh`](14-asset-detail.sh) | `GET /v1/assets/{id}` — full detail for one asset |

## Asset identifiers

The `asset` / `base` / `quote` parameters take canonical
identifiers:

- `native` — XLM
- `<code>-<G-strkey>` — any classic asset (e.g.
  `USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN`)
- `C<contract-id>` — any Soroban SEP-41 token
- `fiat:USD`, `fiat:EUR`, … (ISO 4217) — fiat quotes only (response side)

Use `bash examples/curl/04-assets.sh 100 | jq -r '.data[].asset_id'`
to enumerate live identifiers.
