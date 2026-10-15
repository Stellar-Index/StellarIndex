package pipeline

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/chainlink_stellar"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
)

// realisticGStrkey is a syntactically-valid 56-char G-prefixed strkey fixture; accounts.NewObserver only checks non-
// empty, and strict validation happens upstream at config.Validate.
const realisticGStrkey = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty pins the safe default: no supply opt-in means no registration and no error.
func TestRegisterSupplyEntryDecoders_AccountsNoOpWhenEmpty(t *testing.T) {
	disp := dispatcher.New()
	registered, err := RegisterSupplyEntryDecoders(disp, config.SupplyConfig{}, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("registered = %v, want empty (no watched accounts)", registered)
	}
}

// TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched: the accounts observer attaches when SDFReserveAccounts is non-empty.
func TestRegisterSupplyEntryDecoders_AccountsRegistersWhenWatched(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 {
		t.Fatalf("registered = %v, want exactly 1 (accounts)", registered)
	}
	if registered[0] != "accounts" {
		t.Errorf("registered[0] = %q, want 'accounts'", registered[0])
	}
}

// TestRegisterSupplyEntryDecoders_AccountsRegistersForIssuersOnly pins that issuer home_domain observation is
// decoupled from the SDF reserve list: watching an issuer must not require sdf_reserve_accounts, which would subtract
// its XLM from supply.
func TestRegisterSupplyEntryDecoders_AccountsRegistersForIssuersOnly(t *testing.T) {
	disp := dispatcher.New()
	meta := config.MetadataConfig{WatchedIssuerAccounts: []string{realisticGStrkey}}
	registered, err := RegisterSupplyEntryDecoders(disp, config.SupplyConfig{}, meta)
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 || registered[0] != "accounts" {
		t.Fatalf("registered = %v, want [accounts]", registered)
	}
}

func TestAccountObserverWatchSet_UnionDeduped(t *testing.T) {
	const issuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	sup := config.SupplyConfig{SDFReserveAccounts: []string{realisticGStrkey}}
	meta := config.MetadataConfig{WatchedIssuerAccounts: []string{issuer, realisticGStrkey}}
	got := AccountObserverWatchSet(sup, meta)
	want := []string{realisticGStrkey, issuer}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AccountObserverWatchSet = %v, want %v", got, want)
	}
	if len(sup.SDFReserveAccounts) != 1 {
		t.Fatalf("SDFReserveAccounts mutated to %v; issuers must never join the reserve list", sup.SDFReserveAccounts)
	}
}

// TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey: an empty string in SDFReserveAccounts must fail at registration, not mid-ingest.
func TestRegisterSupplyEntryDecoders_RejectsEmptyStrkey(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts: []string{realisticGStrkey, ""},
	}
	_, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err == nil {
		t.Fatal("expected error for empty G-strkey in watched-accounts list")
	}
}

// TestRegisterSupplyEntryDecoders_ClassicTrioRegisters: trustlines, claimable_balances and liquidity_pools all attach together, since Algorithm 2 sums all three per asset.
func TestRegisterSupplyEntryDecoders_ClassicTrioRegisters(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		WatchedClassicAssets: []string{"USDC-" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	wantSet := map[string]bool{
		"trustlines":         true,
		"claimable_balances": true,
		"liquidity_pools":    true,
	}
	gotSet := map[string]bool{}
	for _, name := range registered {
		gotSet[name] = true
	}
	for name := range wantSet {
		if !gotSet[name] {
			t.Errorf("registered set missing %q (got %v)", name, registered)
		}
	}
}

// TestRegisterSupplyEntryDecoders_SACRegistersWhenWrappers: the SAC observer attaches from sac_wrappers alone, independent of WatchedClassicAssets.
func TestRegisterSupplyEntryDecoders_SACRegistersWhenWrappers(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SACWrappers: map[string]string{realisticCStrkey: "USDC:" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 1 || registered[0] != "sac_balances" {
		t.Errorf("registered = %v, want [sac_balances]", registered)
	}
}

// TestRegisterSupplyEntryDecoders_FullConfigRegistersFive: with every watched set populated, every
// LedgerEntryChangeDecoder is registered. sep41_supply is an event-stream Decoder, outside this helper.
func TestRegisterSupplyEntryDecoders_FullConfigRegistersFive(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		SDFReserveAccounts:   []string{realisticGStrkey},
		WatchedClassicAssets: []string{"USDC-" + realisticGStrkey},
		SACWrappers:          map[string]string{realisticCStrkey: "USDC:" + realisticGStrkey},
	}
	registered, err := RegisterSupplyEntryDecoders(disp, cfg, config.MetadataConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEntryDecoders: %v", err)
	}
	if len(registered) != 5 {
		t.Errorf("registered count = %d, want 5 (accounts + trustlines + claimable + lp + sac); got %v",
			len(registered), registered)
	}
}

// realisticCStrkey is a 56-char C-prefixed Soroban contract id.
// Mirrors the supply-package test fixture pattern. Public ledger
// entry — not a credential.
const realisticCStrkey = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUOZWS4HG3B5UPHHC2QQA"

// TestRegisterSupplyEventDecoders_NoOpWhenEmpty pins the safe default: empty WatchedSEP41Contracts attaches no
// decoder.
func TestRegisterSupplyEventDecoders_NoOpWhenEmpty(t *testing.T) {
	disp := dispatcher.New()
	registered, err := RegisterSupplyEventDecoders(disp, config.SupplyConfig{})
	if err != nil {
		t.Fatalf("RegisterSupplyEventDecoders: %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("registered = %v, want empty", registered)
	}
}

// TestRegisterSupplyEventDecoders_RegistersWhenWatched: sep41_supply and sep41_transfers attach when WatchedSEP41Contracts is set.
func TestRegisterSupplyEventDecoders_RegistersWhenWatched(t *testing.T) {
	disp := dispatcher.New()
	cfg := config.SupplyConfig{
		WatchedSEP41Contracts: []string{realisticCStrkey},
	}
	registered, err := RegisterSupplyEventDecoders(disp, cfg)
	if err != nil {
		t.Fatalf("RegisterSupplyEventDecoders: %v", err)
	}
	want := []string{"sep41_supply", "sep41_transfers"}
	if len(registered) != len(want) {
		t.Fatalf("registered = %v, want %v", registered, want)
	}
	for i, w := range want {
		if registered[i] != w {
			t.Errorf("registered[%d] = %q, want %q", i, registered[i], w)
		}
	}
}

// oracleSourceFixture describes how to enable one oracle source and
// what cadence it declares.
type oracleSourceFixture struct {
	oracle             config.OracleConfig
	resolutionSeconds  float64
	enabledSourceNames []string
}

// oracleSourceFixtures maps each source name config.OracleSourceNames accepts to the config enabling it, so a source missing from BuildDispatcher fails with "unhandled source".
func oracleSourceFixtures() map[string]oracleSourceFixture {
	return map[string]oracleSourceFixture{
		reflector.SourceDEX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{DEXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceDEX},
		},
		reflector.SourceCEX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceCEX},
		},
		reflector.SourceFX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{FXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceFX},
		},
		redstone.SourceName: {
			oracle:             config.OracleConfig{Redstone: config.RedstoneOracleConfig{AdapterContract: realisticCStrkey}},
			resolutionSeconds:  redstone.DefaultResolutionSeconds,
			enabledSourceNames: []string{redstone.SourceName},
		},
		chainlink_stellar.SourceName: {
			oracle:             config.OracleConfig{ChainlinkStellar: config.ChainlinkStellarOracleConfig{CacheContract: realisticCStrkey}},
			resolutionSeconds:  chainlink_stellar.DefaultResolutionSeconds,
			enabledSourceNames: []string{chainlink_stellar.SourceName},
		},
		band.SourceName: {
			oracle:             config.OracleConfig{Band: config.BandOracleConfig{StandardReferenceContract: realisticCStrkey}},
			resolutionSeconds:  band.DefaultResolutionSeconds,
			enabledSourceNames: []string{band.SourceName},
		},
	}
}

// TestBuildDispatcher_DeclaresBudgetForEveryOracleSource: a source that never declared its resolution gets a +Inf budget that can never ticket,
// so every enabled oracle source must come out of BuildDispatcher with a finite budget equal to the alert's 10 x resolution.
func TestBuildDispatcher_DeclaresBudgetForEveryOracleSource(t *testing.T) {
	fixtures := oracleSourceFixtures()
	for source := range config.OracleSourceNames {
		if _, onChain := config.KnownSources[source]; !onChain {
			continue // a poller: TestExternalRun_DeclaresBudgetForEveryPolledOracleSource
		}
		fx, ok := fixtures[source]
		if !ok {
			t.Errorf("unhandled source %q: config.OracleSourceNames accepts staleness "+
				"overrides for it, so this test needs a fixture that enables it — "+
				"and BuildDispatcher needs an obs.DeclareOracleResolution call, or "+
				"every asset on it publishes a +Inf budget and can never ticket", source)
			continue
		}

		t.Run(source, func(t *testing.T) {
			if _, err := BuildDispatcher(fx.enabledSourceNames, fx.oracle, nil); err != nil {
				t.Fatalf("BuildDispatcher(%v): %v", fx.enabledSourceNames, err)
			}

			got := obs.OracleStalenessBudget(source, "crypto:XLM")
			if math.IsInf(got, 1) {
				t.Fatalf("budget for %q is +Inf — BuildDispatcher did not declare its "+
					"resolution, so stellarindex_oracle_stale can never fire for any "+
					"of its assets", source)
			}
			want := obs.OracleStaleBudgetMultiplier * fx.resolutionSeconds
			if source == redstone.SourceName || source == chainlink_stellar.SourceName {
				want = fx.resolutionSeconds + obs.OracleHeartbeatGrace
			}
			if got != want {
				t.Errorf("budget for %q = %v, want %v (declared resolution %v)",
					source, got, want, fx.resolutionSeconds)
			}
			if reg := external.Lookup(source).OracleResolution.Seconds(); reg != fx.resolutionSeconds {
				t.Errorf("external.Registry[%q].OracleResolution = %vs, but the dispatcher "+
					"declares %vs — the registry is the oracle-source authority and must "+
					"state the same cadence", source, reg, fx.resolutionSeconds)
			}
		})
	}
}

// budgetStubPoller is the smallest external.Poller: it names a source
// and a cadence and emits nothing, so external.Run's own wiring is the
// only thing that can give the source a budget.
type budgetStubPoller struct {
	name     string
	interval time.Duration
}

func (p budgetStubPoller) Name() string                { return p.name }
func (p budgetStubPoller) Class() external.Class       { return external.Lookup(p.name).Class }
func (p budgetStubPoller) PollInterval() time.Duration { return p.interval }
func (p budgetStubPoller) PollOnce(context.Context, []canonical.Pair) (
	[]canonical.Trade, []canonical.OracleUpdate, error,
) {
	return nil, nil, nil
}

// TestExternalRun_DeclaresBudgetForEveryPolledOracleSource is the poller-side twin: each must leave external.Run with a finite budget of 10 x its registry cadence (floored at the poll interval).
func TestExternalRun_DeclaresBudgetForEveryPolledOracleSource(t *testing.T) {
	const pollInterval = time.Minute
	polled := 0
	for source := range config.OracleSourceNames {
		if _, onChain := config.KnownSources[source]; onChain {
			continue
		}
		polled++
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			wait, err := external.Run(ctx, nil,
				[]external.PollerSpec{{Poller: budgetStubPoller{name: source, interval: pollInterval}}},
				make(chan consumer.Event, 1), nil)
			if err != nil {
				cancel()
				t.Fatalf("external.Run: %v", err)
			}
			cancel()
			wait()

			got := obs.OracleStalenessBudget(source, "crypto:XLM")
			if math.IsInf(got, 1) {
				t.Fatalf("budget for %q is +Inf — external.Run did not declare its "+
					"resolution, so stellarindex_oracle_stale can never fire for any "+
					"of its assets", source)
			}
			res := max(external.Lookup(source).OracleResolution, pollInterval)
			if want := obs.OracleStaleBudgetMultiplier * res.Seconds(); got != want {
				t.Errorf("budget for %q = %v, want %v (%d × %v)",
					source, got, want, obs.OracleStaleBudgetMultiplier, res)
			}
		})
	}
	if polled == 0 {
		t.Fatal("no polled oracle source in config.OracleSourceNames — this check " +
			"must not pass vacuously")
	}
}

// TestOracleSourceNames_MatchRegistry keeps config's hand-mirrored oracle source set in lockstep with the external registry (config cannot import external).
func TestOracleSourceNames_MatchRegistry(t *testing.T) {
	fromRegistry := map[string]struct{}{}
	for name, m := range external.Registry {
		if m.OracleResolution > 0 {
			fromRegistry[name] = struct{}{}
		}
	}
	for name := range fromRegistry {
		if _, ok := config.OracleSourceNames[name]; !ok {
			t.Errorf("external.Registry[%q] declares an OracleResolution but "+
				"config.OracleSourceNames omits it — no staleness override can name it", name)
		}
	}
	for name := range config.OracleSourceNames {
		if _, ok := fromRegistry[name]; !ok {
			t.Errorf("config.OracleSourceNames has %q but external.Registry declares "+
				"no OracleResolution for it", name)
		}
	}
}

// TestBuildDispatcher_InstallsConfiguredOverrides: configured staleness overrides reach the gauge, even for a source this replica does not run (inert if unmatched).
func TestBuildDispatcher_InstallsConfiguredOverrides(t *testing.T) {
	oracle := config.OracleConfig{
		Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey},
		StalenessOverrides: []config.OracleStalenessOverrideConfig{
			{
				Source:        reflector.SourceCEX,
				Asset:         "crypto:DAI",
				BudgetSeconds: 32400,
				Reason:        "peg asset — publishes only on movement",
			},
		},
	}
	if _, err := BuildDispatcher([]string{reflector.SourceCEX}, oracle, nil); err != nil {
		t.Fatalf("BuildDispatcher: %v", err)
	}
	t.Cleanup(func() { obs.SetOracleStalenessOverrides(nil) })

	if got := obs.OracleStalenessBudget(reflector.SourceCEX, "crypto:DAI"); got != 32400 {
		t.Errorf("overridden budget = %v, want 32400", got)
	}
	// The sibling asset on the same source keeps the default — an
	// override widens one pair, not the feed.
	want := obs.OracleStaleBudgetMultiplier * float64(reflector.DefaultResolutionSeconds)
	if got := obs.OracleStalenessBudget(reflector.SourceCEX, "crypto:XLM"); got != want {
		t.Errorf("sibling budget = %v, want %v (unchanged default)", got, want)
	}
}

// TestBuildDispatcher_NoOverridesLeavesEveryAssetOnTheDefault is the no-behaviour-change assertion: with the shipped
// empty override list every oracle asset's budget equals the number the alert expression computed before the budget
// existed.
func TestBuildDispatcher_NoOverridesLeavesEveryAssetOnTheDefault(t *testing.T) {
	oracle := config.OracleConfig{
		Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey},
	}
	if _, err := BuildDispatcher([]string{reflector.SourceCEX}, oracle, nil); err != nil {
		t.Fatalf("BuildDispatcher: %v", err)
	}

	want := obs.OracleStaleBudgetMultiplier * float64(reflector.DefaultResolutionSeconds)
	for _, asset := range []string{"crypto:XLM", "crypto:DAI", "crypto:USDC", "raw:UNMAPPED"} {
		if got := obs.OracleStalenessBudget(reflector.SourceCEX, asset); got != want {
			t.Errorf("budget(%s, %s) = %v, want %v", reflector.SourceCEX, asset, got, want)
		}
	}
}

// TestBuildDispatcher_FoldsWhitespaceAndCaseInSourceNames: the dispatcher must normalise enabled_sources like config validation (lowercase + trim),
// or a name config accepted would hard-error here as an unknown source at boot.
func TestBuildDispatcher_FoldsWhitespaceAndCaseInSourceNames(t *testing.T) {
	disp, err := BuildDispatcher([]string{"  Comet  "}, config.OracleConfig{}, nil)
	if err != nil {
		t.Fatalf("BuildDispatcher(%q): %v", "  Comet  ", err)
	}
	ev := events.Event{
		ContractID: comet.MainnetBackstopPool,
		Topic:      []string{comet.TopicSymbolPool, comet.TopicSymbolSwap},
	}
	name, ok := disp.Recognize(ev)
	if !ok || name != comet.SourceName {
		t.Fatalf("Recognize() = (%q, %v), want (%q, true) — %q must register the comet decoder",
			name, ok, comet.SourceName, "  Comet  ")
	}
}
