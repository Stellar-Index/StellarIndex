# W06 — Ingest transport, dispatcher, persistence pipeline

## Scope

The data path from raw LedgerCloseMeta to a hypertable row.
`internal/ledgerstream/`, `internal/dispatcher/`,
`internal/pipeline/`, `internal/hashdb/`,
`internal/archivecompleteness/`, the live indexer wiring at
`cmd/ratesengine-indexer/main.go`.

NEW since baseline:
- `Dispatcher.SetRawEventSink` hook (ADR-0029)
- back-pressure on sorobanevents.AsyncSink (W28)
- ledgerstream.TolerateTrailingMissing (W28)
- ctx-cancel early-stop watchdogs

## Inputs

- the four package directories above
- `cmd/ratesengine-indexer/main.go`
- `docs/architecture/ingest-pipeline.md`
- `scripts/ci/lint-imports.sh` rules

## Checks

| # | Check | Method |
| --- | --- | --- |
| W06.1 | ledgerstream: live and archive paths share the same SDK iterator (no Horizon, no stellar-rpc in prod) | code + lint-imports |
| W06.2 | ledgerstream: live (unbounded) uses LiveRetryWait; bounded errors on missing files unless TolerateTrailingMissing | code |
| W06.3 | dispatcher: routing logic — contract-event vs ledger-entry-change vs op-decoder vs contract-call dispatch | code review |
| W06.4 | dispatcher: NEW raw-event hook (SetRawEventSink) fires BEFORE per-source decoders | dispatchOne code path |
| W06.5 | dispatcher: discovery hook fires BEFORE raw-event hook (so SEP-41-shape detection is independent) | dispatchOne |
| W06.6 | dispatcher: stats flush wiring | `internal/dispatcher/statsflush/` |
| W06.7 | pipeline: glue between dispatcher + sink | code |
| W06.8 | pipeline: sink type-switch handles every consumer.Event the decoder set emits | sink.go type-switch |
| W06.9 | pipeline: every new consumer.Event TYPE has a sink case (per memory `feedback_pipeline_sink_type_switch`) | grep |
| W06.10 | pipeline: ProcessLedger ctx-aware in callback (events channel send respects ctx.Done) | code |
| W06.11 | hashdb: ledger_seq → sha256(LCM) record stable across rebuilds | code + tests |
| W06.12 | archivecompleteness: tier A/B/C/D verifier flow (W34 details Tier A) | per-tier path |
| W06.13 | ADR-0001 (no Horizon) holds: grep horizon | grep |
| W06.14 | ADR-0002 (S3-compat storage) holds: filesystem datastore is tests-only | grep |
| W06.15 | stellar-rpc residue: only used by rpc-probe + fixture-capture | grep |
| W06.16 | NEW: ctx-cancel early-stop watchdog in `runBackfillChunk` (line ~343-356) | grep |
| W06.17 | NEW: live indexer's `<-rootCtx.Done(); rawEventSink.Stop()` goroutine | grep |
| W06.18 | NEW: ledgerstream.TolerateTrailingMissing wired into LedgerstreamConfig helper (single place) | grep |
| W06.19 | pipeline: malformed XDR doesn't panic the dispatcher | recover() in ProcessLedger |
| W06.20 | pipeline: sink write failure doesn't block the producer (back-pressure is the intended mechanism) | W28 cross-ref |

## Closure criteria

Every check terminal. Findings on any path where:
- producer outpaces durable writes (W28 mission)
- a new consumer.Event type lacks a sink case
- ledgerstream falls back to a path other than the documented
  Galexie → dispatcher → decoder shape
