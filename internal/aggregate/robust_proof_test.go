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

// TestRobustCentreScale_EmptyValsDoesNotPanic pins that robustCentreScale on empty vals returns nil, nil instead of panicking in madRat on medianRat's nil; the guard belongs on the shared primitive.
func TestRobustCentreScale_EmptyValsDoesNotPanic(t *testing.T) {
	centre, scale := robustCentreScale(nil)
	if centre != nil || scale != nil {
		t.Fatalf("robustCentreScale(nil) = (%v, %v), want (nil, nil)", centre, scale)
	}
}
