package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/astguard"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestDefaultPairs_IncludesBothXLMForms guards that the aggregator publishes under both `crypto:XLM` and `native`,
// which are different cache keys: on-chain DEX/SDEX trades store `native`, off-chain CEX trades emit `crypto:XLM`.
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

// TestResolveUSDPeggedSorobanAssets covers the SAC twin of config.TradesConfig.USDPeggedClassics.
// A SAC contract inherits a USD peg ONLY when BOTH its underlying classic ("CODE:ISSUER" or
// "CODE-ISSUER") is on usd_pegged_classic_assets AND it is registered in [supply].sac_wrappers.
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

// TestBuildTriangulations_RespectsTriangulationEnabled pins the aggregate.triangulation_enabled master switch: false
// yields nil, so the orchestrator's `len(cfg.Triangulations) == 0` short-circuit skips the tick. Validation runs
// first, so malformed rows still fail.
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

// TestRunSupplyRefresh_DurationMetricRecorded pins the latency-histogram wiring on the
// supply-refresh loop. It uses a real *supply.Refresher over in-memory stubs and a pre-cancelled
// context, so the immediate first tick runs once and the ticker loop exits via <-ctx.Done().
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

// Stubs for TestRunSupplyRefresh_DurationMetricRecorded, mirroring internal/supply/refresher_test.go; duplicating them
// beats exporting test fixtures.

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

// TestRunSEP41SupplyRollup_AdvancesSeriallyAndRecordsOutcomes pins that every watched contract advances with the right
// ok/noop/error outcome and duration recorded, and that contracts advance one at a time: a cold first fold is a full-
// history sum, so fanning out would run concurrent full-table scans.
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

// TestRunSEP41SupplyRollup_AbsentCursorIsNoCursorNotNoop pins that a pass folding nothing because the projector's
// sep41_supply cursor row is absent counts as outcome="no_cursor", never the dormant-token "noop", and WARNs once per
// contract.
func TestRunSEP41SupplyRollup_AbsentCursorIsNoCursorNotNoop(t *testing.T) {
	const cPinned = "CROLLUPNOCURSOR0000000000000000000000000000000000000000"
	fake := &fakeRollupAdvancer{
		results: map[string]timescale.SEP41RollupAdvance{
			cPinned: {ContractID: cPinned, Advanced: false, CursorAbsent: true},
		},
	}
	counter := func(outcome string) float64 {
		return testutil.ToFloat64(obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(cPinned, outcome))
	}
	beforeNoCursor, beforeNoop := counter("no_cursor"), counter("noop")

	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSEP41SupplyRollup(ctx, fake, []string{cPinned}, time.Millisecond,
			slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	const passes = 3
	waitForRollup(t, func() bool { return fake.callCount() >= passes })
	cancel()
	<-done

	calls := float64(fake.callCount())
	if got := counter("no_cursor") - beforeNoCursor; got != calls {
		t.Errorf("no_cursor outcomes = %v over %v passes; want one per pass", got, calls)
	}
	if got := counter("noop") - beforeNoop; got != 0 {
		t.Errorf("noop outcomes = %v; an absent cursor must not report as steady-state noop", got)
	}
	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Errorf("WARN lines = %d over %v passes; want exactly 1 (first occurrence per contract)\n%s", got, calls, logs.String())
	}
}

func TestSEP41RollupOutcome(t *testing.T) {
	cases := []struct {
		name string
		res  timescale.SEP41RollupAdvance
		err  error
		want string
	}{
		{"advanced", timescale.SEP41RollupAdvance{Advanced: true}, nil, "ok"},
		{"steady-state", timescale.SEP41RollupAdvance{}, nil, "noop"},
		{"absent cursor", timescale.SEP41RollupAdvance{CursorAbsent: true}, nil, "no_cursor"},
		{"error wins", timescale.SEP41RollupAdvance{CursorAbsent: true}, errors.New("boom"), "error"},
	}
	for _, tc := range cases {
		if got := sep41RollupOutcome(tc.res, tc.err); got != tc.want {
			t.Errorf("%s: outcome = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// TestBuildSupplyPolicy_TranslatesConfig: the refresher builders must not hardcode supply.Policy{}, or configured
// per_asset_locked_sets / max_supply_overrides never reach the computer. Asserts the PerAsset entries and
// MaxSupplyOverride lookup.
func TestBuildSupplyPolicy_TranslatesConfig(t *testing.T) {
	// Dash form, as watched_classic_assets spells it: the computers
	// look entries up by the colon supply.AssetKey, so the built Policy
	// must carry the colon key or the override silently never applies.
	const (
		usdcDash  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcColon = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	cfg := config.SupplyConfig{
		PerAssetLockedSets: map[string]config.SupplyLockedSetConfig{
			usdcDash: {
				Accounts:  []string{"GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"},
				Contracts: nil,
			},
		},
		MaxSupplyOverrides: map[string]string{
			usdcDash: "1000000000000",
		},
	}

	policy, err := buildSupplyPolicy(cfg)
	if err != nil {
		t.Fatalf("buildSupplyPolicy: %v", err)
	}

	// Deliberately excluded (see buildSupplyPolicy): ClassicComputer/SEP41Computer never consult SDFReserveAccounts.
	if policy.SDFReserveAccounts != nil {
		t.Errorf("SDFReserveAccounts = %v, want nil (Algorithm 1 wires it independently)", policy.SDFReserveAccounts)
	}

	locked, ok := policy.PerAsset[usdcColon]
	if !ok {
		t.Fatalf("policy.PerAsset[%q] missing (keys %v) — per_asset_locked_sets was not re-keyed to asset_key form", usdcColon, policy.PerAsset)
	}
	if len(locked.Accounts) != 1 || locked.Accounts[0] != "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4" {
		t.Errorf("policy.PerAsset[%q].Accounts = %v, want the one configured account", usdcColon, locked.Accounts)
	}

	override, ok, err := policy.MaxSupplyOverride(usdcColon)
	if err != nil {
		t.Fatalf("MaxSupplyOverride: %v", err)
	}
	if !ok {
		t.Fatalf("MaxSupplyOverride(%q) not found — max_supply_overrides was not re-keyed to asset_key form", usdcColon)
	}
	want := big.NewInt(1000000000000)
	if override.Cmp(want) != 0 {
		t.Errorf("MaxSupplyOverride(%q) = %s, want %s", usdcColon, override.String(), want.String())
	}
}

// TestBuildSupplyPolicy_RejectsXLMKey: only the classic and SEP-41
// computers read these maps, so an XLM entry would load and apply
// nothing; it must fail startup instead.
func TestBuildSupplyPolicy_RejectsXLMKey(t *testing.T) {
	for name, cfg := range map[string]config.SupplyConfig{
		"per_asset_locked_sets": {PerAssetLockedSets: map[string]config.SupplyLockedSetConfig{
			"XLM": {Accounts: []string{"GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"}},
		}},
		"max_supply_overrides": {MaxSupplyOverrides: map[string]string{"native": "1000000000000"}},
	} {
		if _, err := buildSupplyPolicy(cfg); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: buildSupplyPolicy(XLM key) err = %v, want an error naming %s", name, err, name)
		}
	}
}

// TestBuildSupplyPolicy_RejectsMalformedOverride confirms buildSupplyPolicy runs policy.Validate(), so a typo in
// max_supply_overrides fails the aggregator at startup rather than degrading at the first snapshot.
func TestBuildSupplyPolicy_RejectsMalformedOverride(t *testing.T) {
	cfg := config.SupplyConfig{
		MaxSupplyOverrides: map[string]string{
			"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "not-a-decimal-integer",
		},
	}
	if _, err := buildSupplyPolicy(cfg); err == nil || !strings.Contains(err.Error(), "decimal integer") {
		t.Fatalf("buildSupplyPolicy err = %v, want the non-decimal max_supply_overrides value rejected", err)
	}
}

// TestBuildSupplyPolicy_EmptyConfigIsValid — the common case (no
// operator overrides configured) must still produce a usable,
// validation-clean Policy equivalent to the zero supply.Policy{}.
func TestBuildSupplyPolicy_EmptyConfigIsValid(t *testing.T) {
	policy, err := buildSupplyPolicy(config.SupplyConfig{})
	if err != nil {
		t.Fatalf("buildSupplyPolicy(empty): %v", err)
	}
	if policy.SDFReserveAccounts != nil || policy.PerAsset != nil || policy.MaxSupplyOverrides != nil {
		t.Errorf("buildSupplyPolicy(empty) = %+v, want every field nil (matching the pre-fix supply.Policy{})", policy)
	}
}

// nopOracleReaderAgg satisfies divergence.OracleReader for wiring
// tests — never returns a row (the builder only needs non-nil).
type nopOracleReaderAgg struct{}

func (nopOracleReaderAgg) LatestOracleObservation(_ context.Context, _ string, _, _ []string) (*canonical.OracleUpdate, error) {
	return nil, nil
}

// TestBuildDivergenceReferences_AggregatorParity pins the production divergence wiring: the five on-chain references
// plus the synthetic USD-cross, which does not construct without an FX leg class.
func TestBuildDivergenceReferences_AggregatorParity(t *testing.T) {
	cfg := config.DivergenceConfig{
		Reflector: config.DivergenceOracleConfig{Enabled: true},
		Redstone:  config.DivergenceOracleConfig{Enabled: true},
		Band:      config.DivergenceOracleConfig{Enabled: true},
	}
	refs := buildDivergenceReferences(cfg, config.CoinGeckoVenueConfig{}, nopOracleReaderAgg{}, discardLogger())
	got := make(map[string]bool, len(refs))
	for _, r := range refs {
		got[r.Name()] = true
	}
	for _, want := range []string{
		divergence.OracleSourceReflectorDEX,
		divergence.OracleSourceReflectorCEX,
		divergence.OracleSourceReflectorFX,
		divergence.OracleSourceRedstone,
		divergence.OracleSourceBand,
		divergence.SyntheticCrossName,
	} {
		if !got[want] {
			t.Errorf("missing reference %q (got %v)", want, got)
		}
	}
	if len(refs) != 6 {
		t.Errorf("len(refs) = %d, want 6", len(refs))
	}

	// FX leg class absent → no synthetic.
	cfg = config.DivergenceConfig{
		Redstone: config.DivergenceOracleConfig{Enabled: true},
		Band:     config.DivergenceOracleConfig{Enabled: true},
	}
	for _, r := range buildDivergenceReferences(cfg, config.CoinGeckoVenueConfig{}, nopOracleReaderAgg{}, discardLogger()) {
		if r.Name() == divergence.SyntheticCrossName {
			t.Fatalf("synthetic cross constructed without an FX leg class")
		}
	}
}

// fakeCursorGetter substitutes for *timescale.Store in
// divergenceLedgerAdapter tests.
type fakeCursorGetter struct {
	cursor timescale.Cursor
	err    error
}

func (f fakeCursorGetter) GetCursor(_ context.Context, source, sub string) (timescale.Cursor, error) {
	if source != supplyChainCursorSource || sub != "" {
		return timescale.Cursor{}, errors.New("unexpected source/sub")
	}
	return f.cursor, f.err
}

// TestDivergenceLedgerAdapter_LatestLedger guards the seam: without it NewDivergenceSink has no ledger provider and
// every divergence_observations row carries observed_at_ledger=0. The adapter must surface the real ledgerstream
// cursor.
func TestDivergenceLedgerAdapter_LatestLedger(t *testing.T) {
	a := divergenceLedgerAdapter{cursors: fakeCursorGetter{
		cursor: timescale.Cursor{LastLedger: 55555555},
	}}
	if got := a.LatestLedger(); got != 55555555 {
		t.Fatalf("LatestLedger() = %d, want 55555555 (the live cursor, not the always-0 default)", got)
	}
}

// TestDivergenceLedgerAdapter_LatestLedger_FailsOpenOnError covers
// the cold-start / transient-failure path: a cursor read error must
// degrade to the documented 0 sentinel, never propagate or panic.
func TestDivergenceLedgerAdapter_LatestLedger_FailsOpenOnError(t *testing.T) {
	a := divergenceLedgerAdapter{cursors: fakeCursorGetter{
		err: timescale.ErrNotFound,
	}}
	if got := a.LatestLedger(); got != 0 {
		t.Fatalf("LatestLedger() = %d, want 0 on cursor-read error", got)
	}
}

// TestLazyCloseTimeReader_RetriesTransientDialFailure: the close-time reader must not dial once synchronously at boot,
// or a cold ClickHouse aborts the aggregator. A stub failing twice then succeeding proves newLazyCloseTimeReader
// retries in the background.
func TestLazyCloseTimeReader_RetriesTransientDialFailure(t *testing.T) {
	origMin, origMax := decimalsResolverRetryMin, decimalsResolverRetryMax
	decimalsResolverRetryMin = time.Millisecond
	decimalsResolverRetryMax = 5 * time.Millisecond
	t.Cleanup(func() { decimalsResolverRetryMin, decimalsResolverRetryMax = origMin, origMax })

	var attempts int32
	dial := func(ctx context.Context, addr string) (*clickhouse.ExplorerReader, error) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			return nil, errors.New("transient dial failure")
		}
		return &clickhouse.ExplorerReader{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newLazyCloseTimeReader(ctx, "fake-addr:9000", logger, dial)

	select {
	case <-r.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("lazyCloseTimeReader never became ready — the retry loop must not hang")
	}
	if got := atomic.LoadInt32(&attempts); got < 3 {
		t.Fatalf("dial attempts = %d, want >= 3: a transient failure must retry, not abort after the first attempt", got)
	}
	if r.reader == nil {
		t.Fatal("reader not armed after a successful retry — supply-refresh ticks would keep erroring even though the lake answered")
	}
}

// TestLazyCloseTimeReader_LatestLedgerAtOrBefore_UnblocksOnShutdown proves a refresh tick before the lake answers
// returns once the caller's ctx is done instead of blocking on a dial that never succeeds.
func TestLazyCloseTimeReader_LatestLedgerAtOrBefore_UnblocksOnShutdown(t *testing.T) {
	blockDial := make(chan struct{})
	dial := func(ctx context.Context, addr string) (*clickhouse.ExplorerReader, error) {
		<-blockDial
		return nil, errors.New("unreachable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newLazyCloseTimeReader(ctx, "fake-addr:9000", logger, dial)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.LatestLedgerAtOrBefore(callCtx, 100)
		done <- err
	}()

	cancel() // shutdown before the lake ever answered
	close(blockDial)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error once the caller's ctx is canceled before the dial succeeds")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LatestLedgerAtOrBefore blocked past shutdown instead of returning")
	}
}

// TestRunHoldsInstanceLock is a source-level tripwire: run() must take the Postgres instance lock before starting any
// goroutine and surface a lost lock in its exit error, or a second aggregator double-writes prices, freezes and
// alerts.
func TestRunHoldsInstanceLock(t *testing.T) {
	astguard.RunHoldsInstanceLock(t, "main.go", "AggregatorInstanceLockName")
}

type fakeSchemaReader struct{ version uint }

func (f fakeSchemaReader) SchemaMigrationVersion(context.Context) (uint, bool, error) {
	return f.version, false, nil
}

// TestMetricsMuxReadyzGatesOnSchemaHead pins that the deploy gate
// probes the aggregator's /readyz, which must 503 when the applied schema is
// behind the binary, while /healthz stays a constant liveness 200.
func TestMetricsMuxReadyzGatesOnSchemaHead(t *testing.T) {
	cases := []struct {
		name    string
		applied uint
		want    int
	}{
		{"schema behind binary", v1.ExpectedSchemaVersion - 1, http.StatusServiceUnavailable},
		{"schema at binary head", v1.ExpectedSchemaVersion, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := newMetricsMux(fakeSchemaReader{version: tc.applied})

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && !strings.Contains(rec.Body.String(), "schema") {
				t.Fatalf("/readyz body %q does not name the failed schema check", rec.Body.String())
			}

			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/healthz = %d, want 200 regardless of schema", rec.Code)
			}
		})
	}
}

// The single-host port-collision shift must key off whether the operator set obs.metrics_listen (MetricsListenSet),
// not equality with the indexer's default: an explicit 127.0.0.1:9464 must be honoured verbatim, not rewritten to
// :9465.
func TestMetricsListenAddr_ExplicitDefaultIsHonoured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.ObsConfig{
		MetricsListen:    aggregatorMetricsCollidingDefault,
		MetricsListenSet: true,
	}

	got := metricsListenAddr(cfg, logger)

	if got != aggregatorMetricsCollidingDefault {
		t.Fatalf("metricsListenAddr shifted an explicitly-configured value: got %q, want %q",
			got, aggregatorMetricsCollidingDefault)
	}
}

// Unset (left at Default()'s value) still gets the single-host shift.
func TestMetricsListenAddr_UnsetDefaultShifts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.ObsConfig{
		MetricsListen:    aggregatorMetricsCollidingDefault,
		MetricsListenSet: false,
	}

	got := metricsListenAddr(cfg, logger)

	if got != aggregatorMetricsShiftedAddr {
		t.Fatalf("metricsListenAddr did not shift an unset default: got %q, want %q",
			got, aggregatorMetricsShiftedAddr)
	}
}

// TestBackgroundWorkersRecover is an AST guard: every detached goroutine must register panic recovery, since one
// unrecovered panic kills the process. The worker set is derived from source (internal/worker/guardscan). Only the
// metrics listener is exempt, matched by content (ListenAndServe/Serve).
func TestBackgroundWorkersRecover(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"worker.Recover", "worker.Report"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	var checked, exempt int
	for _, s := range sites {
		if s.Kind == guardscan.KindUnresolved {
			t.Errorf("go %s at main.go:%d could not be resolved to a function body (%s). "+
				"Wrap it in `go func(){ defer worker.Recover(logger, \"<name>\"); … }()` "+
				"so the guard is visible where the goroutine starts.", s.Target, s.Line, s.Reason)
			continue
		}

		if s.Calls("ListenAndServe") || s.Calls("Serve") {
			// The metrics-listener goroutine — see the doc comment. Assert
			// it is genuinely the listener rather than trusting a position.
			exempt++
			if s.Recovers {
				t.Errorf("go func() at main.go:%d serves HTTP but registers "+
					"worker.Recover — recovering the listener leaves a live "+
					"process with nothing listening, which is worse than crashing", s.Line)
			}
			continue
		}

		checked++
		if !s.Recovers {
			t.Errorf("detached goroutine at main.go:%d (go %s, body at %s) does not "+
				"`defer worker.Recover(logger, \"<name>\")`. An unrecovered "+
				"panic in a goroutine terminates the WHOLE process, crash-looping "+
				"the aggregator over a single non-serving background worker.",
				s.Line, s.Target, s.Origin)
		}
	}

	// Guard against the guard covering nothing (e.g. the spawn idiom
	// changes and the AST match stops finding anything). 14 detached
	// workers exist today; this is a floor, not an exact count.
	if checked < 14 {
		t.Errorf("only %d background goroutine(s) discovered, expected at least 14 — "+
			"the discovery in this test has drifted from the code and is no longer "+
			"protecting anything", checked)
	}
	if exempt != 1 {
		t.Errorf("found %d HTTP-listener goroutine(s), want exactly 1 — if the metrics "+
			"listener was restructured, re-check that the exemption still applies to it alone", exempt)
	}
}
