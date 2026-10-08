package ledgerstream

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
)

// TestValidateRange pins the single-ledger fix: a bounded range of
// exactly one ledger (To == From) is valid — the SDK models it as
// ledgerbackend.SingleLedgerRange and the walk loop runs one
// iteration. A `To() <= From()` check would reject it and make
// ch-live-catchup's tip-extend fail whenever the timer fires exactly one
// ledger behind the galexie tip.
func TestValidateRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		r       ledgerbackend.Range
		wantErr bool
	}{
		{"bounded multi-ledger", ledgerbackend.BoundedRange(10, 20), false},
		{"bounded single ledger (To == From)", ledgerbackend.SingleLedgerRange(62984354), false},
		{"bounded inverted (To < From)", ledgerbackend.BoundedRange(20, 10), true},
		{"unbounded", ledgerbackend.UnboundedRange(10), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRange(tc.r)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRange(%v) err = %v, wantErr %v", tc.r, err, tc.wantErr)
			}
		})
	}
}
