package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestAccountIsUnspendable: a locked burn account (master weight 0, no signers)
// is unspendable at ANY threshold — stellar-core only admits the master key as
// a signer when its weight is nonzero, so no signature set can be produced.
func TestAccountIsUnspendable(t *testing.T) {
	cases := []struct {
		th      xdr.Thresholds
		signers int
		want    bool
		msg     string
	}{
		// SetOptions{master_weight:0, low:1, med:1, high:1}: the documented "burn" recipe.
		{xdr.NewThreshold(0, 1, 1, 1), 0, true, "burn recipe (master=0, thresholds=1/1/1, no signers) must be reported unspendable"},
		{xdr.NewThreshold(0, 0, 0, 0), 0, true, "master=0, thresholds=0/0/0, no signers must remain flagged unspendable"},
		{xdr.NewThreshold(0, 1, 1, 1), 1, false, "account with a live signer must not be reported unspendable"},
		{xdr.NewThreshold(1, 1, 1, 1), 0, false, "account with nonzero master weight must not be reported unspendable"},
	}
	for _, tc := range cases {
		if got := accountIsUnspendable(tc.th, tc.signers); got != tc.want {
			t.Error(tc.msg)
		}
	}
}
