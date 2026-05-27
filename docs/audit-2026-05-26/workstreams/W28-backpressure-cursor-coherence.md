# W28 — Back-pressure / ctx-shutdown semantics

## Scope

The cursor-coherence guarantee introduced in rc.80 (and the
trailing-edge tolerance from rc.81):

- `sorobanevents.AsyncSink` lifecycle (Start → Push → Stop)
- the `stopping` channel + Stop's no-close-of-ch invariant
- `PushEvent` blocking semantics under back-pressure
- ctx-cancel early-stop watchdog in
  `cmd/ratesengine-ops/backfill.go::runBackfillChunk` (lines
  around 343-357) and
  `cmd/ratesengine-indexer/main.go` (rootCtx watch around the
  rawEventSink defer)
- `ledgerstream.Config.TolerateTrailingMissing` + window
- the rc.78→rc.79→rc.80 fix sequence; rc.80→rc.81 fix sequence
- the 2026-05-26 fill walk drop incident (18.86M rows / ~0.43%)
- delivery caveat: SDK BufferedStorageBackend cancels its
  context on missing file, dropping pre-fetched ledgers

## Inputs

- `internal/sources/sorobanevents/dispatcher_adapter.go`
  (post-rc.80)
- `internal/sources/sorobanevents/dispatcher_adapter_test.go`
  (regression tests landed with the back-pressure fix)
- `internal/ledgerstream/ledgerstream.go` (TolerateTrailingMissing
  + parseTrailingMissingSeq + maybeTolerateTrailingMissing)
- `internal/ledgerstream/trailing_edge_internal_test.go` +
  `trailing_edge_stream_test.go`
- `cmd/ratesengine-ops/backfill.go::runBackfillChunk`
- `cmd/ratesengine-indexer/main.go` rawEventSink wiring
- memory `feedback_fd2_wrap_drain_on_exit`,
  `feedback_quiet_checksum_was_a_noop`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W28.1 | `AsyncSink.PushEvent` blocks (no `default` branch in the select) when channel is full and stopping is open | code inspection + test `TestAsyncSink_PushEventBacksPressure_BufferFull_NoDrops` |
| W28.2 | `AsyncSink.Stop()` closes `stopping` but NOT `ch` (to avoid send-on-closed panic) | code inspection + test `TestAsyncSink_StopDrainsPendingRows_NoChannelClose` |
| W28.3 | Stop releases blocked producers and counts them as dropped (shutdown-race only) | test `TestAsyncSink_StopReleasesBlockedProducers` |
| W28.4 | `runBackfillChunk` launches a ctx-cancel watcher goroutine that calls `rawSink.Stop()` on ctx.Done | `cmd/ratesengine-ops/backfill.go` around lines 343-356 |
| W28.5 | Live indexer launches `<-rootCtx.Done(); rawEventSink.Stop()` watcher | `cmd/ratesengine-indexer/main.go` around lines 314-323 |
| W28.6 | The backfill cursor (line 349) does NOT advance past a row that PushEvent has not enqueued (the back-pressure block ensures sequentialness) | code review + journal-log evidence on the re-walk |
| W28.7 | `ledgerstream.TolerateTrailingMissing` converts the SDK "ledger object containing sequence X is missing" error to a clean walk-complete when `to - X <= TrailingMissingWindow` | code in `maybeTolerateTrailingMissing` + tests |
| W28.8 | The regex `trailingMissingRE` matches every SDK wrap shape we observe in production (bare, streamTiered prefix, backfill chunk prefix) | `trailing_edge_internal_test.go` |
| W28.9 | Mid-range gaps (X far below `to`) still error regardless of the flag | `TestStream_TolerateTrailingMissing_MidRangeStillErrors` |
| W28.10 | Strict mode (`TolerateTrailingMissing=false`) preserves pre-2026-05-26 behaviour | `TestStream_TolerateTrailingMissing_DisabledStrictMode` |
| W28.11 | `LedgerstreamConfig` helper sets TolerateTrailingMissing=true universally (rc.81 commit `d3b4d492`) | `internal/pipeline/datastore.go` |
| W28.12 | verify-archive walker's lsCfg sets TolerateTrailingMissing=true | `cmd/ratesengine-ops/main.go::verifyArchiveLCMWalk` |
| W28.13 | wasm-history walker's lsCfg sets TolerateTrailingMissing=true | `cmd/ratesengine-ops/main.go::wasmHistory` |
| W28.14 | Delivery caveat doc'd on the Config field warns operators must clamp `-to` for 100% coverage | godoc text |
| W28.15 | The 2026-05-26 fill walk drop count (18.86M rows / 0.43%) is captured in CHANGELOG as a post-mortem | `CHANGELOG.md` rc.80 entry |
| W28.16 | rc.78 fd-2 wrap drain-on-exit fix (`6ef438c4`) still holds in current binary on r1 (per `feedback_fd2_wrap_drain_on_exit`) | r1 probe: run a short-lived `ratesengine-ops backfill -dry-run` and confirm output is not silently dropped |
| W28.17 | rc.77 SDK checksum-WARN flood fix (`b667028b` fd-2 wrap) still in place; logs are not flooded | r1 probe: tail journal during indexer startup |
| W28.18 | NEW: every place that uses `AsyncSink` correctly orders the producer goroutine's exit before calling Stop (no producer can race past close(stopping)) | code review of all callers |
| W28.19 | NEW: the AsyncSink design supports `parallel-N` per-chunk sinks (one per chunk, not shared) per `buildChunkDispatcher` | `backfill.go::buildChunkDispatcher` |
| W28.20 | NEW: the live indexer's single dispatcher uses a single rawEventSink (not per-source); confirm | `cmd/ratesengine-indexer/main.go` |

## Evidence expectations

- W28.1-3: test run output (race-detector enabled).
- W28.4-5: ctx-watcher goroutine line refs.
- W28.6: journal-log excerpt from r1 showing the re-walk's
  "soroban-events sink drained ... dropped=0" lines.
- W28.7-10: trailing-edge test outputs.
- W28.11-13: grep results showing the flag set in three
  places.
- W28.15: CHANGELOG line excerpt.
- W28.16-17: r1 probe transcripts.

## Closure criteria

Every check terminal. Findings on:

- any cursor advance path that bypasses back-pressure
- any AsyncSink-style design (in any other component) that
  doesn't have equivalent cursor-coherence guarantees (would be
  a `critical` finding per the rubric)
- delivery-caveat operator runbook gap (we should have a clear
  doc explaining "clamp -to for 100% coverage")
