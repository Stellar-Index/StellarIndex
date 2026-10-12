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

// overlayProbeStall is how long wedged fakes hold a read its caller failed to bound. It must exceed
// tokenMetadataReadTimeout so a bounded overlay cancels first, yet release an unbounded caller so a regression
// fails on the named assertion, not the package timeout.
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

// TestTokenMetadataOverlays_BoundedByOwnBudget: both overlays are best-effort (fallback is decimals 7 and null total_supply),
// so each needs its own sub-budget; on the raw request context a slow ClickHouse would hold GET /v1/assets/{id} for the whole
// budget. The parent context has NO deadline, so any deadline the reader sees comes from the overlay.
func TestTokenMetadataOverlays_BoundedByOwnBudget(t *testing.T) {
	asset := sorobanTestAsset(t)

	t.Run("applyTokenDecimals", func(t *testing.T) {
		t.Parallel()
		reader := &slowDecimalsReader{}
		s := &Server{logger: discardLogger()}
		s.TokenDecimals = reader

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
		s.TokenSupply = reader

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
