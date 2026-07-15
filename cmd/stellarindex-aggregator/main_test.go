package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestDefaultPairs_IncludesBothXLMForms guards against regression of
// the on-r1 launch finding: the abstract `crypto:XLM` ticker and the
// Stellar-protocol `native` form are different cache keys, and the
// aggregator must publish for both so a customer query under either
// form lands on a populated key. On-chain DEX/SDEX trades store
// `native` quote-asset; off-chain CEX trades emit `crypto:XLM`.
func TestDefaultPairs_IncludesBothXLMForms(t *testing.T) {
	got := defaultPairs()

	hasNativeUSD := false
	hasCryptoXLMUSD := false
	for _, p := range got {
		if p.Quote.Type != canonical.AssetFiat || p.Quote.Code != "USD" {
			continue
		}
		//exhaustive:ignore // test only asserts the Native + Crypto XLM/USD pairs exist
		switch p.Base.Type {
		case canonical.AssetNative:
			hasNativeUSD = true
		case canonical.AssetCrypto:
			if p.Base.Code == "XLM" {
				hasCryptoXLMUSD = true
			}
		}
	}
	if !hasNativeUSD {
		t.Error("defaultPairs missing native/fiat:USD — on-chain XLM trades will publish to a key the API never queries")
	}
	if !hasCryptoXLMUSD {
		t.Error("defaultPairs missing crypto:XLM/fiat:USD — CEX/FX XLM trades will publish to a key the API never queries")
	}
}

// TestResolveUSDPeggedSorobanAssets — Guard 1 (2026-07-10): the SAC
// twin of parseUSDPeggedClassicAssets. A SAC contract inherits a USD
// peg ONLY when BOTH: its underlying classic ("CODE:ISSUER"/
// "CODE-ISSUER") is on the operator's usd_pegged_classic_assets list
// AND it's registered in [supply].sac_wrappers. No new TOML knob —
// this derives entirely from the two existing operator inputs.
func TestResolveUSDPeggedSorobanAssets(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const usdcClassic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	const otherClassic = "YXLM-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	const otherSAC = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"

	t.Run("empty inputs yield nil", func(t *testing.T) {
		if got := resolveUSDPeggedSorobanAssets(nil, nil, logger); got != nil {
			t.Errorf("got %v, want nil", got)
		}
		if got := resolveUSDPeggedSorobanAssets([]string{usdcClassic}, nil, logger); got != nil {
			t.Errorf("got %v, want nil (no sac_wrappers configured)", got)
		}
		if got := resolveUSDPeggedSorobanAssets(nil, map[string]string{usdcSAC: usdcClassic}, logger); got != nil {
			t.Errorf("got %v, want nil (no usd_pegged_classic_assets configured)", got)
		}
	})

	t.Run("SAC wrapping a declared USD peg resolves", func(t *testing.T) {
		got := resolveUSDPeggedSorobanAssets(
			[]string{usdcClassic},
			map[string]string{usdcSAC: usdcClassic},
			logger,
		)
		if len(got) != 1 || got[0].Type != canonical.AssetSoroban || got[0].ContractID != usdcSAC {
			t.Fatalf("got %v, want [{Soroban %s}]", got, usdcSAC)
		}
	})

	t.Run("SAC wrapping a NON-declared classic is excluded", func(t *testing.T) {
		got := resolveUSDPeggedSorobanAssets(
			[]string{usdcClassic}, // declared peg is USDC, not the "other" classic
			map[string]string{otherSAC: otherClassic},
			logger,
		)
		if len(got) != 0 {
			t.Errorf("got %v, want empty — sac_wrappers entry's classic isn't a declared USD peg", got)
		}
	})

	t.Run("mixed sac_wrappers: only the pegged one resolves", func(t *testing.T) {
		got := resolveUSDPeggedSorobanAssets(
			[]string{usdcClassic},
			map[string]string{
				usdcSAC:  usdcClassic,
				otherSAC: otherClassic,
			},
			logger,
		)
		if len(got) != 1 || got[0].ContractID != usdcSAC {
			t.Fatalf("got %v, want exactly [{Soroban %s}]", got, usdcSAC)
		}
	})

	t.Run("malformed sac_wrappers value is skipped, not fatal", func(t *testing.T) {
		got := resolveUSDPeggedSorobanAssets(
			[]string{usdcClassic},
			map[string]string{usdcSAC: "not-a-valid-asset-key"},
			logger,
		)
		if len(got) != 0 {
			t.Errorf("got %v, want empty — malformed sac_wrappers value must be skipped, not panic", got)
		}
	})
}

// TestBuildTriangulations_RespectsTriangulationEnabled pins down the
// aggregate.triangulation_enabled master switch — pre-2026-05-02 the
// field existed but no production code consulted it, so an operator
// setting it false still got triangulation. The wiring lives in
// buildTriangulations: when the switch is false, return nil so the
// orchestrator's `len(cfg.Triangulations) == 0` short-circuit skips
// the triangulation tick. Validation still runs first so a malformed
// row is caught regardless of the switch state.
func TestBuildTriangulations_RespectsTriangulationEnabled(t *testing.T) {
	row := config.TriangulationChainConfig{
		Target: "crypto:XLM/fiat:EUR",
		Legs:   []string{"crypto:XLM/fiat:USD", "fiat:USD/fiat:EUR"},
	}

	t.Run("enabled returns the configured chains", func(t *testing.T) {
		cfg := config.AggregateConfig{
			TriangulationEnabled: true,
			Triangulations:       []config.TriangulationChainConfig{row},
		}
		out, err := buildTriangulations(cfg)
		if err != nil {
			t.Fatalf("buildTriangulations: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("len(out) = %d, want 1", len(out))
		}
		if got := out[0].Target.String(); got != row.Target {
			t.Errorf("Target = %q, want %q", got, row.Target)
		}
	})

	t.Run("disabled returns nil even with rows configured", func(t *testing.T) {
		cfg := config.AggregateConfig{
			TriangulationEnabled: false,
			Triangulations:       []config.TriangulationChainConfig{row},
		}
		out, err := buildTriangulations(cfg)
		if err != nil {
			t.Fatalf("buildTriangulations: %v", err)
		}
		if out != nil {
			t.Errorf("len(out) = %d, want nil — switch is OFF", len(out))
		}
	})

	t.Run("disabled still validates rows so flip-on doesn't surprise", func(t *testing.T) {
		bad := config.TriangulationChainConfig{
			Target: "crypto:XLM/fiat:EUR",
			Legs:   []string{"crypto:XLM/fiat:USD"}, // < 2 legs — invalid
		}
		cfg := config.AggregateConfig{
			TriangulationEnabled: false,
			Triangulations:       []config.TriangulationChainConfig{bad},
		}
		_, err := buildTriangulations(cfg)
		if err == nil {
			t.Fatal("buildTriangulations: want error for malformed row, got nil")
		}
		if !strings.Contains(err.Error(), "triangulations[0]") {
			t.Errorf("err = %v; want substring 'triangulations[0]'", err)
		}
	})
}

// TestRunSupplyRefresh_DurationMetricRecorded pins the wave-90
// (2026-05-13) latency-histogram wiring on the supply-refresh
// loop. Final entry in the wave-92/93/94 regression-test series.
//
// Setup: build a real *supply.Refresher with stub
// LedgerLookup/SnapshotComputer/SnapshotInserter (the supply
// package's own interfaces — production impls are timescale-
// backed, the test ones are in-memory). Pre-cancel the context
// so the immediate first tick runs once and the ticker loop
// exits via <-ctx.Done() without firing.
func TestRunSupplyRefresh_DurationMetricRecorded(t *testing.T) {
	r := supply.NewRefresher(
		stubSupplyLedgers{ledger: 50_000_000, observedAt: time.Unix(1_770_000_000, 0).UTC()},
		stubSupplyComputer{out: supply.Supply{
			AssetKey:          "TEST",
			TotalSupply:       big.NewInt(1_000_000),
			CirculatingSupply: big.NewInt(900_000),
			Basis:             supply.BasisXLMSDFReserveExclusion,
		}},
		&stubSupplyInserter{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	before := obstest.HistogramSampleCount(t, obs.AggregatorSupplyRefreshDurationSeconds, "outcome", "ok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate-first-tick runs; for-loop sees ctx.Done() and returns
	runSupplyRefresh(ctx, r, time.Hour, "TEST")

	after := obstest.HistogramSampleCount(t, obs.AggregatorSupplyRefreshDurationSeconds, "outcome", "ok")
	if after <= before {
		t.Errorf("supply refresh duration histogram did not advance: before=%d after=%d", before, after)
	}
}

// ─── stubs for TestRunSupplyRefresh_DurationMetricRecorded ──────
//
// Mirror the (unexported) stubs in internal/supply/refresher_test.go.
// Re-implemented here since the supply package's stubs are
// package-private; the cost of duplicating ~25 lines beats either
// exporting test fixtures or adding a separate testfixture
// subpackage.

type stubSupplyLedgers struct {
	ledger     uint32
	observedAt time.Time
}

func (s stubSupplyLedgers) LatestKnownLedger(_ context.Context) (uint32, time.Time, error) {
	return s.ledger, s.observedAt, nil
}

type stubSupplyComputer struct {
	out supply.Supply
}

func (s stubSupplyComputer) Compute(_ context.Context, ledger uint32, observedAt time.Time) (supply.Supply, error) {
	out := s.out
	out.LedgerSequence = ledger
	out.ObservedAt = observedAt
	return out, nil
}

type stubSupplyInserter struct{}

func (*stubSupplyInserter) InsertSupply(_ context.Context, _ supply.Supply) error { return nil }

// TestRunSEP41SupplyRollup_AdvancesSeriallyAndRecordsOutcomes drives the
// migration-0085 rollup worker through one fold pass against a fake
// advancer and pins the two properties the incident-2026-07-06 fix
// depends on:
//
//  1. Every watched contract is advanced and the (contract_id, outcome)
//     counter + paired duration histogram record the right outcome
//     (ok / noop / error).
//  2. The worker advances contracts ONE AT A TIME. A cold contract's
//     first fold is a full-history sum; if the worker fanned them out it
//     would recreate the concurrent full-table scans that saturated
//     Postgres — so the fake asserts it is never entered re-entrantly.
func TestRunSEP41SupplyRollup_AdvancesSeriallyAndRecordsOutcomes(t *testing.T) {
	const (
		cOK   = "CROLLUPOK00000000000000000000000000000000000000000000000"
		cNoop = "CROLLUPNOOP000000000000000000000000000000000000000000000"
		cErr  = "CROLLUPERR0000000000000000000000000000000000000000000000"
	)
	contracts := []string{cOK, cNoop, cErr}

	fake := &fakeRollupAdvancer{
		results: map[string]timescale.SEP41RollupAdvance{
			cOK:   {ContractID: cOK, FromLedger: 0, ToLedger: 100, Advanced: true},
			cNoop: {ContractID: cNoop, Advanced: false},
		},
		errs: map[string]error{cErr: errors.New("boom")},
	}

	beforeOK := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cOK, "ok"))
	beforeNoop := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cNoop, "noop"))
	beforeErr := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cErr, "error"))
	beforeHist := obstest.HistogramSampleCount(t, obs.SEP41SupplyRollupAdvanceDurationSeconds, "outcome", "ok")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Hour cadence: only the immediate first fold runs before we cancel.
		runSEP41SupplyRollup(ctx, fake, contracts, time.Hour,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	waitForRollup(t, func() bool { return fake.callCount() >= len(contracts) })
	cancel()
	<-done

	if got := fake.maxConcurrent(); got != 1 {
		t.Fatalf("advancer ran %d-way concurrent; the worker must advance contracts serially", got)
	}
	if got := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cOK, "ok")); got <= beforeOK {
		t.Errorf("ok counter for %s did not advance: before=%v after=%v", cOK, beforeOK, got)
	}
	if got := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cNoop, "noop")); got <= beforeNoop {
		t.Errorf("noop counter (unadvanced contract) did not record")
	}
	if got := testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cErr, "error")); got <= beforeErr {
		t.Errorf("error counter (failed advance) did not record")
	}
	if got := obstest.HistogramSampleCount(t, obs.SEP41SupplyRollupAdvanceDurationSeconds, "outcome", "ok"); got <= beforeHist {
		t.Errorf("advance duration histogram (ok) did not advance: before=%d after=%d", beforeHist, got)
	}
}

// fakeRollupAdvancer is an in-memory sep41RollupAdvancer that records
// call count + peak concurrency so the worker test can assert serial
// execution without a database.
type fakeRollupAdvancer struct {
	results map[string]timescale.SEP41RollupAdvance
	errs    map[string]error

	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
}

func (f *fakeRollupAdvancer) AdvanceSEP41SupplyRollup(_ context.Context, contractID string) (timescale.SEP41RollupAdvance, error) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()

	// Hold briefly so any concurrent entry would overlap and bump the peak.
	time.Sleep(2 * time.Millisecond)

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err := f.errs[contractID]; err != nil {
		return timescale.SEP41RollupAdvance{}, err
	}
	return f.results[contractID], nil
}

func (f *fakeRollupAdvancer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRollupAdvancer) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func waitForRollup(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for rollup worker to advance all contracts")
}
