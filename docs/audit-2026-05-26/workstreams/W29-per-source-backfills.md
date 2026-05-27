# W29 — Per-source backfill subcommands

## Scope

The six subcommands that re-feed soroban_events rows through
live Go decoders to populate per-source hypertables:

- `cmd/ratesengine-ops/cctp_backfill.go` — cctp-backfill
- `cmd/ratesengine-ops/rozo_backfill.go` — rozo-backfill
- `cmd/ratesengine-ops/soroswap_skim_backfill.go` —
  soroswap-skim-backfill
- `cmd/ratesengine-ops/comet_liquidity_backfill.go` —
  comet-liquidity-backfill
- `cmd/ratesengine-ops/phoenix_backfill.go` — phoenix-backfill
- `cmd/ratesengine-ops/blend_backfill.go` — blend-backfill

Plus the supporting machinery:

- `internal/sources/sorobanevents.Reconstruct`
- `internal/scval.DecodeScVecToArgs`
- `internal/storage/timescale.Store.StreamSorobanEvents`

## Inputs

- six `*_backfill.go` files in `cmd/ratesengine-ops/`
- `internal/sources/sorobanevents/reconstruct.go` +
  `reconstruct_test.go`
- `internal/scval/scval.go` `DecodeScVecToArgs` + tests
- per-source live `Decoder.Decode()` paths in
  `internal/sources/{cctp,rozo,soroswap,comet,phoenix,blend}/`
- per-source storage writers in `internal/storage/timescale/`

## Per-subcommand checks

Each of the six follows the same loop:

| # | Check | Method |
| --- | --- | --- |
| W29.X.1 | `-from`, `-to`, `-config` required; `-dry-run` available | flag parsing in source file |
| W29.X.2 | StreamSorobanEvents called with the EXACT contract+topic filter that the source's live `Matches`/`classify()` covers | side-by-side comparison |
| W29.X.3 | Reconstruct → Decode → Insert path mirrors live `dispatchOne` → `Decoder.Decode` → `Sink.persist...` | code trace |
| W29.X.4 | Idempotency: re-running produces identical row counts (per-source `ON CONFLICT DO NOTHING`) | integration test |
| W29.X.5 | Dry-run skips inserts but still surfaces decode errors | flag handling |
| W29.X.6 | Empty input range produces zero-error empty summary | unit test |
| W29.X.7 | Decode error in one row does not abort the run | error handling |
| W29.X.8 | Insert error in one row does not abort the run | error handling |
| W29.X.9 | Final stderr summary line reports rows_scanned, events_emitted, decode_errors, insert_errors | output inspection |
| W29.X.10 | Subcommand registered in `cmd/ratesengine-ops/main.go` switch | grep |

Specific deltas per subcommand:

| # | Check | Subcommand-specific |
| --- | --- | --- |
| W29.SOROSWAP.1 | Two-tuple topic shape: filters by `topic_0_sym='SoroswapPair'` AND callback-side `bytes.Equal(row.Topic1XDR, skimTopic1XDR)` | `soroswap_skim_backfill.go` |
| W29.COMET.1 | Filters by `topic_0_sym='POOL'` AND callback `topic1IsLiquidity` (4 of 5 kinds; swap excluded since already in trades) | `comet_liquidity_backfill.go` |
| W29.PHOENIX.1 | Uses per-action correlation buffer in `phoenix.Decoder` — feeding in order keeps buffer churn quiet | `phoenix_backfill.go` |
| W29.PHOENIX.2 | Reports `EvictedOrphans()` in final summary | grep |
| W29.PHOENIX.3 | Swap (5-tuple action) is NOT in scope (already in trades from live) | comment + filter list |
| W29.BLEND.1 | 20 topic kinds → 3 target tables (PositionEvent → blend_positions, EmissionEvent → blend_emissions, AdminEvent → blend_admin) | type-switch |
| W29.BLEND.2 | Auction events excluded (already in blend_auctions from live) | filter list |

## Reconstruct + DecodeScVecToArgs checks

| # | Check | Method |
| --- | --- | --- |
| W29.R.1 | Reconstruct round-trips Capture for the fields decoders read | `reconstruct_test.go::TestReconstruct_RoundTripsCapture` |
| W29.R.2 | Reconstruct handles a row with no OpArgs gracefully | `TestReconstruct_NoOpArgs` |
| W29.R.3 | Reconstruct refuses empty contract_id / topic / non-32-byte tx_hash | input validation |
| W29.R.4 | DecodeScVecToArgs is the byte-perfect inverse of EncodeArgsAsScVec | unit test |
| W29.R.5 | DecodeScVecToArgs returns (nil, nil) on empty input | nil-input case |
| W29.R.6 | DecodeScVecToArgs errors on bytes that aren't a ScVal::Vec | malformed input |

## StreamSorobanEvents checks

| # | Check | Method |
| --- | --- | --- |
| W29.S.1 | Filters push down to SQL (no client-side filtering) | query inspection |
| W29.S.2 | Empty contractIDs slice means "no filter" (no broken IN clause) | edge-case |
| W29.S.3 | Empty topic0Syms slice means "no filter" | edge-case |
| W29.S.4 | Rows returned in `(ledger_close_time, ledger, tx_hash, op_index)` order — required for correlation-buffer correctness | ORDER BY clause |
| W29.S.5 | Callback returning error aborts iteration (no spurious work) | callback semantics |
| W29.S.6 | ctx.Cancel during iteration cancels the query | driver behaviour |

## Live-runtime check

| # | Check | Method |
| --- | --- | --- |
| W29.LIVE.1 | Each subcommand exists on r1 under `/usr/local/bin/ratesengine-ops` and shows the right version | r1 probe |
| W29.LIVE.2 | At least one subcommand has been run on r1 against a non-trivial range with documented output | r1 ops log |

## Closure criteria

Every check terminal per subcommand (10 × 6 = 60 + 7 + 6 + 6 + 2
= 81 checks). Findings on:

- any filter mismatch between subcommand and live source
- any non-idempotent behaviour
- any decode-error suppression that should escalate
- any reconstruct edge-case not covered by tests
- any per-source orphan-eviction discrepancy
