package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSortTradesChronological_NeutralToVenueName pins GH-1154 on the
// fiat point path: the merged window's slice order picks the served
// open/close, so inside one ledger close it must follow tx_hash and not
// the alphabetical rank of the venue names. Swapping the names must leave
// every print where it was.
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
