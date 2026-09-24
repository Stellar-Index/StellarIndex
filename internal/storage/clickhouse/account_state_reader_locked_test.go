package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestAccountIsUnspendable_BurnRecipeWithNonzeroThresholds covers CA2-A14-correct-2:
// a locked burn account (master weight 0, no signers) is unspendable at ANY
// threshold — stellar-core only admits the master key as a signer when its
// weight is nonzero, so weight 0 with no other signers means no signature
// set can ever be produced, regardless of what the thresholds require.
func TestAccountIsUnspendable_BurnRecipeWithNonzeroThresholds(t *testing.T) {
	// SetOptions{master_weight:0, low:1, med:1, high:1}, no other signers —
	// the documented "burn" recipe. Must be flagged locked.
	th := xdr.NewThreshold(0, 1, 1, 1)
	if !accountIsUnspendable(th, 0) {
		t.Fatalf("burn recipe (master=0, thresholds=1/1/1, no signers) must be reported unspendable")
	}
}

func TestAccountIsUnspendable_StillFlagsAllZeroThresholds(t *testing.T) {
	th := xdr.NewThreshold(0, 0, 0, 0)
	if !accountIsUnspendable(th, 0) {
		t.Fatalf("master=0, thresholds=0/0/0, no signers must remain flagged unspendable")
	}
}

func TestAccountIsUnspendable_SpendableWhenSignersPresent(t *testing.T) {
	th := xdr.NewThreshold(0, 1, 1, 1)
	if accountIsUnspendable(th, 1) {
		t.Fatalf("account with a live signer must not be reported unspendable")
	}
}

func TestAccountIsUnspendable_SpendableWhenMasterWeightNonzero(t *testing.T) {
	th := xdr.NewThreshold(1, 1, 1, 1)
	if accountIsUnspendable(th, 0) {
		t.Fatalf("account with nonzero master weight must not be reported unspendable")
	}
}
