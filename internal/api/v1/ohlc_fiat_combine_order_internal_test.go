package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSortTradesChronological_NeutralToVenueName pins that on the fiat point path the merged window's order, which picks
// open/close, follows tx_hash within a ledger close, not venue-name rank; swapping names moves nothing.
func TestSortTradesChronological_NeutralToVenueName(t *testing.T) {
	ts := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	want := []string{"aa", "bb", "cc"}
	for _, names := range [][2]string{{"aquarius", "soroswap"}, {"soroswap", "aquarius"}} {
		trades := []canonical.Trade{
			{Source: names[0], Ledger: 900, TxHash: "cc", Timestamp: ts},
			{Source: names[0], Ledger: 900, TxHash: "bb", Timestamp: ts},
			{Source: names[1], Ledger: 900, TxHash: "aa", Timestamp: ts},
		}
		sortTradesChronological(trades)
		for k := range want {
			if trades[k].TxHash != want[k] {
				t.Fatalf("venues %v: position %d = %q (%s), want %q — the open of a "+
					"same-ledger window must not go to the alphabetically-first venue",
					names, k, trades[k].TxHash, trades[k].Source, want[k])
			}
		}
	}
}
