package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPersistTrade_PermanentFaultIsReportedNotSwallowed pins the RLT-132
// return contract: a trade the store rejects as a permanent data fault is
// DROPPED (never retried, never landed) and persistTrade says so. It used
// to return nil, which is also what a landed trade returns — so the
// projector's per-event sink counted the dropped row as emitted and
// published it under outcome="ok".
func TestPersistTrade_PermanentFaultIsReportedNotSwallowed(t *testing.T) {
	before := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade")
	store := &fakeTradeStore{dataErr: true}
	store.healthy.Store(true)
	tr := mkTrade("soroswap", 700)

	err := persistTrade(context.Background(), discardLogger(), store, tr)
	if err == nil {
		t.Fatal("persistTrade returned nil for a permanently dropped trade — indistinguishable from a landed one, so the projector reports it outcome=ok")
	}

	var dropped *TradeDroppedError
	if !errors.As(err, &dropped) {
		t.Fatalf("err = %T (%v); want *TradeDroppedError", err, err)
	}
	if dropped.Source != tr.Source || dropped.Ledger != tr.Ledger || dropped.TxHash != tr.TxHash || dropped.OpIndex != tr.OpIndex {
		t.Errorf("TradeDroppedError identity = %s/%d/%s/%d; want the dropped trade's %s/%d/%s/%d",
			dropped.Source, dropped.Ledger, dropped.TxHash, dropped.OpIndex,
			tr.Source, tr.Ledger, tr.TxHash, tr.OpIndex)
	}

	// The cause must stay reachable: the projector classifies with
	// timescale.IsPermanentDataError (SQLSTATE 22/23) and SKIPS — an opaque
	// wrapper would classify as "unclassified" and HOLD a sole-writer cursor
	// on a row that can never land.
	if !timescale.IsPermanentDataError(err) {
		t.Error("timescale.IsPermanentDataError(err) = false; the projector would hold the cursor on a dropped trade instead of skipping it")
	}
	// …and this sink's own classifier must agree, or persistEventResilient
	// would block-and-retry the drop report forever.
	if got := classifyFault(err); got != faultData {
		t.Errorf("classifyFault(err) = %v; want faultData", got)
	}
	// Every batch-path caller keys "carry into the shutdown drain" on
	// isCtxErr; a drop must never read as an abandon.
	if isCtxErr(err) {
		t.Error("isCtxErr(err) = true for a permanent drop; flushTradeBatch would carry a poison row into the shutdown drain")
	}

	if n := store.landedCount(); n != 0 {
		t.Errorf("landed %d; want 0", n)
	}
	if got := store.rowCalls; got != 1 {
		t.Errorf("InsertTrade attempts = %d; want 1 (a permanent fault is never retried)", got)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade") - before; got != 1 {
		t.Errorf("source_insert_errors{soroswap,trade} delta = %v; want 1", got)
	}
}

// TestPersistTrade_LandedTradeStillReturnsNil is the other half of the
// contract: nil now means exactly one thing.
func TestPersistTrade_LandedTradeStillReturnsNil(t *testing.T) {
	store := &fakeTradeStore{}
	store.healthy.Store(true)
	if err := persistTrade(context.Background(), discardLogger(), store, mkTrade("soroswap", 701)); err != nil {
		t.Fatalf("persistTrade on a healthy store = %v; want nil", err)
	}
	if n := store.landedCount(); n != 1 {
		t.Fatalf("landed %d; want 1", n)
	}
}

// TestPersistTrade_AbandonStaysABareCtxError guards the pre-existing half
// of the contract the batch path depends on: a ctx-cancelled retry is an
// ABANDON (row is carried / re-derived), never a TradeDroppedError.
func TestPersistTrade_AbandonStaysABareCtxError(t *testing.T) {
	store := &fakeTradeStore{} // unhealthy → infra fault → retry loop
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := persistTrade(ctx, discardLogger(), store, mkTrade("soroswap", 702))
	if !isCtxErr(err) {
		t.Fatalf("err = %v; want the ctx error", err)
	}
	var dropped *TradeDroppedError
	if errors.As(err, &dropped) {
		t.Fatal("an abandoned (re-derivable) trade was reported as permanently dropped")
	}
}

// TestPersistTrade_AbandonIsNotCountedAsADrop pins the insert-error label
// split (GH-612): kind="trade" is what the any-rate persist_drop tripwire
// keys on, so it must mean "row gone". A ctx-abandoned retry (cursor held,
// re-derivable) counts under kind="trade_abandoned" instead. The literal
// strings are deliberate — they are the alert rule's wire contract.
func TestPersistTrade_AbandonIsNotCountedAsADrop(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade")
	abandonedBefore := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade_abandoned")
	store := &fakeTradeStore{} // unhealthy → infra fault → retry loop
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := persistTrade(ctx, discardLogger(), store, mkTrade("soroswap", 705)); !isCtxErr(err) {
		t.Fatalf("err = %v; want the ctx error", err)
	}

	if got := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade") - droppedBefore; got != 0 {
		t.Errorf("source_insert_errors{soroswap,trade} delta = %v; want 0 — an abandon is not a drop, and persist_drop tickets on this kind", got)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "soroswap", "trade_abandoned") - abandonedBefore; got != 1 {
		t.Errorf("source_insert_errors{soroswap,trade_abandoned} delta = %v; want 1", got)
	}
}

// TestHandleEvent_PermanentlyInvalidTradeReturnsDrop drives the PRODUCTION
// entry point the projector's sink is bound to (cmd/stellarindex-indexer:
// sinkFn → pipeline.HandleEvent). A zero-value trade fails
// canonical.Trade.Validate inside Store.InsertTrade before any SQL runs, so
// a nil store is never dereferenced.
func TestHandleEvent_PermanentlyInvalidTradeReturnsDrop(t *testing.T) {
	ev := soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap", Ledger: 703}}
	err := HandleEvent(context.Background(), discardLogger(), nil, ev)
	if err == nil {
		t.Fatal("HandleEvent returned nil for a trade the store permanently rejected — the projector counts it emitted/ok")
	}
	var dropped *TradeDroppedError
	if !errors.As(err, &dropped) {
		t.Fatalf("err = %T (%v); want *TradeDroppedError", err, err)
	}
	if !errors.Is(err, canonical.ErrInvalidTrade) {
		t.Errorf("err = %v; want it to wrap canonical.ErrInvalidTrade (the projector's value-shape skip arm)", err)
	}
}

// TestPersistTradeRouted_ExternalPermanentFaultIsReported — the external
// (CEX/FX) arm never goes through persistTrade, so it carried its own copy
// of the nil-on-drop contract.
func TestPersistTradeRouted_ExternalPermanentFaultIsReported(t *testing.T) {
	store := &fakeTradeStore{dataErr: true}
	store.healthy.Store(true)
	extBuf := newExternalRetryBuffer(store, discardLogger(), 8)
	src := firstExternalSource(t)

	err := persistTradeRouted(context.Background(), discardLogger(), store, extBuf, mkTrade(src, 704))
	var dropped *TradeDroppedError
	if !errors.As(err, &dropped) {
		t.Fatalf("err = %T (%v); want *TradeDroppedError", err, err)
	}
	if isCtxErr(err) {
		t.Error("isCtxErr(err) = true for a permanent drop")
	}
	extBuf.mu.Lock()
	depth := len(extBuf.ring)
	extBuf.mu.Unlock()
	if depth != 0 {
		t.Errorf("external retry buffer depth = %d; want 0 (a permanent fault must not be re-queued)", depth)
	}
}

// firstExternalSource returns a source name external.IsOnChain rejects, so
// persistTradeRouted takes its external arm.
func firstExternalSource(t *testing.T) string {
	t.Helper()
	for _, s := range []string{"binance", "kraken", "coinbase"} {
		if !external.IsOnChain(s) {
			return s
		}
	}
	t.Fatal("no external source name found")
	return ""
}
