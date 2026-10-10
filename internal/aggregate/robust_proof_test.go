package aggregate

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkAggRow builds an aggregator-class OracleUpdate whose raw integer
// price is `priceScaled` at `decimals` fixed-point.
func mkAggRow(source string, priceScaled int64, decimals uint8) canonical.OracleUpdate {
	return canonical.OracleUpdate{
		Source:    source,
		Timestamp: time.Now().UTC(),
		Price:     canonical.NewAmount(big.NewInt(priceScaled)),
		Decimals:  decimals,
	}
}

// TestRobustCentreScale_EmptyValsDoesNotPanic pins a latent
// panic: medianRat(nil) returns nil (its own documented defensive
// behaviour), and robustCentreScale without a guard would pass that nil straight into
// madRat -> big.Rat.Sub as the subtrahend with no guard of its own,
// which panics on a nil-pointer dereference rather than returning the
// same "nothing to judge" nil, nil every other empty-input path here
// uses. Every current caller happens to guard len(vals)>0 first, but
// the guard belongs on the shared primitive, not on each caller's
// memory of doing so.
func TestRobustCentreScale_EmptyValsDoesNotPanic(t *testing.T) {
	centre, scale := robustCentreScale(nil)
	if centre != nil || scale != nil {
		t.Fatalf("robustCentreScale(nil) = (%v, %v), want (nil, nil)", centre, scale)
	}
}
