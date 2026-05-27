# W27 — Soroban events landing zone (ADR-0029)

## Scope

The entire raw-events architecture introduced in rc.78 + rc.79
+ rc.80 + rc.81:

- `internal/sources/sorobanevents/` package: `Row`, `Capture`,
  `Reconstruct`, `AsyncSink`, `RawEventSink` interface
- `internal/dispatcher/dispatcher.go`: `SetRawEventSink` +
  `dispatchOne` raw-event hook
- migration 0041: `soroban_events` hypertable
- `internal/storage/timescale/soroban_events.go`:
  `InsertSorobanEventsBatch`, `StreamSorobanEvents`,
  `CountSorobanEventsInRange`
- `cmd/ratesengine-ops` subcommands that use this: `backfill
  -source soroban-events`, `scan-soroban-events`,
  the six per-source backfills (W29)
- the 2026-05-26 fill walk incident (drop incoherence) and
  rc.80 fix
- ADR-0029 document text vs implementation

## Inputs

- ADR-0029 (`docs/adr/0029-soroban-events-landing-zone.md`)
- migration 0041 + the rc.78 → rc.79 ON CONFLICT shape match fix
  (commit `6347b54f`)
- `internal/sources/sorobanevents/` (entire package)
- `internal/dispatcher/dispatcher.go` around the
  `SetRawEventSink` + `dispatchOne` raw-event hook
- `internal/storage/timescale/soroban_events.go`
- the 2026-05-26 fill walk journal logs on r1
- memory `project_soroban_events_landing_zone`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W27.1 | ADR-0029 invariants are implemented exactly as documented | per-line ADR vs code |
| W27.2 | Migration 0041 PK = `(ledger_close_time, ledger, tx_hash, op_index, event_index)` (TS103-compliant) | migration up.sql + writer ON CONFLICT |
| W27.3 | Writer ON CONFLICT column list byte-matches PK | `internal/storage/timescale/soroban_events.go:104` |
| W27.4 | `Capture` is total (handles every events.Event shape without panicking) | unit tests + malformed inputs |
| W27.5 | `Reconstruct(Capture(ev))` is loss-free for the fields decoders read | `reconstruct_test.go` |
| W27.6 | `AsyncSink.PushEvent` blocks under back-pressure (W28 detail; here verify the contract) | `dispatcher_adapter_test.go` |
| W27.7 | `Dispatcher.SetRawEventSink` is called from both `cmd/ratesengine-indexer/main.go` (live) and `cmd/ratesengine-ops/backfill.go` (fill walk) | grep |
| W27.8 | Raw-event hook fires BEFORE the per-source decoder pass (so rejected events still capture) | `dispatcher.dispatchOne` |
| W27.9 | `topic_0_sym` populated when topic[0] is Symbol or String; NULL otherwise | `Capture` logic + tests |
| W27.10 | `op_args_xdr` is the ScVec-marshalled bytes of the originating InvokeContract args; NULL when no op args | `Capture` + `scval.EncodeArgsAsScVec` |
| W27.11 | `Store.StreamSorobanEvents` filters push-down (contract_id + topic_0_sym SQL filter); returns rows in deterministic order | query inspection |
| W27.12 | Hypertable compression policy + retention policy defined (or explicit decision to defer) | timescale jobs |
| W27.13 | NEW migration audit: 0041 has matching down.sql that drops the hypertable cleanly | `migrations/0041_create_soroban_events.down.sql` |
| W27.14 | The 2026-05-26 fill walk's drop incident is post-mortemed: ADR-0029 now describes back-pressure semantics (not original buffer-full-drop) | ADR §positive/negative + rc.80 commits |
| W27.15 | r1 has migration 0041 applied | r1 probe R1-P22 |
| W27.16 | `soroban_events` table is populating live (post-rc.79) | r1 probe R1-P21 |
| W27.17 | The re-walk under rc.80 back-pressure produces ZERO drop warnings | r1 probe R1-P14 |
| W27.18 | `scan-soroban-events` operator subcommand uses MinIO directly (not the hypertable) per its code path | `cmd/ratesengine-ops/main.go::scanSorobanEvents` |
| W27.19 | The catch-all hook applies regardless of contract_id allowlist (no IsX gating) | `dispatcher.dispatchOne` |
| W27.20 | NEW: index coverage on `soroban_events` supports the StreamSorobanEvents predicate (`ledger BETWEEN`, contract_id IN, topic_0_sym IN) | migration 0041 indexes |

## Evidence expectations

- W27.1: ADR-0029 text excerpt + code reference per invariant.
- W27.2-3: migration up.sql line + writer.go line, side-by-side
  in evidence/log.md.
- W27.5: test output excerpt.
- W27.6: regression test output excerpt.
- W27.13-14: ADR-0029 line ranges showing the rewrite.
- W27.15-17: R1 probe transcripts.

## Closure criteria

Every check terminal. Findings on:
- any ADR-0029 invariant not enforced
- any cursor-coherence weakening
- any race between RawEventSink and per-source decoder pass
- any compression/retention gap that lets the hypertable grow
  unbounded (storage planning gap)
