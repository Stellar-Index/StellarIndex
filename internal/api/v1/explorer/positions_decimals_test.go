package explorer

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type blendRowsReader struct {
	*slowPositionsReader
	rows []timescale.BlendPositionFold
}

func (r *blendRowsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return r.rows, nil
}

// Only a net-underlying leg names a token, so only it carries a scale; a
// superseded leg's amount is not in token units and an unreadable token
// omits the field rather than guessing 7.
func TestBuildBlendPositions_DecimalsOnlyOnNetUnderlying(t *testing.T) {
	const unreadable = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	reader := &blendRowsReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.BlendPositionFold{
		{Pool: "P1", Asset: validTestContract, HasSupplyLeg: true, SupplyNet: "5", HasBorrowLeg: true, BorrowSuperseded: true, BorrowTokens: "3"},
		{Pool: "P2", Asset: unreadable, HasSupplyLeg: true, SupplyNet: "5"},
	}}
	h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, reader)
	h.TokenDecimals = func(_ context.Context, contractID string) (int, bool) {
		return 18, contractID == validTestContract
	}
	got := h.buildBlendPositions(context.Background(), validTestAccount, func(s string) string { return s }, &positionsCoverage{})
	var dec []any
	for _, e := range got {
		if e.Decimals == nil {
			dec = append(dec, nil)
		} else {
			dec = append(dec, *e.Decimals)
		}
	}
	if want := []any{18, nil, nil}; fmt.Sprint(dec) != fmt.Sprint(want) {
		t.Errorf("decimals = %v, want %v (supply, superseded borrow, unreadable)", dec, want)
	}
}
