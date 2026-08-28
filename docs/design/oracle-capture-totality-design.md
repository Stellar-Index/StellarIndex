# Oracle capture-totality design — StellarIndex oracle_updates record layer (Reflector / RedStone / Band)

> Status: DESIGN (Wave B, 2026-08-28). Produced by a read-only investigation against HEAD 4aa93e96; every claim cites file:line. Open questions at the end are decisions for the operator.

# Oracle capture-totality — record layer total, interpretation layer selective

Date: 2026-08-28. Status: DESIGN (read-only investigation; nothing implemented). Scope: on-chain oracle sources `reflector-dex` / `reflector-cex` / `reflector-fx` / `redstone` / `band`, the `oracle_updates` hypertable, and every reader of it.

Operator directive: **the record layer must be total; only the interpretation layer is selective.** Every price entry an indexed oracle publishes on-chain is recorded, whether or not its symbol maps to a canonical asset we serve.

## 0. Position of oracles in the system (verified from code)

Oracles are REFERENCE data, never VWAP inputs:

- `internal/sources/external/registry.go:45-49,148` — every oracle-class source is `Class: ClassOracle, IncludeInVWAP: false`.
- `internal/aggregate/orchestrator/orchestrator.go:379-405` — `DisableClassFilter` zero value keeps the VWAP numerator to `ClassExchange` trades.
- Readers of `oracle_updates`: divergence (`internal/divergence/oracle.go`), confidence cross-oracle factor (`orchestrator/confidence.go lookupCrossOracle` — reads the divergence cache, not the table), Phase-2 freeze release lens (`phase2_freeze.go:168 CrossOracleMedian` — same cache), MEV detectors (`storage/timescale/mev.go OracleUpdatesForMEVScan`), `/v1/oracle/latest` + `/v1/oracle/streams` (`api/v1/oracle.go`), source bespoke page (`storage/timescale/bespoke_oracle.go`), `protocol_stats` 24h counts, `per_source_gaps`, `protocol_contracts` roster, `source_entry_counts`, the seven 0034 CAGGs, and the completeness / verify-reconciliation / ch-reproject tooling.

## 1. Current behaviour (the defect)

| Source | File | Unmapped path today | All-unmapped event |
|---|---|---|---|
| reflector-* | `internal/sources/reflector/decode.go` `decodeUpdateDataEntry` → `ErrUnknownSymbol`; `sdkDecodeUpdateBody` lands `PriceEntry{Skip:true}`; `decodeUpdate` `continue`s | dropped, counter bump | `ErrEmptyPrices` (decode error) |
| redstone | `internal/sources/redstone/decode.go:144-155` `lookupFeed` miss → counter + `continue` | dropped | `ErrEmptyUpdates` |
| band | `internal/sources/band/decode.go` `symbolToAsset` → `ErrUnknownSymbol` → counter + `continue` | dropped | `ErrEmptyRates` |

The counter `stellarindex_source_unknown_symbols_total` has no alert consumer (`internal/obs/metrics.go:898-918`; r1 carried 7,794 dropped reflector slots when that note was written). The 2026-07-24 RedStone expansion lost ~5,600 events until `feeds.go` caught up. Recovery today requires a code change AND a lake replay.

Not mapping-related and **stays dropped**: band `sym == "USD"` (contract rejects the write) and `rate == 0`; reflector/redstone non-positive prices (contract invariant). Totality is about symbol mapping only.

## 2. Representation of unmapped entries

### 2.1 Decision: `raw:` namespace in the existing `asset` column — zero migration

Schema facts (`migrations/0003`): `asset text NOT NULL`, `quote text NOT NULL`, no CHECK on format; PK `(source, ledger, tx_hash, op_index, ts)` excludes asset; `compress_segmentby = 'source, asset, quote'`; 0109 added `derive_generation` with the generation-guarded `DO UPDATE` upsert that rewrites asset/quote/price in place on the same PK. Nothing in SQL rejects `raw:…`.

Code facts (`internal/canonical/asset.go`): `Validate()` is closed over six allow-listed types; `ParseAsset()` prefix-dispatches only `fiat:` / `crypto:` / `rwa:`, then contract id, then splits on the first `-` or `:` — so `raw:BTC` becomes `NewClassicAsset("raw","BTC")` and fails. `InsertOracleUpdate` calls `Validate()`; `Asset.Value()` calls `Validate()`; every reader re-parses with `ParseAsset` (keyed readers return an error → 500; `LatestOracleStreams` silently `continue`s). **A `raw:` namespace is rejected by construction in Go, not in Postgres.** The change is therefore a new `AssetType`, not a migration.

New type:

```go
// AssetOracleRaw is an oracle-published symbol recorded VERBATIM because it
// maps to no canonical asset. Wire form: `raw:<symbol>`. Record-layer only:
// never a Pair leg, never a VWAP input, never compared by the interpretation
// layer. Source scoping comes from oracle_updates.source, not the code.
AssetOracleRaw AssetType = "raw"
```

- `Validate`: `Code` 1–64 bytes, printable ASCII `0x21–0x7E`, no whitespace; `Issuer`/`ContractID` empty. `/` and `.` are allowed (RedStone feed_ids are `ScString`, e.g. `SolvBTC.BBN_FUNDAMENTAL/USD`; Reflector/Band symbols are `ScSymbol` `[A-Za-z0-9_]{1,32}`).
- `ParseAsset`: `raw:` prefix dispatch placed BEFORE the classic split.
- `String()`: `"raw:" + Code`.
- `func (a Asset) IsMapped() bool { return a.Type != AssetOracleRaw }`.
- `canonical/pair.go NewPair` must refuse a raw leg (test).
- Exhaustive-switch guard (`asset_type_exhaustive_guard_test.go`) discovers the new const automatically and will fail on every unhandled `switch a.Type` — known sites: `asset.go` String/Validate, `internal/supply/key.go:32` (raw → same error as fiat/crypto/rwa), `internal/storage/timescale/bespoke_dex.go:305`.
- `OracleUpdate.Validate` self-price guard (`Asset.Equal(Quote)`) is unaffected — a raw asset never equals a fiat quote.

### 2.2 Trade-off vs a `raw_symbol` column

| | `raw:` namespace (chosen, Phase 1) | `raw_symbol text NULL` column (optional Phase 2) |
|---|---|---|
| Migration | none | additive, `DEFAULT NULL`, old-binary-safe (0109 precedent, cheap on compressed chunks in TS ≥ 2.11) |
| Unmapped predicate | `asset LIKE 'raw:%'` / `IsMapped()` | `raw_symbol IS NOT NULL AND asset = …`? — needs a placeholder asset for unmapped rows anyway (asset is NOT NULL), so it does not remove the namespace |
| Mapped rows | lossy: `EUROC/EUR` → `crypto:EUROC`+`fiat:EUR`; `SolvBTC_FUNDAMENTAL/USD` → code with `_USD`; Invert (MXNe) not recoverable from the row | preserves the on-wire symbol for EVERY row — a genuine auditability gain |
| Keying / CAGGs / segmentby | unchanged | unchanged |
| Promotion when allow-list grows | gen-N re-derive of the same PK flips `asset` in place (0109) | same, and `raw_symbol` stays as provenance |

Recommendation: ship the namespace now (totality with zero migration); schedule the column as a separate additive migration if the operator wants per-row provenance for mapped rows. The two compose.

### 2.3 Quote for unmapped rows

- reflector-*: `usdFiat` (variant-implied for every row today — `quoteForVariant`).
- band: `fiat:USD` (as today).
- redstone: if the feed_id ends in `/<ISO4217>` and `IsKnownFiat`, `fiat:<that>`; else `fiat:USD`. The full feed_id (including any suffix) is stored verbatim in the raw code. **No `Invert`** — orientation of an unmapped feed is unknown; unmapped rows are explicitly orientation-unknown and must never be compared. `mapped=false` is the only guard.

## 3. Decoder changes per source

Shared helper (fixes the fiat/crypto precedence inconsistency: reflector tries fiat→crypto, band tries crypto→fiat):

```go
// MapOracleSymbol: fiat → crypto → rwa → raw. ok=false means raw.
func MapOracleSymbol(sym string) (canonical.Asset, bool)
```

- **reflector** (`decode.go`): `decodeUpdateDataEntry` returns a raw `PriceEntry` instead of `ErrUnknownSymbol`; delete `PriceEntry.Skip` and the `Skip` branch in `decodeUpdate` (the placeholder existed only to hold the slot — the raw row now holds it). `ErrUnknownSymbol` becomes unused → remove or keep as deprecated alias for ops grep. `ErrEmptyPrices` now fires only for a genuinely empty/all-non-positive vector. OpIndex formula unchanged.
- **redstone** (`decode.go:144-155`): on `lookupFeed` miss build `feedEntry{Base: raw(feed_id), Quote: suffixQuote(feed_id), Invert: false}` and fall through; `ErrEmptyUpdates` now only for the empty on-wire batch path. Attribution (`resolveFeedAttribution`, state-write subset) is untouched — unknown feed_ids already participate in it.
- **band** (`decode.go`): on `symbolToAsset` miss use raw; keep USD and `rate == 0` skips.
- Metric: keep incrementing `SourceUnknownSymbolsTotal` (help text → "recorded under raw:"); no rename (dashboards).

Positional safety (DAT-03) is already true in all three decoders: reflector indexes the raw vector (Skip consumed the slot), redstone indexes the on-wire `prices` subset, band indexes the raw `symbol_rates` vector (`decode_test.go:300` pins `OpIndex=1` when USD is skipped at slot 0). Switching skip→emit fills empty slots and moves no existing row; a later allow-list extension re-derives the same PK and the 0109 guard promotes `raw:X` → `crypto:X` in place.

## 4. Consumer audit table

| Reader | File | Keying | Disposition |
|---|---|---|---|
| Divergence `OracleReference.LookupPrice` | `internal/divergence/oracle.go` via `LatestOracleObservation` | exact canonical strings (`oracleAssetKeys`) | **safe by keying**; add defensive `!IsMapped() → ErrAssetUnsupported` |
| Confidence `CrossOracleFactor` | `orchestrator/confidence.go lookupCrossOracle` | divergence cache | safe (derived) |
| Phase-2 freeze `CrossOracleMedian` | `orchestrator/phase2_freeze.go:168` | divergence cache | safe (derived) |
| `/v1/oracle/latest` | `api/v1/oracle.go handleOracleLatest` → `LatestOracleUpdatesForAssets` | `asset = ANY(candidates)` | safe by keying; **must be updated** to emit `mapped` and to accept an explicit `asset=raw:…` query |
| `/v1/oracle/streams` + explorer /oracles | `handleOracleStreams` → `LatestOracleStreams` | unfiltered DISTINCT ON | **must deliberately include** with `mapped:false` + badge; remove the silent `continue` on parse failure (log + counter) |
| Source bespoke page | `storage/timescale/bespoke_oracle.go` (text-only counts/latest table) | none | **deliberately include**; add an "Unmapped feeds" KPI + note |
| MEV sandwich | `aggregate/mev/inputs.go tradeTouches` | exact string | safe by keying |
| MEV liquidation cascade | `aggregate/mev/cascade.go:113-126` + `storage/timescale/mev.go:118` | **none** (any oracle row in window; evidence capped at 10) | **must be updated**: exclude `raw:` at the query or via `IsMapped()` |
| `protocol_stats` 24h counts, `protocol_events_24h` | `storage/timescale/protocol_stats.go:95` | source | include (totality is the count) |
| `per_source_gaps` | `per_source_gaps.go:365-369` | ledger | include |
| `protocol_contracts` roster | `protocol_contracts.go:182` | contract_id | include |
| `source_entry_counts` / diagnostics | `oracle.go` insert CTE, `seed_entry_counts.go` | source | include |
| CAGGs `oracle_prices_*` | `migrations/0034` | `GROUP BY source, asset, quote` (text) | include — raw rows form their own series; no Go consumer reads these CAGGs today (grep) |
| `LatestAggregatorPricesForPair` | `storage/timescale/oracle.go` | exact pair + aggregator sources | safe by keying |
| SEP-40 adapter `/v1/oracle/lastprice` | `api/v1/oracle_sep40.go` | reads prices_1m / Redis VWAP | not an oracle_updates reader |
| Chainlink/CoinGecko/… pollers | `sources/external/*` | writers | unaffected |
| Completeness / verify-reconciliation / ch-reproject | `ops/chops/*` | same decoders both sides | symmetric — see §6 |
| `lint-pk-discriminators` | `scripts/ci/…` | PK | unaffected |

Guard against the cascade class recurring: a repo test that every `FROM oracle_updates` in `internal/` is either asset-keyed, carries `asset NOT LIKE 'raw:%'`, or carries a `-- totality: includes unmapped` marker.

## 5. Explorer surface

- `components/AssetLink.tsx assetSlug` and `lib/asset-label.ts shortAssetText`: `raw:` → no slug (plain label), label = symbol after the prefix in monospace. Today `raw:X` would fall into the classic branch and link to `/assets/raw` (static-export 404).
- `app/oracles/OraclesView.tsx` streams table: when `mapped === false` render an `unmapped` badge, title "Recorded verbatim from the oracle; symbol is not mapped to a canonical asset — reference only, never compared or aggregated". Active-stream counts include unmapped rows (totality is what the page claims to show); add a per-oracle "of which unmapped" secondary count.
- `sources/[name]` page: the new bespoke KPI renders automatically.
- OpenAPI: `OracleReading.mapped: boolean` (required); regenerate `web/explorer/public/openapi` + `docs/reference/api`.

## 6. Backfill / replay plan and the completeness gate

Expected side of the gate = the SAME decoders run over the lake (`compute_completeness.go expectedProjection`: band via `reDeriveContractCallCensus` keyed `(tx, op_index, ts)`; reflector/redstone via `ReDeriveOutputCountsByKindFromEvents` summing `reflector.update` / `redstone.update`; catalogue at `reconciliation_catalogue.go:450-489`, oracle sources use the `aggregateReconcile` totals compare). Decode errors soft-fail to zero on both sides. Consequences:

1. Deploying the new decoder makes the expected side rise by every historical unmapped entry while served does not → all five oracle sources read incomplete (Δ = total unmapped rows) until replayed. Sequence deploy + replay in one window.
2. After replay, Δ returns to 0 with no gate change — the gate is symmetric by construction.

Per-source replay (gen-stamped writers; PK-stable so idempotent):

```
stellarindex-ops ch-rebuild -config /etc/stellarindex/stellarindex.toml \
  -sources reflector-dex,reflector-cex,reflector-fx,redstone -from <G> -to <TIP>
  # genesis per reconciliation_catalogue.go: dex 50_644_229, cex 50_644_239, fx 56_733_481, redstone 58_758_722
stellarindex-ops ch-rebuild -config … -contract-calls -contracts <BAND_STANDARD_REFERENCE_C> -from <G_band> -to <TIP>
  # catalogue says 60_000_000, per_source_gaps.go says 50_842_736 — REQUIRES-LIVE-VERIFY:
  #   psql -c "SELECT min(ledger) FROM oracle_updates WHERE source='band'"
stellarindex-ops seed-entry-counts -config …
psql: SELECT refresh_continuous_aggregate('oracle_prices_<grain>', <from_ts>, <to_ts>)  -- seven grains, per 0040 header
stellarindex-ops compute-completeness -config … -ch -pass
```

REQUIRES-LIVE-VERIFY before the full range: time a one-ledger rebuild and inspect `timescaledb_information.chunks` compression for `oracle_updates` — in-place promotion of raw rows later touches compressed segments keyed by asset.

Monitor to completion (do not fire-and-forget).

## 7. Tests

- canonical: ParseAsset/String round-trip for `raw:SolvBTC.BBN_FUNDAMENTAL/USD`, `raw:NOTACOIN`; rejects empty, whitespace, control bytes, 65+ bytes; `NewPair` refuses a raw leg; `Asset.Value()` accepts raw; exhaustive-switch guard green.
- decoders (rewritten from the existing skip tests):
  - reflector `TestRealDecoder_unknownSymbol…`: mixed USD+NOTACURRENCY → 2 rows, `updates[1].Asset == raw:NOTACURRENCY`, OpIndex slot 1; all-unknown → 1 raw row, no error.
  - redstone `TestDecode_UnknownFeedSkipped_KnownLands` → 3 rows, `raw:NOTAFEED` at OpIndex 1 with quote fiat:USD; `TestDecode_AllUnknown_ErrEmptyUpdates` → 2 raw rows, nil error; new test: unknown `XYZ/EUR` → quote fiat:EUR, no Invert.
  - band `TestDecodeRelay_UnknownSymbolSkipped` → 2 rows, `raw:NOTACOIN` OpIndex 0, BTC OpIndex 1; USD and rate-0 still skipped.
- **Red-proof** (memory rule "prove tests red before shipping"): with PR-2's emit branch temporarily reverted to `continue`, run `go test ./internal/sources/reflector/ ./internal/sources/redstone/ ./internal/sources/band/ -run 'Unknown|AllUnknown'` and paste the exact failures (expected 2/3/2 rows, got 1/2/err) into the PR body; then restore.
- storage/integration: raw round-trip through `InsertOracleUpdate` → `LatestOracleUpdateForAsset`; gen-N promotion `raw:X`→`crypto:X` on the same PK updates in place; gen-0 replay cannot demote.
- MEV: cascade test with 12 raw rows + 1 mapped row in window → evidence contains only the mapped row.
- API: `/v1/oracle/streams` returns `mapped:false` rows; `/v1/oracle/latest?asset=raw:NOTACOIN` returns the row with `mapped:false`.
- Explorer: OraclesView.test.tsx renders the badge and no link for a raw row.
- Completeness: `catalogue_completeness_test.go` case where the decoder emits raw rows and both sides count them.

## 8. PR breakdown (fixer wave)

1. **PR-1 canonical `raw` AssetType** — asset.go + pair.go + supply/key.go + bespoke_dex.go switches; tests. No behaviour change elsewhere.
2. **PR-2 decoders emit raw** — shared `MapOracleSymbol`, reflector/redstone/band; rewritten tests + red-proof in PR body. (Depends on PR-1.)
3. **PR-3 storage/integration** — raw round-trip, promotion test, `LatestOracleStreams` parse-failure counter. (Depends on PR-1.)
4. **PR-4 interpretation exclusions + API `mapped`** — mev.go/cascade filter, divergence defensive check, OracleReading.mapped + OpenAPI regen, bespoke KPI, oracle_updates-query lint test. (Depends on PR-1.)
5. **PR-5 explorer** — AssetLink/asset-label raw handling, unmapped badge, tests. (Depends on PR-4's OpenAPI.)
6. **PR-6 observability + docs** — metric help text, alert + runbook, metrics README, redstone/reflector READMEs (fix the wrong metric name), docs/protocols, ADR amendment, this design doc.
7. **PR-7 ops replay** (no code) — commands in §6, run after PR-2/3/4 are deployed fleet-wide (old indexers never write raw rows; mixed fleet = vintage-dependent totality).

## 9. Open questions

- Doc location (docs/design/ does not exist; docs/architecture/ or ADR-0046?).
- Phase-2 `raw_symbol` column for mapped-row provenance.
- `/v1/oracle/streams` default include_unmapped (recommend true).
- Alert threshold and allow-list ownership.
- Live inventory of unmapped symbols per source (REQUIRES-LIVE-VERIFY: metric read + one-day dry replay for row growth).
- Band replay genesis.

## Coverage attestation

Examined: reflector decode.go/events.go/dispatcher_adapter.go/decode_test.go; redstone decode.go(56-200, 395-420)/feeds.go/events.go(errors)/decode_test.go(376-440); band decode.go/events.go(errors)/decode_test.go(300-340); canonical asset.go/oracle.go/asset_crypto.go/asset_fiat.go/asset_rwa.go/asset_type_exhaustive_guard_test.go; migrations 0003/0034/0040(header)/0109/0116(refs); storage/timescale oracle.go/mev.go(118-150)/bespoke_oracle.go/protocol_stats.go/per_source_gaps.go/protocol_contracts.go/diagnostics.go/row_counts.go; divergence/oracle.go; orchestrator orchestrator.go(370-420)/confidence.go/phase2_freeze.go (grep-level); mev cascade.go/inputs.go/domain/mev.go; api/v1 oracle.go/oracle_cache.go(grep); ops/chops compute_completeness.go(1400-1560,1630-1800)/reconciliation_catalogue.go(440-490)/ch_reproject.go(20-130); pipeline/sink.go (grep); external/registry.go (grep); explorer OraclesView.tsx/AssetLink.tsx/asset-label.ts; openapi OracleReading; obs/metrics.go(895-925); cmd/stellarindex-ops help text.

NOT examined: redstone payload.go / resolveFeedAttribution internals (attribution unaffected by mapping — asserted from the call shape, not traced); chainlink/coingecko pollers; test/integration beyond grep; deploy/monitoring rule trees beyond a token grep for unknown_symbols; the projector (projected_rebuild.go) write path for oracle rows beyond confirming it routes reflector/redstone UpdateEvents through sink.handleEvent; live r1 state (all REQUIRES-LIVE-VERIFY items).


## Open questions

- Where should the design doc live — docs/design/ (does not exist) vs docs/architecture/ vs a new ADR-0046 amending ADR-0010/0014/0028? (F13)
- Should Phase-2 add the additive `raw_symbol text NULL` column so MAPPED rows also retain the on-wire symbol (feed_id `EUROC/EUR`, `SolvBTC_FUNDAMENTAL/USD` are lossy today)? Zero-migration namespace covers totality for unmapped rows only.
- Should /v1/oracle/streams default to include_unmapped=true (totality visible on the explorer) or false (API consumers unchanged, explorer opts in)? Recommendation: true with the badge; confirm with operator since it changes the row count of a public endpoint.
- Alert threshold for sustained unmapped rate (proposal: any unmapped rows for > 6h per source) and who owns extending the allow-list/feed registry.
- REQUIRES-LIVE-VERIFY: actual unmapped-symbol inventory per source today — operator command on r1: `journalctl -u stellarindex-indexer --since -1d | grep -i 'unknown' | head` plus the metric read in F2; and a one-ledger dry replay to size row growth.
- Band genesis ledger for the replay (60_000_000 vs 50_842_736).

## Risks

- Completeness gate goes red for all five oracle sources the moment the new decoder binary deploys (expected side rises by every historical unmapped entry) and stays red until PR-7's replay completes; sequence deploy + replay in one maintenance window or temporarily annotate the verdict.
- Compressed-chunk in-place UPDATE cost when raw rows are later promoted to mapped assets (segmentby includes asset) — REQUIRES-LIVE-VERIFY timing on one ledger before a full-range replay; a slow path may need the ops runbook to decompress the affected chunks first (as 0057 did for a PK swap).
- Row-count growth: reflector-cex/fx publish dozens of symbols per 5-minute event; if a large share is unmapped today (7,794 slots on r1 per metrics godoc; ratio unknown until measured) the oracle_updates hypertable and the 0034 CAGGs grow proportionally. Measure first: `SELECT source, count(*) FROM oracle_updates WHERE ts > now()-interval '1 day' GROUP BY 1` before/after a one-day replay.
- Unbounded namespace: a malicious/buggy relayer could publish arbitrary symbol strings that we now persist verbatim (64-byte cap + charset validator bounds this; still, raw codes must never be used as URL path segments or as log-injection vectors — quote them in logs).
- Any consumer added later that scans oracle_updates without keying by canonical asset (the cascade class of bug, F7) will silently include unmapped rows; add a lint/test that every `FROM oracle_updates` query in internal/ either keys by asset or carries the `NOT LIKE 'raw:%'` exclusion or a documented `-- totality: includes unmapped` marker.
- RedStone quote inference for unmapped feed_ids (suffix `/EUR`) is a heuristic; orientation (the MXNe Invert class) is unknowable for an unmapped feed, so unmapped rows are explicitly orientation-unknown and must never be compared — the mapped flag is the only guard.
- The band genesis disagrees between reconciliation_catalogue.go (60_000_000) and per_source_gaps.go (50_842_736); replaying from the wrong floor either wastes hours or leaves a hole.
- Old-binary compatibility during rollout: an OLD api binary reading a raw: row via LatestOracleStreams silently drops it (`continue`), and OLD keyed readers can never match it — safe; but an old INDEXER never writes raw rows, so a mixed fleet produces vintage-dependent totality until every writer is upgraded (note in the release plan).
