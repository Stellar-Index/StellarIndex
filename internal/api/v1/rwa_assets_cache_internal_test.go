package v1

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// A failed RWA view build keeps serving the last good totals, so it must count as a refresh_error or no alert can see it.
func TestSettleRWAAssetsView_FailedBuildCountsRefreshError(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := obs.APICacheOpsTotal.WithLabelValues("rwa_assets", "build", "refresh_error")
	before := testutil.ToFloat64(c)
	s.settleRWAAssetsView(rwaAssetsBuild{}, errors.New("lake query over budget"))
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Fatalf("refresh_error delta = %v, want 1", got)
	}
	s.settleRWAAssetsView(rwaAssetsBuild{}, nil)
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Fatalf("a good build counted as refresh_error: delta = %v, want 1", got)
	}
}
