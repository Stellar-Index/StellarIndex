---
title: Domain lexicon — one word per concept
last_verified: 2026-07-05
status: binding
---

# Domain lexicon

**One word per concept.** This is the codification pass of the
2026-07-01 maintainability audit's D2 dimension
(`docs/maintainability-audit-2026-07-01/D2-naming-lexicon.md`): the
canonical term for every domain concept, the deviations that exist
today (with file:symbol pointers), and the migration rule.

## The migration rule

1. **New code MUST use the canonical term.** No new `Coin*` symbol, no
   new `venue` outside config, no third asset-id encoding.
   `scripts/ci/lint-lexicon.sh` enforces the grep-able subset (runs in
   `verify.sh` and CI); the rest is review-enforced via this doc.
2. **Renames ride other changes.** Do not open rename-only PRs; when a
   deviating file is being materially edited anyway, migrate its
   vocabulary in the same PR and delete its
   `scripts/ci/lint-lexicon.baseline` entry.
3. **The `Coin*`→`Asset*` bulk rename is PENDING and deferred to
   @ash** (it sweeps the storage read-layer + api read-path in one
   go; audit rename-map item 1). Until it lands, the deviations below
   are grandfathered in the lint baseline — frozen, not growing.

## Concept → canonical term

| Concept | Canonical | Deprecated / restricted synonyms | Where the deviations live |
|---|---|---|---|
| A tradeable asset (classic, SAC, SEP-41, fiat) | **asset** | **coin** (deprecated — rename pending, deferred to @ash): `internal/storage/timescale/coins.go` (`CoinRow`, `ListCoins`, `ListCoinsExt`, `CoinsOrder`, `GetCoinBySlug`, `GetCoinATH`, `GetCoinsATHBatch`, …), `internal/api/v1/coins.go` (`CoinsReader`), `internal/api/v1/coins_cache.go`, `internal/api/v1/assets_coin_extension.go`, `internal/api/v1/changes.go:62` (wire `entity_type="coin"` on `/v1/changes/coin/{id}`), `pkg/client/endpoints.go:638` (dangling `CoinsOptions` doc comment — the type was removed with `/v1/coins`, the comment and the `[CoinsOptions]` doc-link on `IssuersOptions` were not). **currency** (restricted): allowed ONLY for the verified-currency catalogue domain (`internal/currency/` — the hand-curated trust surface; that is its real name, keep it). Never for a generic asset. |
| Asset identity (wire + storage) | **dash form**: `CODE-ISSUER`, `native`, `C…`, `fiat:USD` / `crypto:XLM` / `rwa:…` prefixes (`canonical.ParseAsset`) | **colon form** `CODE:ISSUER` + literal `XLM`: `internal/supply/key.go` (`supply.AssetKey`) — a second, deliberate encoding for supply hypertable keys. Net effect: native has three ids (`native`, `XLM`, `crypto:XLM`), every classic asset has two — a standing "why did the join return zero rows" source. Rule: NEVER introduce a third encoding; convert at the seam like `internal/storage/timescale/usd_volume_quote_spec.go` does (normalises `supply.AssetKey` colon form via `canonical.ParseAsset`). Audit rename-map item 2 (converge or rename to `SupplyKey`) is open. |
| A base/quote trading pair | **pair** (`canonical.Pair`, `/v1/pairs`) | **market** — accepted ONLY as the public wire surface of `/v1/markets` + `/v1/markets/sources` (`internal/api/v1/market_sources.go`). Picking one public noun is an API-version decision (audit rename-map item 4); internally, say pair. |
| A price value | **price** | **rate** — FX-vendor terminology only, inside the FX pollers (`internal/sources/external/ecb/`, `exchangeratesapi/`, `polygonforex/`). NOTE: `RateLimit*` / `ratelimit` is UNRELATED (request throttling) — never sweep it in a rename. |
| A data origin | **source** (`canonical.Trade.Source`, `external.Registry`) | **venue** — config-surface only (`internal/config/config.go:242` `ExternalVenueConfig` + CLAUDE.md recipes); **exchange** — a source *class* (`external.ClassExchange`), not a synonym for source. New code: source. |
| Ledger | **ledger** | Clean — zero `block` leakage. Keep it that way. Route note: `/v1/ledgers` (collection) vs `/v1/ledger/tip` + `/v1/ledger/stream` (singleton sub-resources) is accepted, documented drift. |
| Transaction | **`Transaction`** for types/XDR; **`Tx`/`tx_hash`** for field names + short forms | The boundary is deliberate: full word for types, abbreviated for the ubiquitous hash field. Route drift: API `/v1/tx/{hash}` vs explorer `/transactions/{hash}` (SEO decision, 2026-06-24) — accepted, don't add a third. |
| Operation index within a tx | **`OpIndex`** (~440 uses) | `OperationIndex` (~116 uses) — the minority form; converge to `OpIndex` when touching a file anyway (audit rename-map item 3). New code: `OpIndex`. |
| Soroban contract event (transport level) | **event** (`consumer.Event`, `internal/events`, `soroban_events`) | — |
| An executed swap/fill | **trade** (`canonical.Trade`, `trades`) | — |
| A recorded per-source price point | **observation** (`/v1/price` … `Observations`, `divergence_observations`) | — |
| An oracle push | **update** (`oracle_updates`) | — (event/trade/observation/update are four DISTINCT concepts, each with exactly one term — don't blur them) |
| Asset issuer | **issuer** | **anchor** — restricted to the SEP-1/SEP-24 anchor sense (an anchor IS an issuer with services); not a general synonym. |

## Verb lexicon (already consistent — enforced at zero)

| Verb | Meaning | Notes |
|---|---|---|
| `Get…` | single keyed read | returns one item or error |
| `List…` | slice read | plural noun; keyset pagination where applicable |
| `…Batch` | multi-key read | e.g. `GetCoinsATHBatch` (name aside) |
| `New…` | constructor | the universal ctor verb — see engineering-standards "Go idioms" for the signature shape |
| `Load…` | read embedded/file data | e.g. `currency.LoadEmbedded`, `incidents.Load` |

**Banned verbs:** `Fetch`, `Make`, `Enumerate`, accessor-`Read` — the
repo has zero today and `lint-lexicon.sh` fails the build on the first
one (`func Fetch…` / `func Make…`).

## Type-suffix system (already consistent — don't invent new ones)

`*View` (wire projection) · `*Row` (storage row) · `Envelope[T]` (API
envelope) · `*Snapshot` (point-in-time aggregate) · `*Response` (wire
response; the lone `MarketSourcesResp` abbreviation in
`internal/api/v1/market_sources.go:46` is grandfathered — public
SemVer surface, accept-with-doc) · `*Options` (constructor options).

## What NOT to churn

The audit found these consistent — leave them alone: the
`fiat:`/`crypto:`/`rwa:` off-chain prefixes; `Source`/`SourceName`;
plural collection routes (two documented singular exceptions above);
package plural/singular mix (`events`/`incidents` vs
`currency`/`supply` — cosmetic, M2, not worth the git-blame damage).

## Enforcement

- `scripts/ci/lint-lexicon.sh` — zero rules (Fetch/Make verbs,
  non-slog loggers) + shrink-only per-file ratchet
  (`scripts/ci/lint-lexicon.baseline`) for `coin` vocabulary and the
  two non-canonical constructor shapes. Wired into `verify.sh`,
  `make lint-lexicon`, and CI's import-checks job; baseline growth
  requires a `Baseline-Growth:` commit trailer
  (`scripts/ci/lint-baseline-growth.sh`).
- Everything else in this doc is review-enforced; reviewers cite this
  file by concept row.

## Related

- `docs/maintainability-audit-2026-07-01/D2-naming-lexicon.md` — the
  audit evidence + M0/M1/M2 grading behind this lexicon.
- [engineering-standards.md](../engineering-standards.md) — the "Go
  idioms" section (D6 companion to this doc).
- `docs/architecture/coins-to-assets-migration.md` — the completed
  `/v1/coins` → `/v1/assets` HTTP migration (the wire side is done;
  this lexicon tracks the surviving internal vocabulary).
