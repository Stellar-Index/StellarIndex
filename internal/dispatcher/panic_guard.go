package dispatcher

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ErrDecoderPanic wraps every error a recovered decoder panic is turned
// into, so a caller that cares (an ops diagnostic, a test) can tell a
// crash apart from an ordinary decode refusal with errors.Is.
var ErrDecoderPanic = errors.New("decoder panicked")

// panicSite is the ledger coordinate of the input a decoder crashed on.
// Carried into the log line so an operator can pull the exact raw event
// out of the ClickHouse lake and replay it against the corrected decoder.
type panicSite struct {
	Ledger  uint32
	TxHash  string
	OpIndex int
}

// recordDecoderPanic converts a recovered decoder panic into the decode
// error every dispatch seam already skips on.
//
// A decoder's Matches/Decode runs on adversary-influenced ledger data. An
// unrecovered panic would be caught at LEDGER granularity in
// pipeline.ProcessLedger, discarding every source's output for that ledger and
// refusing the cursor advance; the indexer then exits, restarts at the same
// cursor, panics on the same event, and parks in `failed` after
// StartLimitBurst: one decoder bug as a total ingest outage. Treating a panic
// like any other decode error (count it, skip that ONE input) shrinks the
// blast radius to one input, one decoder.
//
// That is defensible only because the skip is DURABLY RECORDED:
//   - The raw event is already in the ClickHouse lake (dispatchOne pushes to
//     rawEventSink BEFORE the decoder pass; clickhouse.ExtractLedger is
//     decoder-independent), so re-derivation is `projector-replay` /
//     `ch-rebuild` (invariant 8).
//   - The decode-error delta reaches decoder_stats via statsflush, and
//     ADR-0033's re-derive marks the ledger a blind spot
//     (completeness.safeDecode → BlindSpots → /v1/coverage complete=false).
//   - DecoderPanicsTotal pages (stellarindex_decoder_panicked).
//
// A panicking Matches is treated like a panicking Decode, and a first-match
// seam stops scanning: offering the input to the NEXT decoder would let a
// broken decoder hand its events to another source, a misattribution ADR-0033
// cannot see. The op seam is the exception: it offers every op to every
// decoder, so continuing re-attributes nothing.
//
// seenCounted says whether bumpEventsSeen already ran (the panic came out of
// Decode, not Matches); if not, we bump it here so the decoder error-rate
// denominator keeps one input attempted per error, as
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
// Dispatcher). Never nil: a nil-deref on the panic path would turn a
// recovered panic back into a fatal one.
func (d *Dispatcher) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// DecodeRow runs one lake row through dec for a caller outside the dispatch
// loop (the projector, projected-rebuild) under the same panic discipline as
// recordDecoderPanic. Those callers skip a failed row and advance past it, so
// the skip must be loud here, where no caller can forget it: a recovered panic
// is counted in DecoderPanicsTotal and logged with its stack, and a returned
// decode error is logged with the row coordinate.
//
// matched is false when dec does not claim ev (outs and err are then nil). A
// panic in Matches counts as that decoder's failure, exactly as in the
// dispatcher. err is the decoder's own error or an [ErrDecoderPanic] wrap.
func DecodeRow(name string, dec Decoder, ev events.Event, log *slog.Logger) (outs []consumer.Event, matched bool, err error) {
	if name == "" {
		name = "unknown"
	}
	if log == nil {
		log = slog.Default()
	}
	defer func() {
		if r := recover(); r != nil {
			obs.DecoderPanicsTotal.WithLabelValues(name).Inc()
			log.Error("decoder panicked; row SKIPPED",
				"source", name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex,
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			outs, matched, err = nil, true, fmt.Errorf("%w: %s: %v", ErrDecoderPanic, name, r)
		}
	}()
	if !dec.Matches(ev) {
		return nil, false, nil
	}
	outs, err = dec.Decode(ev)
	if err != nil {
		log.Warn("decode failed; row SKIPPED",
			"source", name, "ledger", ev.Ledger, "tx", ev.TxHash,
			"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", err)
		return nil, true, err
	}
	return outs, true, nil
}
