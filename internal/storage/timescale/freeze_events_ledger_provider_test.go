package timescale

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeLedgerProvider is a stub LedgerProvider returning a fixed ledger.
type fakeLedgerProvider struct{ ledger uint32 }

func (f fakeLedgerProvider) LatestLedger() uint32 { return f.ledger }

// TestRecordFreeze_StampsLedgerFromProvider pins that frozen_at_ledger
// reflects a wired LedgerProvider, not the 0 sentinel. Without
// WithFreezeLedgerProvider no caller can set
// FreezeEventSink.getLedger, so every production insert would stamp 0.
func TestRecordFreeze_StampsLedgerFromProvider(t *testing.T) {
	// Statement 0: advisory lock exec. Statement 1: the INSERT.
	store, conn := newScriptedStore(t,
		scriptedResult{rowsAffected: 0},
		scriptedResult{rowsAffected: 1},
	)

	sink := NewFreezeEventSink(store, WithFreezeLedgerProvider(fakeLedgerProvider{ledger: 918273}))

	asset, err := canonical.ParseAsset("crypto:BTC")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}

	if err := sink.RecordFreeze(context.Background(), asset, quote, "1.23", anomaly.Decision{}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}

	if len(conn.stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(conn.stmts))
	}
	insert := conn.stmts[1]
	got := insert.arg(t, 4)
	if got != int64(918273) {
		t.Errorf("frozen_at_ledger arg = %v, want 918273 (from the wired LedgerProvider, not the 0 sentinel)", got)
	}
}
