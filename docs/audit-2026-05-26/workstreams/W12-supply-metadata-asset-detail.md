# W12 — Supply, metadata, asset detail enrichment

## Scope

Every field surfaced by `/v1/assets/{slug}`.

- `internal/supply/`: xlm, classic, sep41, reader/writer split,
  cross-check, refresher, policy, overlay
- `internal/currency/`: hand-curated verified-currency catalogue,
  marketcap
- `internal/metadata/`: SEP-1 / stellar.toml resolution
- `internal/api/v1/assets*.go` set: assets, assets_global,
  assets_f2, assets_coin_extension, assets_sep1, assets_verified
- `cmd/ratesengine-ops/supply.go`, `sep1_refresh.go`

## Inputs

- the directories above
- ADR-0011 (supply algorithm), ADR-0021/22/23 (supply observers)
- `internal/currency/data/seed.yaml`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W12.1 | Supply algorithm 1 (XLM): per `internal/supply/xlm.go` | code + test |
| W12.2 | Supply algorithm 2 (classic): trustlines + claimables + LP reserves + SAC | per-observer |
| W12.3 | Supply algorithm 3 (SEP-41): mint/burn delta | `sep41.go` |
| W12.4 | reader/writer split: storage_classic_reader, storage_sep41_reader | code |
| W12.5 | crosscheck + refresher: how often, what triggers | `crosscheck_refresher.go` |
| W12.6 | overlay: when used | `overlay.go` |
| W12.7 | textfile output for node_exporter | `textfile.go` |
| W12.8 | Verified-currency catalogue (`seed.yaml`): every entry is hand-curated, no auto-population | YAML inspection |
| W12.9 | Catalogue propagates to: CG poller ticker map, indexer aggregator pair set, unverified-collision warning, /v1/assets/verified, explorer badge UI | grep usage |
| W12.10 | SEP-1 resolution: SSRF defence (`metadata/ssrf_internal_test.go`) | tests |
| W12.11 | SEP-1 caching: TTL + invalidation | `metadata/cache.go` |
| W12.12 | LCM resolver: helpers + tests | `metadata/lcm_resolver*.go` |
| W12.13 | /v1/assets/{slug} dual-shape behaviour (GlobalAssetView vs AssetDetail) | per CLAUDE.md surprise + handler |
| W12.14 | NEW: ATH / sparkline / top_markets / fiat market cap chart paths (rc.43..rc.46) | per-feature audit |
| W12.15 | NEW: currency/marketcap path (added since baseline) | code |
| W12.16 | NEW: `cmd/ratesengine-ops/supply.go` snapshot CLI help/error text matches reality (prior finding from 05-12 audit — re-test cold) | shell |
| W12.17 | sep1_refresh: trigger + rate limit | code |

## Closure criteria

All checks terminal. Findings on:
- any silent fallback (verified catalogue entry missing → API
  silently returns empty)
- any SSRF in SEP-1 fetch
- any cache-key drift in supply layer
