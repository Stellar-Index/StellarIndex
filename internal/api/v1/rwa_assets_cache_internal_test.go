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

// The RWA series report failure as available=false with a nil error; that must count too.
func TestSettleRWASeries_UnavailableBuildCountsRefreshError(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	hist := obs.APICacheOpsTotal.WithLabelValues("rwa_history", "build", "refresh_error")
	prem := obs.APICacheOpsTotal.WithLabelValues("rwa_premium_history", "build", "refresh_error")
	h0, p0 := testutil.ToFloat64(hist), testutil.ToFloat64(prem)

	s.settleRWAValueHistory(rwaValueHistory{}, nil)
	s.settleRWAPremiumHistory(rwaPremiumHistory{}, nil)
	if dh, dp := testutil.ToFloat64(hist)-h0, testutil.ToFloat64(prem)-p0; dh != 1 || dp != 1 {
		t.Fatalf("refresh_error deltas = history %v, premium %v; want 1 each", dh, dp)
	}
	s.settleRWAValueHistory(rwaValueHistory{available: true}, nil)
	s.settleRWAPremiumHistory(rwaPremiumHistory{available: true}, nil)
	if dh, dp := testutil.ToFloat64(hist)-h0, testutil.ToFloat64(prem)-p0; dh != 1 || dp != 1 {
		t.Fatalf("a good build counted as refresh_error: deltas = history %v, premium %v", dh, dp)
	}
}
