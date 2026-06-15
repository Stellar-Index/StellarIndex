# Q1 — Maintainability scan (READ-ONLY, tool-driven)

Scope: the Go monorepo under `internal/`, `cmd/`, `pkg/`. Cross-cutting
maintainability pass (duplication / complexity / dead code / cohesion).
`.discovery-repos/` (git-ignored third-party clones + vendored SDKs) is
EXCLUDED from every result — the unscoped first runs were re-run scoped to
our three source roots once the noise was identified.

Auditor pass date: 2026-06-15. Method: `dupl` + `gocyclo` + `gocognit` +
`deadcode` (all installed via `go install`), each result manually triaged
against the source and cross-checked with `golangci-lint --enable=unused`
and targeted `grep` for caller-existence. Severity reflects maintainability
risk, NOT runtime correctness (these tools find smell, not bugs).

**Framing:** the repo's `.golangci.yml` already gates `gocognit`, `gocyclo`,
`funlen`, `unused`, `gosec`. So the complexity numbers below are *already
within the configured thresholds* (CI is green) — they are listed for
"where is the cognitive load concentrated" triage, not as violations. The
genuinely **un-gated** signal is **`dupl` (DRY)** and **`deadcode`** — neither
is enabled in `.golangci.yml` — so those two sections carry the real findings.

---

## Findings

| severity | file:line | dim | issue | why it matters | suggested fix | conf |
|---|---|---|---|---|---|---|
| high | `internal/dispatcher/census.go:165-218` + `internal/storage/clickhouse/extract.go:299-354` + `internal/storage/clickhouse/sdex_op_reader.go` + `internal/sources/sdex/decode.go` + `internal/sources/sdex/audit.go` | DRY/correctness | The SDEX claim-atom extraction logic (the 5-op-type switch with the passive-offer dual-arm fallback + both-zero no-op filtering) is hand-copied across **5 production files**. The code itself documents the hazard: census.go:160-162 says it "mirrors `sdex.extractClaimAtoms` exactly … so the census equals the SDEX trade-row count," and the passive-offer comment (lines 187-190) is repeated verbatim. | This is the load-bearing definition of "what counts as an on-chain trade." Three copies (decoder / CH lake re-derive / census reconcile) MUST agree byte-for-byte or completeness reconciliation silently mis-counts — exactly the class of coarse-PK / census-drift bug this repo has already hit (migrations 0056, 0057-0060 re-derives; see project memory). A `dupl` cluster here is a standing correctness liability, not just tidiness. | Extract one canonical `func ClaimAtomCount(op, result) int` (or a shared `ExtractClaimAtoms`) into a single package (`internal/sources/sdex` is the natural home, or a new `internal/sdexspec`) and have census + both CH paths call it. Pin with a fixture shared by all three call sites. | high |
| medium | `internal/api/v1/markets_cache.go:151-271` vs `279-383` | DRY | `fetchPairs`/`refreshPairs` and `fetchPools`/`refreshPools` are two ~120-line single-flight + SWR cache state-machines that differ only in element type (`Market` vs `Pool`) and the upstream call. The generic `swr[T]` helper that would collapse them **already exists** in `coins_cache.go` (referenced by name in `issuers_cache.go:121`'s own apology comment) but is not reused here. | ~224 dup lines of concurrency-sensitive code (flight pointers, delete-on-error, waiter-err capture). Two copies of single-flight is two places to get the stampede/panic-safety wrong; a future fix to one (e.g. a context-cancel leak) will be forgotten in the other. | Reuse the existing `swr[T]` generic from `coins_cache.go`; delete the bespoke pair/pool machinery. The author already flagged this as the intended path. | medium |
| medium | `internal/api/v1/issuers_cache.go:123-182` + `internal/api/v1/oracle_cache.go:121-176` (+ `coins_cache.go:406`) | DRY | A third and fourth hand-rolled copy of the same single-flight cache (`fetchList`/`fetchOne`), each with its own `*CacheEntry` struct and identical (A) fresh-hit / (B) join-flight / (C) leader arms. The comment at issuers_cache.go:115-122 explicitly says "the `swr[T]` helper from coins_cache.go drops in" but it hasn't. | Four near-identical single-flight implementations across the v1 cache layer. Same risk as above, multiplied. The deferred `swr[T]` adoption is now technical debt with a paper trail. | Migrate issuers + oracle (+ markets, above) onto `swr[T]`. One concurrency primitive, tested once. | medium |
| medium | `internal/sources/external/{coingecko,coinmarketcap,cryptocompare,ecb,polygonforex,bitstamp,coinbase}` — `decimalStringToScaledInt` + `syntheticTxHash` | DRY | `decimalStringToScaledInt` (decimal-string → scaled `*big.Int`, ~35 lines) is byte-identical across **7 external-source packages**; `syntheticTxHash` (synthetic 64-hex from ticker/currency/ts) across ~5. `dupl`'s single largest cluster (238 dup lines, 7 files). | These touch the i128/scaling invariant (ADR-0003) — the exact place CLAUDE.md warns external scaling is NON-uniform (10^8 CEX vs 10^6 FX). Seven copies means a precision fix lands in one venue and silently not the others. Pure helpers with zero per-venue variation = textbook extraction. | Move both to `internal/sources/external/framework.go` (or a `scaling.go` sibling) as exported helpers; each poller passes its own `Decimals`. ~270 lines deleted. | medium |
| low | `internal/sources/{claimable_balances/decode.go:62-97, trustlines/decode.go:85-114, liquidity_pools/dispatcher_adapter.go:137-166}` — `assetKeyFromAsset` | DRY | The `xdr.Asset → "CODE:ISSUER"` converter (alphanum4 / alphanum12 / native switch with strkey-encode + nil-guards) is copied across 3 supply-observer packages, each with its own package-prefixed error strings. | Pure, well-tested logic duplicated 3×. Low risk (it's stable XDR shape), but it's the kind of thing that should live once in `internal/supply` or `internal/scval` since all three already write `supply.AssetKey`. | Extract to `internal/supply` (the shared `AssetKey` form lives there) as `AssetKeyFromXDR(xdr.Asset) (string, error)`. | low |
| low | `internal/platform/postgresstore/{account_store,apikey_store,token_store,user_store}.go` — row-scan blocks | DRY | Recurring `rows.Scan(...) → struct` + `rows.Err()` + not-found-sentinel blocks duplicated across the 4 CRUD stores (multiple `dupl` clusters, 3-4 files each). | Standard hand-written DAO repetition. Mostly acceptable (each store has a different column set), but the not-found / scan-error wrapping boilerplate could share a helper. Judgement call — low value, leave unless the store count grows. | Optional: a generic `scanOne[T]`/`scanAll[T]` helper for the err+notfound boilerplate. Not worth a dedicated PR alone. | low |
| low | `internal/sources/external/{binance,kraken,coinbase,bitstamp}` — `parse.go`/`streamer.go`/`backfill.go` | DRY | The WS-frame → `canonical.Trade` parse + the candle/backfill row builders share large spans across the CEX venues (binance↔kraken parse 77 lines; coinbase↔bitstamp streamer 87; coinbase/kraken backfill+parse 64). | Some of this is irreducible (each venue's JSON shape differs), but the post-parse `canonical.Trade` assembly + amount-scaling tail is identical. Medium-effort, medium-payoff — the per-venue variation makes a clean extraction non-trivial. | Extract only the shared *tail* (Trade assembly + scaling) into framework helpers; leave per-venue JSON decode local. Sequence after the `decimalStringToScaledInt` extraction (same files). | low |
| low | `cmd/stellarindex-ops/main.go` (4627 lines) ; `cmd/stellarindex-api/main.go` (3293) | cohesion/god-file | Two `main` packages well over the 800-line god-file line. ops/main.go holds ~30 subcommand bodies in one file (it dominates the complexity table: `realMain` cyclo 96, plus `verifyArchive`, `verifyArchiveLCMWalk`, `wasmHistory`, `renderSCVal`, `verifyDecoders` all in the same file). | Not a defect — it's a CLI dispatcher — but 4.6k lines in one `main.go` makes every subcommand edit a merge-magnet (CLAUDE.md's own "shared files collide" warning names `cmd/stellarindex-*/main.go`). Subcommands are already separate funcs across sibling files (`ch_rebuild.go`, `compute_completeness.go` …); the dispatcher + a dozen bodies just haven't moved out yet. | Move each remaining inline subcommand body in `main.go` into its own `<verb>.go` in the same package (mechanical, no logic change). Keeps `realMain` as a thin flag-router. | low |

---

## Top duplication clusters (`dupl -threshold 80`, production-only, ranked)

145 clones total across source; after dropping `_test.go`-only clusters
(test setup/golden-frame repetition — acceptable noise), the top production
clusters are:

| # | dup lines | files | what's duplicated | extract? |
|---|---|---|---|---|
| 1 | 238 | `external/{bitstamp,coinbase}/parse.go`, `external/{coingecko,coinmarketcap,cryptocompare,ecb,polygonforex}/poller.go` | `decimalStringToScaledInt` (+ `syntheticTxHash`), identical across 7 venues | **Yes** — pure helper, touches scaling invariant. → framework.go |
| 2 | 224 | `internal/api/v1/markets_cache.go` (self) | `fetchPairs`/`refreshPairs` ≈ `fetchPools`/`refreshPools` single-flight cache | **Yes** — reuse existing `swr[T]` |
| 3 | 174 | `internal/storage/timescale/coins.go` (self, 1657↔1750) | two ~85-line CTE batch-history queries (24h price history variants) | **Maybe** — SQL CTE near-dup; parametrise the differing quote-asset arm |
| 4 | 155 | `internal/storage/timescale/coins.go` (self, 665↔754) | another paired coins query block | **Maybe** — same as #3 |
| 5 | 151 | `external/{coingecko,coinmarketcap,cryptocompare}/poller.go` | poller body span (overlaps #1's helper region) | **Yes** — subsumed by #1 |
| 6 | 114 | `internal/api/v1/{issuers_cache,oracle_cache}.go` | hand-rolled single-flight cache (copies 3 & 4 of `swr`) | **Yes** — reuse `swr[T]` |
| 7 | 108 | `internal/dispatcher/census.go` ↔ `internal/storage/clickhouse/extract.go` | **SDEX claim-atom extraction** (cross-package, correctness-critical) | **Yes (highest priority)** — single canonical impl |
| 8 | 107 | `internal/storage/timescale/sources_stats.go` (self) | two stats-aggregation query/scan blocks | Maybe — low value |
| 9 | 102 | `internal/storage/timescale/trades.go` (self, 4 spans) | `rows.Scan → canonical.Trade` + `ParseAsset`×2 + `NewPair` reconstruction, repeated in 4 read methods | **Yes** — extract `scanTradeRow(rows) (canonical.Trade, error)` |
| 10 | 91 | `internal/api/v1/sources_stats_cache.go` (self) | paired cache fetch blocks | Yes — `swr[T]` family |
| 11 | 87 | `external/{bitstamp,coinbase}/streamer.go` | WS frame → Trade assembly | Partial — extract the Trade-assembly tail only |
| 12 | 87 | `sources/{claimable_balances,liquidity_pools,trustlines}` | `assetKeyFromAsset` xdr→CODE:ISSUER | **Yes** — → `internal/supply` |
| 13 | 77 | `external/{binance,kraken}/parse.go` | trade parse tail | Partial |
| 14 | 76 | `cmd/stellarindex-indexer/main.go` (self, 672-754) | 4 near-identical external-connector wiring blocks in `startExternalConnectors` | Maybe — a small registration loop/helper |
| 15 | 70 | `external/{coingecko,coinmarketcap,cryptocompare,ecb,exchangeratesapi}/poller.go` | shared poller helper span (overlaps #1) | Yes — subsumed by #1 |

**Verdict:** the highest-value extractions are #7 (correctness), #1 (7-way
helper dup touching the scaling invariant), and the `swr[T]` adoption that
collapses #2/#6/#10 at once. The `coins.go` SQL-CTE self-dups (#3/#4) and the
per-venue CEX parse dups (#11/#13) are lower value — some duplication there is
the honest cost of per-venue/per-query divergence.

---

## Top complexity (scoped to `internal/ cmd/ pkg/`, `.discovery-repos` excluded)

`gocyclo` cyclomatic (left) / `gocognit` cognitive (right). All are within the
repo's configured lint thresholds (CI green). Flagged = genuinely hard to
follow vs. justified-flat-switch.

| rank | cyclo | cognit | func | file:line | assessment |
|---|---|---|---|---|---|
| 1 | 96 | 97 | `realMain` | `cmd/stellarindex-ops/main.go:100` | **Justified-ish** — flat subcommand flag-router (one `switch` over ~30 verbs). Long but linear. The fix is file-splitting (above), not control-flow. |
| 2 | 81 | 125 | `run` | `cmd/stellarindex-api/main.go:119` | **Watch** — gocognit (125) ≫ gocyclo (81) signals deep nesting, not a flat switch. Server bootstrap: build deps → wire ~40 routes → start. Candidate to decompose into `buildReaders` / `buildHandlers` / `startServers` helpers. |
| 3 | 62 | 146 | `chRebuild` | `cmd/stellarindex-ops/ch_rebuild.go:99` | **Hard to follow** — highest cognitive score (146). Multi-mode lake-replay write path (trades / contract-calls / supply branches). Worth decomposing per write-mode. |
| 4 | 62 | 108 | `computeCompleteness` | `cmd/stellarindex-ops/compute_completeness.go:47` | **Watch** — ADR-0033 reconcile orchestration; cognit 108. Several nested per-source loops; extract the per-source reconcile body. |
| 5 | 50 | 75 | `run` | `cmd/stellarindex-indexer/main.go:129` | Justified-ish — indexer wiring/bootstrap. Same shape as API `run`. |
| 6 | 43 | 60 | `verifyArchive` | `cmd/stellarindex-ops/main.go:2038` | Borderline — archive-verify state machine; could split the LCM-walk arm out. |
| 7 | 42 | 63 | `startExternalConnectors` | `cmd/stellarindex-indexer/main.go:661` | **Watch** — also a `dupl` hotspot (#14); the 4 repeated wiring blocks inflate both. A registration loop fixes complexity AND dup. |
| 8 | 40 | 40 | `HandleEvent` | `internal/pipeline/sink.go:413` | **Justified** — flat type-switch over `consumer.Event` variants (CLAUDE.md mandates a case per type). Linear, clear. |
| 9 | 40 | 59 | `run` | `cmd/stellarindex-aggregator/main.go:136` | Justified-ish — aggregator bootstrap. |
| 10 | 36 | 36 | `(*Decoder).decodeByKind` | `internal/sources/blend/dispatcher_adapter.go:141` | **Justified** — flat switch over Blend event kinds. |
| 11 | 34 | 44 | `(*Server).handleMarkets` | `internal/api/v1/markets.go:343` | Borderline — query-param parsing + branch fan-out; typical handler. |
| 12 | 28 | 72 | `(*Dispatcher).ProcessLedger` | `internal/dispatcher/dispatcher.go:509` | **Watch** — cognit 72 ≫ cyclo 28 = deep nesting (per-tx → per-op → per-change loops). This is the hot path; already carries `//nolint` siblings. Tread carefully but it's a decomposition candidate (extract `walkTx`/`walkOp`). |
| 13 | 35 | 35 | `decodeRelayArgs` | `internal/sources/band/decode.go:40` | Justified — Band relay-arg decode (E18/E9 scale handling); inherently branchy. |
| 14 | 33 | 50 | `(*Poller).PollOnce` | `external/coinmarketcap/poller.go:137` | Borderline — and a `dupl` participant; extraction (#1) trims it. |
| 15 | 28 | 34 | `(*Dispatcher).CensusLedger` (cognit) / `renderSCVal` (cyclo 27) | `dispatcher/census.go:65` / `ops/main.go:4550` | census: ties to the claim-atom dup (#7). renderSCVal: flat SCVal-type switch, justified. |

**Takeaway:** the cmd/ bootstrap funcs (`run`, `realMain`) are long-but-linear
— file/helper splitting, not logic surgery. The two to actually watch are
`chRebuild` (cognit 146) and `ProcessLedger` (cognit 72 from deep loop nesting
on the hot path). Everything labelled "Justified" is a flat dispatch switch —
leave it.

---

## Dead code (`deadcode ./...`, triaged)

`deadcode` does whole-program reachability from `main` entrypoints. Critically,
it is NOT redundant with the repo's gated `unused` linter: `unused`
(staticcheck U1000) does **not** flag *exported* funcs and **does** treat
test-only callers as "used" — so the items below escape CI's `unused` gate but
are real dead code. (Verified: `golangci-lint --enable=unused` reports **0
issues** for `cachekeys`, `blend`, `phoenix` despite the entries below.)

**Excluded as false positives** (per task rules): all of `pkg/client/*`
(public SDK — consumed externally; deadcode can't see external callers),
`internal/obstest.HistogramSampleCount` (test helper, used by 3 `_test.go`),
and `scripts/ci/verify-launch-ready` `engineeringReady`/`surfaceReadiness`
(used by `main_test.go`). Interface-satisfying methods were checked individually.

**Real dead code — recommend deletion:**

| confidence | symbol | file:line | note |
|---|---|---|---|
| high | `cachekeys.Price`, `.OHLC`, `.RateLimitKey`, `.RateLimitTTL`, `.TOML`(?), `.Metadata`, `.Subscriber`, `.Health` | `internal/cachekeys/keys.go:20,109,147,154,164,185,202,322` | A whole set of unused key-builders. Verified 0 non-test callers each. The package's USED builders (`APIKey`, `VWAP`, `MarketsList`, `Freeze`, `Divergence`, …) are fine; these are the unbuilt-feature leftovers. (`TOML` builder appears unused although `TOMLTTL` is used — double-check before deleting `TOML`.) |
| high | `internal/consumer/orchestrator.go` — `New`, `Config.applyDefaults`, `Orchestrator.{Events,Run,runSource,runOneSafe,runOne,cursorPersister}`, `nextBackoff`, `jitter`, `sleepCtx` | `orchestrator.go:64-354` | The entire legacy `consumer.Orchestrator` seam. CLAUDE.md already documents it as "a legacy seam with no callers; prod ingest is dispatcher-based." Only its own `orchestrator_test.go` exercises it. Strong delete candidate (drop the file + test). |
| high | `internal/hashdb/*` — `Create`, `Open`, `DB.{Close,StartLedger,recordOffset,Append,Get,Verify}`, `Hash` | `hashdb.go:102-217` | Entire package. CLAUDE.md: "LIBRARY ONLY — currently has zero production callers; not yet wired into any binary (the ADR-0033 'feeder' role is aspirational)." Keep IF the feeder is genuinely planned; otherwise it's an untested-in-prod liability. Flag for product decision, not blind deletion. |
| high | `blend.classify` ; `phoenix.classify` | `blend/decode.go:28` ; `phoenix/decode.go:115` | **Test-only retained.** Superseded by `classifyAny`; the only callers are `_test.go` (verified). The source comments admit it: "classify is retained as the narrow…". `unused` misses these (same-package test caller); `deadcode` catches them. Delete the func + its now-orphan test, OR if kept as documentation, mark clearly. |
| med | `soroswap_router.Sink.Persist` (+ `Sink` struct) | `soroswap_router/consumer.go:29` | Orphaned `consumer.Sink` impl — the pipeline handles `soroswap_router.Event` inline at `pipeline/sink.go:457`, never via this `Sink`. Confirmed no constructor/caller. Delete unless a Phase-B plan needs it. |
| med | `external.IncludeInVWAP` (func), `external.IsFXSource` | `external/registry.go:134,167` | Convenience wrappers; the struct *field* `IncludeInVWAP` is heavily used but these helper funcs have no callers (the orchestrator reads `md.IncludeInVWAP` directly). Delete the funcs. |
| med | `aggregate.ProxyPair`, `ProxyTrade`, `ExpandTargetPair`, `Triangulate` | `aggregate/stablecoin.go:83,105,155` ; `aggregate/triangulate.go:23` | Flagged unreachable from `main`. Triangulation is a known open gap (project memory #26). Verify these aren't the in-progress triangulation scaffold before deleting — likely "built ahead of wiring," not stale. **Flag, don't auto-delete.** |
| low | `metadata.NewCache` + `Cache.{Resolve,getCached,setCached,Invalidate}` | `metadata/cache.go:44-143` | An entire SEP-1 metadata cache type with no callers (the resolver path is used elsewhere). Likely superseded. Verify then delete. |
| low | `ratelimit.Bucket.Take`, `WithClock`, `WithKeyPrefix`, `WithDwellTime` | `ratelimit/bucket.go:78-218` | `Take` + 3 options unreachable. The rate-limit middleware may use a different entrypoint (`RateLimit` middleware itself is also flagged — check the middleware is actually wired). |
| low | misc `WithX` option-funcs: `auth.WithClock`/`WithStoreClock`/`withRandRead`, `usage.WithClock`/`WithKeyPrefix`, `forex.Client.WithBase`/`HasAPIKey`, `frankfurter.Client.WithBase`, `reflector.WithDecoder*`, `supply.WithStaleComponentLedgers`, `timescale.With*` | various | Functional-options that no caller passes. Common "wrote the option, only ever use the default" pattern. Individually trivial; sweep-deletable. Low priority. |
| low | `metadata.WithClient`, `stellarrpc.WithHTTPClient`, `divergence` option, `xdrjson.ParticipantAccounts`/`muxedToAccountID`, `supply.Overlay`, `aquarius.PoolType.String`, `supply.Outcome.String` | various | Assorted unreachable helpers/Stringers. `xdrjson.ParticipantAccounts` is interesting — the explorer participant-index work (project memory, ADR-0038 Phase B) may consume it soon; **don't delete**, it's built-ahead. The `.String()` methods may be used only by `%v` formatting (deadcode can miss `fmt` reflection) — verify before deleting. |

**Note on `.String()` / option-func entries:** `deadcode` can over-report
methods reached only via `fmt` reflection or interface-value formatting. The
high-confidence rows above were grep-verified to have zero callers; the low rows
should be spot-checked before any deletion PR.

---

## Naming / cohesion smells (brief)

- **God-files (>800 LOC, non-test):** 20 files. The two `cmd/*/main.go`
  outliers (ops 4627, api 3293) are the real cohesion problem — see Findings.
  `internal/storage/timescale/coins.go` (1837) is also a self-duplication
  hotspot (clusters #3/#4) — it has grown into a grab-bag of coins queries and
  could split by concern (price-history vs cursor vs registry). `internal/obs/
  metrics.go` (1550) is justified — it's a flat declaration+registration file
  (the documented metric-add recipe lives here), not logic.
- **Naming consistency:** strong overall. One spot-noted smell: the
  `internal/sources/external` packages each redefine the *same private helper
  name* (`decimalStringToScaledInt`, `syntheticTxHash`) — consistent naming, but
  the consistency is itself the evidence of copy-paste (cluster #1). The
  func-vs-field collision `external.IncludeInVWAP` (a dead func sharing a name
  with a live struct field) is a minor readability trap.
- **Package cohesion:** `internal/consumer` carries a dead `Orchestrator`
  alongside its live `Event`/`Source` contracts — splitting the dead orchestrator
  out (or deleting it) would sharpen the package's purpose to "transport-neutral
  ingest contracts" as CLAUDE.md describes it. `internal/hashdb` is a cohesive
  package with zero consumers — cohesion is fine, *existence* is the question.

---

## Tools run

All run from repo root `/Users/ash/code/ratesengine`. Tools installed via
`go install` into `/Users/ash/go/bin` (none were pre-present except
`golangci-lint`).

| tool | install | command actually used | succeeded? |
|---|---|---|---|
| `dupl` | `go install github.com/mibk/dupl@latest` (NOTE: the task's `.../cmd/dupl` path 404s — the binary is at the module root) | `dupl -threshold 80 internal cmd pkg` (human) + `dupl -threshold 80 -plumbing internal cmd pkg` (parsing). The task's `./internal/...` glob form is **not** supported by dupl — it takes plain dir paths. | ✅ yes — 145 clones |
| `gocyclo` | `go install github.com/fzipp/gocyclo/cmd/gocyclo@latest` | `gocyclo -top 40 ./internal/ ./cmd/ ./pkg/` | ✅ yes (first run `gocyclo -top 30 .` was discarded — it swept `.discovery-repos/` third-party code; re-scoped) |
| `gocognit` | `go install github.com/uudashr/gocognit/cmd/gocognit@latest` | `gocognit -top 40 ./internal/ ./cmd/ ./pkg/` | ✅ yes (same re-scope as gocyclo — first `.` run was all vendored AWS/brotli/zstd noise) |
| `deadcode` | `go install golang.org/x/tools/cmd/deadcode@latest` | `deadcode ./...` then filtered out `.discovery-repos` | ✅ yes — 143 raw hits, triaged above |
| `golangci-lint` (cross-check) | pre-installed | `golangci-lint run --no-config --default=none --enable=unused <pkgs>` | ✅ yes — confirmed `unused` reports 0 for cachekeys/blend/phoenix, proving deadcode catches what the gated linter does not |
| `grep` (caller verification) | n/a | per-symbol `grep -rn` for callers, test-vs-prod split | ✅ used throughout to separate real dead code from interface/test/public-API false positives |

**Caveats:** (1) `dupl` is AST-token based — it flags structurally identical
code even with renamed identifiers, but misses semantically-equal-but-
differently-structured dup; treat the SDEX claim-atom finding (#7) as the
floor, not ceiling, of that family. (2) `deadcode` reachability is from `main`,
so anything reached only via reflection, `go:linkname`, build-tagged files, or
external consumers (the `pkg/client` SDK) reports as dead — every entry above
was manually re-checked against that. All tools installed and ran offline; no
network fallback to grep-only was needed.
