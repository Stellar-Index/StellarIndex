# `internal/sources/sorobanevents`

Catch-all Soroban-event landing-zone capture (ADR-0029).

## Scope

Every Soroban contract event that flows through the dispatcher is
also written to the `soroban_events` hypertable (migration 0041)
as a raw row — contract id + topics + body + op args, all as raw
XDR (with a convenience-decoded `topic_0_sym` column for the
common Symbol/String case).

This is **additive**, not a replacement. Per-source decoders
(trades, blend_auctions, cctp_events, rozo_events,
sep41_supply_events, ...) continue to write their domain-specific
tables from the live event stream. The `soroban_events` table
unblocks **future** per-source decoders: when a new decoder ships
that needs to backfill, it can do so via

```sql
INSERT INTO <protocol_table>
SELECT ...
  FROM soroban_events
 WHERE contract_id IN (...)
   AND topic_0_sym IN (...)
```

— milliseconds-to-minutes instead of hours of MinIO re-walking.

## Wiring

This package does NOT register with the dispatcher's
`Decoder` / `OpDecoder` / `ContractCallDecoder` chains. Instead it
plugs into the new `dispatcher.RawEventSink` hook (ADR-0029), which
is orthogonal to per-source decoder routing — every Soroban contract
event the dispatcher sees fires the hook, regardless of whether a
per-source decoder claimed it.

```
ledger → dispatcher.ProcessLedger
        ├── per-source decoders (trades, oracles, bridge, etc.)
        └── RawEventSink.PushEvent  ← this package
```

The standard wiring:

```go
sink := sorobanevents.NewAsyncSink(store, sorobanevents.AsyncSinkOptions{
    BufferSize:    4096,
    BatchSize:     1000,
    FlushInterval: time.Second,
})
sink.Start()
defer sink.Stop()
disp.SetRawEventSink(sink)
```

## Files

- `events.go` — `Row` shape (1:1 with migration 0041) +
  `Capture(events.Event) (Row, error)` projection.
- `dispatcher_adapter.go` — `RawEventSink` interface +
  `AsyncSink` (buffered drain + batched insert).
- `events_test.go` — unit tests for `Capture` covering the
  Symbol/String/Address topic[0] cases + op_args round-trip.

## Backfill mode

`stellaratlas-ops backfill -source soroban-events -from N -to M
-parallel K` populates `soroban_events` for a historical range
without per-source decoding (the catch-all sees every event no
matter what). Special-cased in the backfill subcommand because
soroban-events is not an `external.Registry` source — it's a
pseudo-source that's always present at the dispatcher's
RawEventSink seam.

## See also

- [ADR-0029](../../../docs/adr/0029-soroban-events-landing-zone.md)
  — the architectural rationale.
- [Migration 0041](../../../migrations/0041_create_soroban_events.up.sql)
  — the table schema and index strategy.
