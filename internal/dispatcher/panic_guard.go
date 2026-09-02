package dispatcher

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ErrDecoderPanic wraps every error a recovered decoder panic is turned
// into, so a caller that cares (an ops diagnostic, a test) can tell a
// crash apart from an ordinary decode refusal with errors.Is.
var ErrDecoderPanic = errors.New("decoder panicked")

// panicSite is the ledger coordinate of the input a decoder crashed on.
// Carried into the log line so an operator can pull the exact raw event
// out of the ClickHouse lake and replay it against the fixed decoder.
type panicSite struct {
	Ledger  uint32
	TxHash  string
	OpIndex int
}

// recordDecoderPanic converts a recovered decoder panic into the decode
// error every dispatch seam already skips on (#371 F1).
//
// The problem it removes: a decoder's Matches/Decode is arbitrary source
// code running on adversary-influenced ledger data, and an index-out-of
// -range in one of them used to unwind through ProcessLedger, get caught
// at LEDGER granularity in pipeline.ProcessLedger, and be returned as
// "dispatcher panic for ledger N". That discards the outputs of EVERY
// source for that ledger, refuses the cursor advance, and returns an
// error the indexer's realMain turns into a process exit — so systemd
// restarts, the same ledger is re-read from the same cursor, the same
// decoder panics on the same event, and after StartLimitBurst restarts
// the unit parks in `failed`. One decoder's bug is a total ingest
// outage, indefinitely.
//
// The dispatch seams already have a policy for "this decoder cannot
// handle this input": count it and skip that ONE input, leaving every
// other decoder and the rest of the ledger untouched (see the
// `if err != nil { continue }` arms in ProcessLedger). A panic is the
// same fact stated more loudly, so it gets the same handling — the
// blast radius shrinks from "all sources, forever" to "one input, one
// decoder".
//
// That trade is only defensible because the skip is DURABLY RECORDED,
// three ways, and none of them depends on this process surviving:
//
//   - The raw event is already in the ClickHouse lake. dispatchOne
//     pushes to rawEventSink BEFORE the decoder pass, and the lake
//     extractor (clickhouse.ExtractLedger) is decoder-independent — so
//     the substrate keeps its genesis-to-tip claim and re-derivation
//     after a decoder fix is `projector-replay` / `ch-rebuild`, per
//     invariant 8.
//   - The decode-error delta reaches decoder_stats via statsflush, and
//     ADR-0033's re-derive marks the ledger a blind spot
//     (completeness.safeDecode → BlindSpots → projection_ok=false →
//     /v1/coverage complete=false), so the coverage VERDICT tells the
//     truth about the gap rather than papering over it.
//   - DecoderPanicsTotal pages immediately (stellarindex_decoder_panicked).
//
// A panicking Matches is treated exactly like a panicking Decode: the
// input is that decoder's error, and the seam stops scanning. Offering
// the input to the NEXT decoder instead would let a broken decoder
// silently hand its events to a different source — a misattribution,
// which ADR-0033 cannot see, whereas the gap this produces it can.
//
// seenCounted says whether bumpEventsSeen already ran for this input
// (i.e. the panic came out of Decode, not Matches). When it did not, we
// bump it here so the denominator of "decoder error rate" keeps
// counting one input attempted per error — the invariant
// SourceMatchedEventsTotal's godoc states.
func (d *Dispatcher) recordDecoderPanic(name string, seenCounted bool, r any, site panicSite) error {
	if name == "" {
		name = "unknown"
	}
	if !seenCounted {
		d.bumpEventsSeen(name)
	}
	d.bumpDecodeError(name)
	// Count before logging so the page fires even if the log sink is
	// broken — same ordering rationale as worker.Recover.
	obs.DecoderPanicsTotal.WithLabelValues(name).Inc()
	d.log().Error("decoder panicked — input SKIPPED, ingest continues",
		"decoder", name,
		"ledger", site.Ledger,
		"tx_hash", site.TxHash,
		"op_index", site.OpIndex,
		"panic", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()))
	return fmt.Errorf("%w: %s: %v", ErrDecoderPanic, name, r)
}

// log returns the dispatcher's logger, falling back to slog.Default()
// when the caller never wired one (the ops diagnostics build a bare
// Dispatcher). Never nil, because the only caller is the panic path and
// a nil-deref there would turn a recovered panic back into a fatal one.
func (d *Dispatcher) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}
