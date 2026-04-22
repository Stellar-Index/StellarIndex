# withObsrvr — nebu

**Status:** 🧪 Design inspiration. **Do not import as a dependency yet.**
The contract is clean and worth studying; but pre-1.0, an external repo
per processor, and aimed at a multi-processor ecosystem that is larger
than our single-purpose needs.

**Repo:** <https://github.com/withObsrvr/nebu> (v0.6.7 at audit time)
**Verified against:** `pkg/processor/types.go`, `pkg/processor/origin.go`,
`pkg/processor/transform.go`, `pkg/processor/sink.go`,
`pkg/source/source.go`, `README.md` at clone time (2026-04-22).

## What nebu is

From its own README (lightly paraphrased): nebu is two things in one
repo:

1. A **stable Go contract** (`pkg/processor` + `pkg/source`) — the
   interfaces that anyone implementing a Stellar processor should
   satisfy. External processors live in their own Go modules and only
   depend on this contract.
2. A **CLI + reference processors** — `nebu install token-transfer`,
   `token-transfer --start-ledger N --end-ledger M` emits newline-
   delimited JSON on stdout.

For us, the first part is what matters.

## The processor contract (verified)

### Three types, each a focused interface

`pkg/processor/types.go:37-44`:

```go
type Type int
const (
    TypeOrigin Type = iota   // ledger → events
    TypeTransform            // events → events
    TypeSink                 // events → side effect
)

type Processor interface {
    Name() string
    Type() Type
}
```

### Origin — consumes `LedgerCloseMeta`, emits events

`pkg/processor/origin.go:52-60`:

```go
type Origin interface {
    Processor
    ProcessLedger(ctx context.Context, ledger xdr.LedgerCloseMeta)
}
```

### Transform — events in, events out

`pkg/processor/transform.go:58-68`:

```go
type Transform interface {
    Processor
    ProcessEvent(ctx context.Context, event proto.Message)
}
```

### Sink — terminal

`pkg/processor/sink.go:49-58`:

```go
type Sink interface {
    Processor
    WriteEvent(ctx context.Context, event proto.Message)
}
```

### Explicit error-handling contract

No method returns `error`. The design choice (from the doc comments,
verbatim): "A pipeline that runs for hours over millions of ledgers
must not die because one ledger had a malformed field. Per-ledger
errors should be reported via `ReportWarning` and the method should
return normally; … unrecoverable errors should be reported via
`ReportFatal` and the method should return immediately."

This is the right error model for long-running indexers. We should
adopt it.

### Typed events with protobuf

Events between processors are `proto.Message`. This is stricter than
cdp-pipeline-workflow's `interface{}`/`[]byte` approach — it forces
callers to type-assert and it allows a JSON schema to be emitted
automatically.

### `LedgerSource` abstraction

`pkg/source/source.go:12-22`:

```go
type LedgerSource interface {
    Stream(ctx context.Context, start, end uint32, out chan<- xdr.LedgerCloseMeta) error
    Close() error
}
```

Clean separation from processors. A source produces ledgers; processors
consume them. This is the same split `ingest.ApplyLedgerMetadata` makes
in the Stellar SDK.

## What nebu is *not*

- Not a full pipeline runner in-process. Each processor is a separate
  binary; nebu's CLI shells out and pipes NDJSON between them. Useful
  for Unix-style composition, not what we want for a single-process
  low-latency hot path.
- Not production-hardened yet (pre-1.0).
- Not focused on multi-sink fan-out within one process. Each run is
  `processor | jq` or `processor | duckdb`.

## Decision for CTX Rates

1. **Adopt the three-type split (Origin/Transform/Sink) + explicit
   warning/fatal error model** for our own internal processor
   interfaces. It's the right shape. We don't need to import nebu's
   packages to get the benefit — just mirror them.
2. **Consider using `proto.Message` events** if we want schema-strict
   pipelines. For Phase 2 we'll likely use Go structs plus interface
   assertions; protobuf comes later if we externalise processors.
3. **Use `stellar-extract` as the extraction engine** inside our Origin
   implementations (see [withobsrvr-stellar-extract.md](withobsrvr-stellar-extract.md)).
4. **Revisit importing nebu directly** once it hits 1.0 and if we want
   to publish our processors for reuse. Not Phase 1.

## Open items

- [ ] Check whether nebu's reference processors (`token-transfer`,
      `contract-events`, `contract-invocation`) already correctly
      extract Soroswap swap events / Reflector oracle reads — if so,
      they're potential drop-in Origins for us.
- [ ] Look at `nebu-processor-registry` to see their registry format
      (`description.yml`) — might be worth adopting for our own
      internal plugin system.
- [ ] Check whether the stellar SDK's `token_transfer.EventsProcessor`
      (mentioned in nebu's Origin docstring) is our shortest path to
      a full Soroswap/Aquarius/LP-trade event stream.

## References

- Repo: <https://github.com/withObsrvr/nebu>
- Website: <https://nebu.withobsrvr.com>
- Stability commitment: `docs/STABILITY.md` + `.api/` snapshots
  enforced in CI.
