---
title: D9 — Convention docs as checklists — findings + drop-in checklists
---

# D9 — Convention docs as checklists

## Findings on the existing recipes (CLAUDE.md "Common task recipes")
- **M0 — "Add a new on-chain Soroban DEX" misleads → a source that registers nowhere +
  emits nothing.** The recipe says "five-file convention" but omits **`dispatcher_adapter.go`
  (the production seam — the actual `dispatcher.Decoder`)** and lists **zero of the 6 wiring
  edits** (KnownSources, BuildDispatcher, sink.HandleEvent, sink.IsProjectedEvent,
  projector.buildSource, external.Registry). An agent who reads it writes decode logic and
  never creates the object the dispatcher calls → exactly the rebuild/silent-loss failure.
- **M1 — metric recipe wrong:** "register in `init()`" (real: `registerAppMetrics()`/`…Tail()`);
  the build-header claim that `docs-all` regenerates metrics is stale (`docs-metrics` is a
  no-op; the metrics ref is hand-edited, lint-guarded). **M1 — CEX recipe** omits the trap
  that external venues are gated by `cfg.<Venue>.Enabled` only and are **NOT** in `KnownSources`.
- **M2 — supply-observer** layout minor (no `decode.go` for simple observers).
- **Gaps:** no "add an API endpoint" recipe; no "add a migration" recipe (only `migrations/README.md`).
- **Accurate (keep):** OpenAPI-change, cut/deploy-release, price-divergence, metric-alerting recipes.

## Recommendation
Move the checklists into **`docs/contributing/`** (one file per recipe), each co-located with
its CI guard; **CLAUDE.md keeps only the invariants (#6/#7/#8) + a one-line pointer per recipe**
(replace the prose bodies with links so they can't rot below the fold). Immediate fixes
regardless of location: correct the "five-file" line to name `dispatcher_adapter.go`+invariant #7,
and the `init()`/`docs-all` metric claims — those are the 3 active mislead vectors.

## The drop-in checklists (the D9 artifact — condensed to the wiring-critical steps)

### A — Add an on-chain source (ref `internal/sources/soroswap/`)
Package: `README.md` · `events.go` (`classify()` + `consumer.Event` wrappers, reuse
`canonical.Trade/Amount`) · `decode.go` (pure, via `internal/scval`, decode-by-NAME) ·
**`dispatcher_adapter.go`** (impl `dispatcher.Decoder`; **`Matches()` gates on contract
identity, not topic**) · `*_test.go` + `test/fixtures/<name>/`.
**Wire (miss one = silent no-op):** ①`config/validate.go` KnownSources ②`pipeline/dispatcher.go`
BuildDispatcher case ③`pipeline/sink.go` HandleEvent write ④`pipeline/sink.go` IsProjectedEvent
(if projected) ⑤`projector/registry.go` buildSource ⑥`external/registry.go` Metadata
(BackfillSafe:false). +migration +Store writer. **Guard:** `TestIsProjectedEvent_TableDriven`.
**Done:** fixture tests green, verify.sh green, rows appear; catch-up = `projector-replay`.

### B — Add a CEX/FX connector (ref `internal/sources/external/binance/`)
`events.go`/`parse.go` (**set `Trade.Decimals`** — CEX 10^8, FX 10^6)/`streamer.go`(`external.Streamer`)/
`backfill.go`/`pairs.go`. **Wire:** ①`external/registry.go` Metadata(`ClassExchange`,IncludeInVWAP:true)
②`config.go` `<Venue>{Enabled}` ③`indexer/main.go` buildExternal ④`ops/main.go` parallel block.
**NOT in KnownSources/enabled_sources.** Reuse `external.Runner` (reconnect/backoff/dust) — don't
hand-roll. **Done:** poller metric fires, `/v1/sources` classifies it.

### C — Add an API endpoint
①`openapi/…v1.yaml` path ②`api/v1/<area>.go` handler (reuse `envelope.go`, cache wrappers —
byte-identical prewarm args) ③`server.go` mountRoutes `HandleFunc` ④handler test ⑤regen ALL
THREE: `make docs-api docs-postman web-generate-api` ⑥CHANGELOG + version bump. **Guard:**
lint-docs route↔spec (but it greps `HandleFunc(` only — CS-052).

### D — Add a Prometheus metric
①declare `*Vec` in `obs/metrics.go` (paired `…Total{outcome}`+`…DurationSeconds{outcome}`,
copy `DivergenceRefresh*` — no helper) ②register in **`registerAppMetrics()`/`…Tail()`**
(NOT init()) ③pre-seed zero series if alerted ④wire ⑤document in `docs/reference/metrics/README.md`
(hand-edited) ⑥if alerting: rule in BOTH `deploy/monitoring/rules/` + `configs/prometheus/rules.r1/`
+ runbook ⑦test w/ `obstest.HistogramSampleCount`. **Guard:** lint-metric-refs + monitoring-rules job.

### E — Add a migration (ref `migrations/README.md`)
①`NNNN_<desc>.{up,down}.sql` dense-sequential ②amounts NUMERIC (never bigint), asset-ids
canonical text, timestamptz UTC ③CAGG needs a refresh policy in the same file (a CAGG w/o
refresh = silent bug; don't add `drop_after` to trades/oracle) ④event-hypertable PK leads with
`ledger_close_time` ⑤README "Current migrations" row ⑥apply as `stellarindex` role, never postgres
superuser. **Done:** up then down both succeed locally.

### F — Add a supply observer (ref `internal/sources/sac_balances/`, `supply-pipeline.md`)
`doc.go`(not README)+`events.go`+`dispatcher_adapter.go`. Pick hook: `LedgerEntryChangeDecoder`/
`OpDecoder`/`Decoder`. Register in `pipeline/dispatcher.go` `RegisterSupplyEntryDecoders`/
`RegisterSupplyEventDecoders`; `sink.go` HandleEvent write (NOT projected); migration + reader;
`[supply.*]` config; wire in indexer main. **Guard:** integration test if NUMERIC arithmetic.
