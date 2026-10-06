---
title: Domain lexicon — one word per concept
last_verified: 2026-10-06
status: binding
---

# Domain lexicon

One word per concept. `scripts/ci/lint-lexicon.sh` enforces the grep-able
subset (verbs, slog-only, `coin` vocabulary and constructor-shape ratchet
in `scripts/ci/lint-lexicon.baseline`); reviewers cite this file for the rest.

## The migration rule

1. New code MUST use the canonical term: no new `Coin*` symbol, no new
   `venue` outside config, no third asset-id encoding.
2. Renames ride other changes. No rename-only PRs; when a deviating file is
   edited anyway, migrate its vocabulary and delete its baseline entry.
   Baseline growth needs a `Baseline-Growth:` commit trailer.

## Concept → canonical term

| Concept | Canonical | Restricted / deprecated |
|---|---|---|
| Tradeable asset | **asset** | **coin**: only the wire enum `entity_type="coin"` (`internal/api/v1/changes.go`) and the proper name "USD Coin". **currency**: only the verified-currency catalogue (`internal/currency/`), never a generic asset. |
| Asset identity | dash form `CODE-ISSUER`, `native`, `C…`, `fiat:`/`crypto:`/`rwa:` prefixes (`canonical.ParseAsset`) | Colon form `CODE:ISSUER` + `XLM` lives only in `internal/supply/key.go`. NEVER add a third encoding; convert at the seam (see `usd_volume_quote_spec.go`). |
| Base/quote pair | **pair** | **market** only for the public `/v1/markets*` routes. |
| Price | **price** | **rate** only in FX pollers. `RateLimit*` is unrelated throttling. |
| Data origin | **source** | **venue** only in config (`ExternalVenueConfig`); **exchange** is a source class. |
| Transaction | `Transaction` for types, `Tx`/`tx_hash` for fields | Routes `/v1/tx/{hash}` vs explorer `/transactions/{hash}` are accepted drift; no third. |
| Op index | `OpIndex` | `OperationIndex` (minority; converge when touching the file). |
| Event / trade / observation / update | `consumer.Event` / `canonical.Trade` / per-source price point / oracle push | Four distinct concepts, one term each. Don't blur them. |
| Issuer | **issuer** | **anchor** only in the SEP-1/SEP-24 sense. |
| Range-limiting a caller value | **clamp** = saturate and proceed; **reject** = 400 | Say which. ADR-0015 closed-bucket adjustment saturates; `limit` / `window_seconds` ranges reject. |

## Verbs

`Get…` single keyed read; `List…` slice read; `…Batch` multi-key read;
`New…` constructor (signature shape: engineering-standards "Go idioms");
`Load…` read embedded/file data. Banned: `Fetch`, `Make`, `Enumerate`,
accessor-`Read` (lint fails on `func Fetch…` / `func Make…`).

## Type suffixes

`*View` wire projection, `*Row` storage row, `Envelope[T]` API envelope,
`*Snapshot` point-in-time aggregate, `*Response` wire response, `*Options`
constructor options. Don't invent new ones.

## Leave alone

`fiat:`/`crypto:`/`rwa:` prefixes, `Source`/`SourceName`, plural collection
routes, package plural/singular mix.
