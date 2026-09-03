package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ctxBudgetProbe records the deadline of the context a best-effort token-
// metadata read was handed. Both fakes below embed it.
type ctxBudgetProbe struct {
	called    bool
	hadDL     bool
	remaining time.Duration
}

func (p *ctxBudgetProbe) observe(ctx context.Context) {
	p.called = true
	if dl, ok := ctx.Deadline(); ok {
		p.hadDL = true
		p.remaining = time.Until(dl)
	}
}

// overlayProbeStall is how long the wedged fakes below hold a read that its
// caller failed to bound. It must comfortably exceed tokenMetadataReadTimeout
// so a correctly-bounded overlay always cancels first, while still releasing
// an UNBOUNDED caller — so the un-fixed code fails on the assertion that
// names the defect rather than on the package test timeout.
const overlayProbeStall = 4 * time.Second

// errOverlayUnbounded is what the fakes return when they stalled the full
// overlayProbeStall without being cancelled — i.e. nothing bounded them.
var errOverlayUnbounded = errors.New("test: overlay read was never cancelled")

// stall blocks until the read's context is cancelled, or gives up after
// overlayProbeStall — the "ClickHouse is wedged" shape.
func (p *ctxBudgetProbe) stall(ctx context.Context) error {
	p.observe(ctx)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(overlayProbeStall):
		return errOverlayUnbounded
	}
}

type slowDecimalsReader struct{ probe ctxBudgetProbe }

func (r *slowDecimalsReader) TokenDecimals(ctx context.Context, _ string) (uint32, bool, error) {
	return 0, false, r.probe.stall(ctx)
}

type slowSupplyReader struct{ probe ctxBudgetProbe }

func (r *slowSupplyReader) TokenSupply(ctx context.Context, _ string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, r.probe.stall(ctx)
}

func (r *slowSupplyReader) NativeTotalCoins(ctx context.Context) (int64, uint32, error) {
	return 0, 0, r.probe.stall(ctx)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sorobanTestAsset is a well-formed Soroban asset — the only kind either
// overlay consults (classic + native are 7 by protocol and short-circuit).
func sorobanTestAsset(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(validContractID)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", validContractID, err)
	}
	if a.Type != canonical.AssetSoroban {
		t.Fatalf("fixture is %s, want a Soroban asset", a.Type)
	}
	return a
}

// TestTokenMetadataOverlays_BoundedByOwnBudget pins #371 F9. Both overlays
// are BEST-EFFORT — their failure mode is the documented decimals default
// of 7 and a null total_supply — yet both ran on the raw request context,
// so a slow ClickHouse held GET /v1/assets/{id} for the entire request
// budget before serving exactly the values it would have served in 2 s.
// The sibling resolveTokenDecimals always had the bound; these two did not.
//
// The parent context here carries NO deadline of its own, so the deadline
// the reader observes can only have come from the overlay's own sub-budget.
//
// Red without the fix: hadDL is false for both overlays (the reader is
// handed the unbounded parent verbatim) and the two "no sub-budget"
// failures fire.
func TestTokenMetadataOverlays_BoundedByOwnBudget(t *testing.T) {
	asset := sorobanTestAsset(t)

	t.Run("applyTokenDecimals", func(t *testing.T) {
		t.Parallel()
		reader := &slowDecimalsReader{}
		s := &Server{logger: discardLogger()}
		s.tokenDecimals = reader

		detail := AssetDetail{Decimals: 7}
		start := time.Now()
		s.applyTokenDecimals(context.Background(), &detail, asset)
		elapsed := time.Since(start)

		if !reader.probe.called {
			t.Fatal("the decimals reader was never consulted — the test is not exercising the overlay")
		}
		if !reader.probe.hadDL {
			t.Fatal("no sub-budget: the decimals overlay handed the reader an unbounded context, so a wedged lake stalls /v1/assets/{id} for the whole request budget")
		}
		if reader.probe.remaining <= 0 || reader.probe.remaining > tokenMetadataReadTimeout {
			t.Fatalf("sub-budget = %v, want a positive budget no larger than %v", reader.probe.remaining, tokenMetadataReadTimeout)
		}
		// The bound must actually fire, and the documented default must
		// survive it: a timed-out overlay may not corrupt Decimals (the
		// market-cap / FDV math divides by 10^Decimals).
		if elapsed > tokenMetadataReadTimeout+2*time.Second {
			t.Fatalf("overlay took %v — the sub-budget did not bound the wedged read", elapsed)
		}
		if detail.Decimals != defaultTokenDecimals {
			t.Fatalf("Decimals = %d after a failed overlay, want the documented default %d", detail.Decimals, defaultTokenDecimals)
		}
	})

	t.Run("sep41 supply overlay", func(t *testing.T) {
		t.Parallel()
		reader := &slowSupplyReader{}
		s := &Server{logger: discardLogger()}
		s.tokenSupply = reader

		detail := AssetDetail{Decimals: 7}
		start := time.Now()
		s.applyF2Fields(context.Background(), &detail, asset)
		elapsed := time.Since(start)

		if !reader.probe.called {
			t.Fatal("the supply reader was never consulted — the test is not exercising the overlay")
		}
		if !reader.probe.hadDL {
			t.Fatal("no sub-budget: the SEP-41 supply overlay handed the reader an unbounded context, so a wedged lake stalls /v1/assets/{id} for the whole request budget")
		}
		if reader.probe.remaining <= 0 || reader.probe.remaining > tokenMetadataReadTimeout {
			t.Fatalf("sub-budget = %v, want a positive budget no larger than %v", reader.probe.remaining, tokenMetadataReadTimeout)
		}
		if elapsed > tokenMetadataReadTimeout+2*time.Second {
			t.Fatalf("overlay took %v — the sub-budget did not bound the wedged read", elapsed)
		}
		// "We don't fabricate" (ADR-0011): an unavailable overlay leaves the
		// supply fields null rather than inventing a number.
		if detail.TotalSupply != nil || detail.CirculatingSupply != nil {
			t.Fatalf("supply fields populated from a failed overlay: total=%v circulating=%v", detail.TotalSupply, detail.CirculatingSupply)
		}
	})
}
