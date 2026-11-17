package explorer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type truncatedDefindexReader struct {
	*slowPositionsReader
	rows []timescale.DefindexVaultFold
}

func (r *truncatedDefindexReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	return r.rows, fmt.Errorf("probe: %w", timescale.ErrPositionsTruncated)
}

// TestCollectPositions_CappedFoldIsDisclosed: a fold over the venue cap keeps its rows and names itself in
// coverage_note, so the list is not served as every venue the account holds.
func TestCollectPositions_CappedFoldIsDisclosed(t *testing.T) {
	positions := &truncatedDefindexReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.DefindexVaultFold{
		{ContractID: greepVault, SharesNet: "10"},
	}}
	h := newProbeHandler(&vaultMetaReader{capReader: capReader{probe: &deadlineProbe{}}}, positions)
	cov := &positionsCoverage{}
	got := h.collectPositions(context.Background(), validTestAccount, func(string) string { return "" }, cov)
	if len(got) != 1 {
		t.Fatalf("rows = %d, want the capped fold's rows kept", len(got))
	}
	if note := cov.note(); !strings.Contains(note, "TRUNCATED: defindex") || slices.Contains(cov.degraded, "defindex") {
		t.Errorf("coverage_note = %q, want a truncation note naming defindex and no defindex read failure", note)
	}
}

// TestBuildDefindexPositions_ZeroNetFlowIsNotClosed: a net of own flows is not a balance, so a zero net cannot hide the row.
func TestBuildDefindexPositions_ZeroNetFlowIsNotClosed(t *testing.T) {
	positions := &defindexRowsReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.DefindexVaultFold{
		{ContractID: greepVault, SharesNet: "0"},
	}}
	h := newProbeHandler(&vaultMetaReader{capReader: capReader{probe: &deadlineProbe{}}}, positions)
	got := h.buildDefindexPositions(context.Background(), validTestAccount, &positionsCoverage{})
	if len(got) != 1 || got[0].closed {
		t.Fatalf("got %+v, want one open row", got)
	}
}
