package redstone

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input; a
// failure here means a ts-derivation change needs its own cleanup run (see
// "Re-deriving a timestamp" in docs/architecture/ingest-pipeline.md).
func TestDecodeWritePrices_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	ceil := closedAt.Add(24 * time.Hour) // literal: a change to the shared window must trip this

	for name, tc := range map[string]struct {
		pkgTsMs uint64
		want    time.Time
	}{
		"package ts wins (past)":     {1_745_000_000_000, time.UnixMilli(1_745_000_000_000)},
		"millisecond precision kept": {1_745_000_000_123, time.UnixMilli(1_745_000_000_123)},
		"zero clamps to close":       {0, closedAt},
		"pre-2001 floor clamps":      {999_999_999_999, closedAt},
		"at 2001 floor kept":         {1_000_000_000_000, time.UnixMilli(1_000_000_000_000)},
		"at close+24h kept":          {uint64(ceil.UnixMilli()), ceil},
		"close+24h+1ms clamps":       {uint64(ceil.UnixMilli()) + 1, closedAt},
		"u64 max clamps":             {^uint64(0), closedAt},
	} {
		t.Run(name, func(t *testing.T) {
			body := encodeWritePricesBody(t, relayerG,
				[]*big.Int{big.NewInt(oneBTCAt8)}, tc.pkgTsMs, 1_745_000_060_000)
			ev := &events.Event{
				Topic: []string{TopicSymbolRedstone},
				Value: body,
				OpArgs: []string{
					encodeAddressArg(t, relayerG),
					encodeStringVecArg(t, []string{"BTC"}),
					encodePayloadArg(t),
				},
				ContractID:     adapterC,
				Ledger:         52_000_000,
				TxHash:         "abcd",
				LedgerClosedAt: closedAt.Format(time.RFC3339),
			}
			got, err := decodeWritePrices(ev, closedAt)
			if err != nil {
				t.Fatalf("decodeWritePrices: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d updates, want 1", len(got))
			}
			if !got[0].Timestamp.Equal(tc.want) {
				t.Errorf("ts = %s (%d ms), want %s (%d ms)",
					got[0].Timestamp, got[0].Timestamp.UnixMilli(), tc.want, tc.want.UnixMilli())
			}
		})
	}
}
