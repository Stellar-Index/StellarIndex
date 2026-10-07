package reflector

import (
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input; a
// failure here means a ts-derivation change needs its own cleanup run (see
// "Re-deriving a timestamp" in docs/architecture/ingest-pipeline.md).
func TestDecodeUpdate_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	ceil := closedAt.Add(24 * time.Hour) // literal: a change to the shared window must trip this

	for name, tc := range map[string]struct {
		topicMs uint64
		want    time.Time
	}{
		"topic ms wins (past)":       {1_745_123_456_000, time.UnixMilli(1_745_123_456_000)},
		"millisecond precision kept": {1_745_123_456_789, time.UnixMilli(1_745_123_456_789)},
		"zero clamps to close":       {0, closedAt},
		"pre-2001 floor clamps":      {999_999_999_999, closedAt},
		"at 2001 floor kept":         {1_000_000_000_000, time.UnixMilli(1_000_000_000_000)},
		"at close+24h kept":          {uint64(ceil.UnixMilli()), ceil},
		"close+24h+1ms clamps":       {uint64(ceil.UnixMilli()) + 1, closedAt},
		"u64 max clamps":             {^uint64(0), closedAt},
	} {
		t.Run(name, func(t *testing.T) {
			usd := xdr.ScSymbol("USD")
			body := encodeUpdateBody(t,
				[]xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &usd}},
				[]*big.Int{big.NewInt(100_000_000_000_000)})
			e := &events.Event{
				Topic:          []string{TopicSymbolReflector, TopicSymbolUpdate, encodeTimestampTopic(t, tc.topicMs)},
				Value:          body,
				ContractID:     adapterContract,
				Ledger:         52_000_000,
				TxHash:         "abc",
				LedgerClosedAt: closedAt.Format(time.RFC3339),
			}
			got, err := decodeUpdate(e, VariantCEX, DefaultDecimals, "GRELAYER", closedAt)
			if err != nil {
				t.Fatalf("decodeUpdate: %v", err)
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
