# Upstream bug report — draft (not yet filed)

> Status: **draft**. Characterized from our own workaround
> (`internal/ledgerstream`, commit `a375e0ad`) for filing against
> `github.com/stellar/go-stellar-sdk` by the operator. Not filed via
> `gh` from this repo — wrong org.

---

## `ingest.ApplyLedgerMetadata` rejects `ledgerbackend.SingleLedgerRange`, the exact range shape the package exports for that purpose

### Versions

- `github.com/stellar/go-stellar-sdk v0.6.0` (module SHA
  `dd844ab32ac8bef7984c76ad1e59c2209a4aacc5`, tag commit) — the
  version we have pinned and reproduced this against.
- Also present, unchanged, in `v0.5.0` (`475bbd9a...`) — this looks
  like a long-standing bug, not a regression introduced in `v0.6.0`.
- Go 1.25.11 / darwin-arm64, but this is pure control flow with no
  platform dependency.

### Summary

`ingest.ApplyLedgerMetadata` unconditionally rejects any bounded
range whose `To() == From()` — i.e. a range describing exactly one
ledger — even though `ledgerbackend` exports `SingleLedgerRange(seq)`
as the documented, first-class constructor for precisely that shape.
Any caller that does the obviously-correct thing —
`ApplyLedgerMetadata(ledgerbackend.SingleLedgerRange(seq), ...)` —
gets an immediate hard error instead of one ledger's `LedgerCloseMeta`.

### Root cause

`ingest/producer.go`, inside `ApplyLedgerMetadata`:

```go
// ingest/producer.go:119-121 (v0.6.0)
if ledgerRange.Bounded() && ledgerRange.To() <= ledgerRange.From() {
    return fmt.Errorf("invalid end value for bounded range, must be greater than start")
}
```

This uses `<=` (rejects `To() == From()`) where it should use `<`
(reject only `To() < From()`, the genuinely inverted/malformed case).

Meanwhile `ingest/ledgerbackend/range.go` defines:

```go
// ingest/ledgerbackend/range.go:71-74 (v0.6.0)
// SingleLedgerRange constructs a bounded range containing a single ledger.
func SingleLedgerRange(ledger uint32) Range {
    return Range{from: ledger, to: ledger, bounded: true}
}
```

`SingleLedgerRange(ledger)` produces exactly the `From() == To()`
shape that `producer.go:119` rejects. The package ships a
constructor for a range its own primary consumption function refuses
to process — the two haven't been kept in sync.

### Minimal repro

Self-contained; only depends on the SDK's own public API
(`datastore.NewFilesystemDataStoreWithPath` + `compressxdr`), no
network access, no mocks:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func main() {
	ctx := context.Background()
	dir := "/tmp/sdk-single-ledger-repro"

	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		log.Fatal(err)
	}

	dsCfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, dsCfg); err != nil {
		log.Fatal(err)
	}

	// Write a single ledger (seq 5) in Galexie's on-wire shape.
	const seq = uint32(5)
	lcm := xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{LedgerSeq: xdr.Uint32(seq)},
			},
			TxSet: xdr.GeneralizedTransactionSet{V: 1, V1TxSet: &xdr.TransactionSetV1{}},
		},
	}
	batch := xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(seq),
		EndSequence:      xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
	}
	encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
	var buf bytes.Buffer
	if _, err := encoder.WriteTo(&buf); err != nil {
		log.Fatal(err)
	}
	key := dsCfg.Schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, bytes.NewReader(buf.Bytes()), nil); err != nil {
		log.Fatal(err)
	}

	// The documented way to ask for exactly one ledger.
	rng := ledgerbackend.SingleLedgerRange(seq)

	err = ingest.ApplyLedgerMetadata(
		rng,
		ingest.PublisherConfig{
			BufferedStorageConfig: ingest.DefaultBufferedStorageBackendConfig(1),
			DataStoreConfig:       dsCfg,
		},
		ctx,
		func(xdr.LedgerCloseMeta) error {
			fmt.Println("got a ledger — this line never runs")
			return nil
		},
	)
	fmt.Printf("ApplyLedgerMetadata(SingleLedgerRange(%d)) => err = %v\n", seq, err)
}
```

An even smaller repro for a maintainer already in the `ingest`
package's own test file (`producer_test.go`): take the existing
`TestBSBProducerFn` and swap
`ledgerbackend.BoundedRange(startLedger, endLedger)` (with
`endLedger := startLedger`) for
`ledgerbackend.SingleLedgerRange(startLedger)` — it fails the same
way, no filesystem I/O needed, using the package's own
`createMockdataStore` mock.

### Expected

`ApplyLedgerMetadata(ledgerbackend.SingleLedgerRange(5), ...)` walks
exactly one ledger: invokes `callback` once with ledger 5's
`LedgerCloseMeta`, then returns `nil`.

### Actual

```
ApplyLedgerMetadata(SingleLedgerRange(5)) => err = invalid end value for bounded range, must be greater than start
```

The callback is never invoked. No I/O happens beyond opening the
datastore/schema — the rejection is pure range validation, before
`PrepareRange`/`GetLedger` are ever reached.

### Suggested fix

One-character change in `ingest/producer.go`:

```diff
- if ledgerRange.Bounded() && ledgerRange.To() <= ledgerRange.From() {
+ if ledgerRange.Bounded() && ledgerRange.To() < ledgerRange.From() {
```

This still rejects genuinely inverted ranges (`To() < From()`) and
now accepts the `To() == From()` single-ledger case that
`SingleLedgerRange` exists to construct. The `for` loop immediately
below already handles this correctly as written —
`for ledgerSeq := from; ledgerSeq <= ledgerRange.To() || !ledgerRange.Bounded(); ledgerSeq++`
runs exactly once when `from == ledgerRange.To()`; no loop-body
change needed, only the guard.

### Impact / why we noticed

Our indexer's tip-extend catch-up (a periodic job that asks for
"whatever ledgers have landed since last time") frequently computes
a range that's exactly one ledger wide — e.g. a 10-minute timer
firing exactly one ledger behind the upstream tip. Every one of
those calls hit this error, which we initially misread as an
infrastructure problem before tracing it to this validation. Roughly
half of one host's scheduled runs failed this way over one release
before we found the root cause.

### Workaround (ours, not upstream)

We don't carry a patched fork. Instead, our `internal/ledgerstream`
wrapper detects a bounded `From() == To()` range *before* calling
into the SDK's `ApplyLedgerMetadata` and routes it through our own
thin `BufferedStorageBackend` construction + `GetLedger` loop instead
(same backend, same config, just without the SDK's overly strict
guard). See commit `a375e0ad` (`fix(ledgerstream): accept + walk
single-ledger bounded ranges (To == From)`) — specifically
`streamHot` / `walkDataStore` in
`internal/ledgerstream/ledgerstream.go`, and the routing in `Stream`
that sends single-ledger bounded, non-tiered requests there instead
of through `ingest.ApplyLedgerMetadata`. This is a caller-side
workaround; it does not touch the SDK.

### Additional note

`ingest/producer_test.go`'s existing coverage
(`TestBSBProducerFn` and friends) only exercises multi-ledger bounded
ranges and the unbounded case — there's no test asserting
`SingleLedgerRange` round-trips through `ApplyLedgerMetadata`, which
is presumably how this shipped unnoticed alongside the constructor.
