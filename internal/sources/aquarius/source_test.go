package aquarius

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		topics []string
		want   string
	}{
		{"trade", []string{TopicSymbolTrade}, EventTrade},
		{"deposit_liquidity", []string{TopicSymbolDepositLiquidity}, EventDepositLiquidity},
		{"withdraw_liquidity", []string{TopicSymbolWithdrawLiquidity}, EventWithdrawLiquidity},
		{"update_reserves", []string{TopicSymbolUpdateReserves}, EventUpdateReserves},
		{"reserves_sync", []string{TopicSymbolReservesSync}, EventReservesSync},
		{"set_protocol_fee", []string{TopicSymbolSetProtocolFee}, EventSetProtocolFee},
		{"claim_protocol_fee", []string{TopicSymbolClaimProtocolFee}, EventClaimProtocolFee},
		{"kill_deposit", []string{TopicSymbolKillDeposit}, EventKillDeposit},
		{"unkill_deposit", []string{TopicSymbolUnkillDeposit}, EventUnkillDeposit},
		{"kill_swap", []string{TopicSymbolKillSwap}, EventKillSwap},
		{"unkill_swap", []string{TopicSymbolUnkillSwap}, EventUnkillSwap},
		{"kill_claim", []string{TopicSymbolKillClaim}, EventKillClaim},
		{"unkill_claim", []string{TopicSymbolUnkillClaim}, EventUnkillClaim},
		{"kill_gauges_claim", []string{TopicSymbolKillGaugesClaim}, EventKillGaugesClaim},
		{"unkill_gauges_claim", []string{TopicSymbolUnkillGaugesClaim}, EventUnkillGaugesClaim},
		{"pool_state", []string{TopicSymbolPoolState}, EventPoolState},
		{"claim_reward", []string{TopicSymbolClaimReward}, EventClaimReward},
		{"set_rewards_config", []string{TopicSymbolSetRewardsConfig}, EventSetRewardsConfig},
		{"position_update", []string{TopicSymbolPositionUpdate}, EventPositionUpdate},
		{"deposit (bare)", []string{TopicSymbolGaugeDeposit}, EventGaugeDeposit},
		{"claim_fees", []string{TopicSymbolClaimFees}, EventClaimFees},
		{"rewards_gauge_claim", []string{TopicSymbolRewardsGaugeClaim}, EventRewardsGaugeClaim},
		{"claim (bare)", []string{TopicSymbolGaugeClaim}, EventGaugeClaim},
		{"rewards_gauge_schedule_reward", []string{TopicSymbolRewardsGaugeScheduleReward}, EventRewardsGaugeScheduleReward},
		{"set_rewards_state", []string{TopicSymbolSetRewardsState}, EventSetRewardsState},
		{"rewards_gauge_add", []string{TopicSymbolRewardsGaugeAdd}, EventRewardsGaugeAdd},
		{"config_rewards", []string{TopicSymbolConfigRewards}, EventConfigRewards},
		{"apply_upgrade", []string{TopicSymbolApplyUpgrade}, EventApplyUpgrade},
		{"commit_upgrade", []string{TopicSymbolCommitUpgrade}, EventCommitUpgrade},
		{"set_privileged_addrs", []string{TopicSymbolSetPrivilegedAddrs}, EventSetPrivilegedAddrs},
		{"apply_transfer_ownership", []string{TopicSymbolApplyTransferOwnership}, EventApplyTransferOwnership},
		{"commit_transfer_ownership", []string{TopicSymbolCommitTransferOwnership}, EventCommitTransferOwnership},
		{"enable_emergency_mode", []string{TopicSymbolEnableEmergencyMode}, EventEnableEmergencyMode},
		{"disable_emergency_mode", []string{TopicSymbolDisableEmergencyMode}, EventDisableEmergencyMode},
		{"pool_gauge_switch_token", []string{TopicSymbolPoolGaugeSwitchToken}, EventPoolGaugeSwitchToken},
		{"unknown", []string{"AAAAsomething-else"}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &events.Event{Topic: tc.topics}
			if got := classify(e); got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.topics, got, tc.want)
			}
		})
	}
}

// eventConstNamePattern matches the package's Event* constant naming
// convention (events.go).
var eventConstNamePattern = regexp.MustCompile(`^Event[A-Z]`)

// eventsExcludedFromClassify are Event* constants that are
// deliberately NOT part of classify()'s closed set. EventAddPool is
// the ROUTER's pool-registration topic — recognised by
// dispatcher_adapter.go's own topic check, not routed through
// kindByTopicSymbol/classify (see events.go's EventAddPool doc).
var eventsExcludedFromClassify = map[string]bool{
	"EventAddPool": true,
}

// packageEventConstants AST-parses every non-test .go file in this
// package directory and returns every top-level string constant named
// Event* (name -> value). Unlike a hand-copied list, this reflects
// whatever events.go actually declares: a future Event* constant is
// picked up automatically, with no second list to remember to update.
func packageEventConstants(t *testing.T) map[string]string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !eventConstNamePattern.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", name.Name, err)
					}
					out[name.Name] = val
				}
			}
		}
	}
	return out
}

// TestClassify_completenessVsUpstream is a forcing function: if a
// future agent adds an Event* constant without also wiring it into
// kindByTopicSymbol/classify(), this test fails. It AST-enumerates
// every Event* constant declared in the package (see
// packageEventConstants) instead of a hand-maintained list, so the
// enumeration itself cannot drift out of sync with events.go. The
// test fixture also acts as documentation of the closed set of topics
// aquarius emits (verified against
// aquarius-amm/liquidity_pool_events/src/lib.rs).
func TestClassify_completenessVsUpstream(t *testing.T) {
	consts := packageEventConstants(t)
	if len(consts) == 0 {
		t.Fatal("packageEventConstants found no Event* constants — enumeration is broken")
	}
	for name, val := range consts {
		if eventsExcludedFromClassify[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			symbol := scval.MustEncodeSymbol(val)
			got := classify(&events.Event{Topic: []string{symbol}})
			if got != val {
				t.Fatalf("classify(symbol for %s=%q) = %q, want %q — wire %s into kindByTopicSymbol in decode.go", name, val, got, val, name)
			}
		})
	}
}

func TestPoolTypeString(t *testing.T) {
	cases := map[PoolType]string{
		PoolVolatile:     "volatile",
		PoolStableswap:   "stableswap",
		PoolConcentrated: "concentrated",
		PoolUnknown:      "unknown",
		PoolType(99):     "unknown",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", p, got, want)
		}
	}
}

// TestDecodeTrade_withFakeDecoders uses the package-level hook vars
// to substitute decoders for the topic + body SCVals. Exercises the
// full direction-assignment path without the real XDR codec —
// real_fixture_test.go covers that path against mainnet captures.
func TestDecodeTrade_withFakeDecoders(t *testing.T) {
	prevAmt, prevAsset, prevAddr := decodeTradeAmounts, decodeAssetTopic, decodeAddressTopic
	defer func() {
		decodeTradeAmounts, decodeAssetTopic, decodeAddressTopic = prevAmt, prevAsset, prevAddr
	}()

	usdc, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	xlm := canonical.NativeAsset()

	decodeAssetTopic = func(slot string) (canonical.Asset, error) {
		switch slot {
		case "token_in_slot":
			return xlm, nil
		case "token_out_slot":
			return usdc, nil
		}
		t.Fatalf("unexpected topic slot: %q", slot)
		return canonical.Asset{}, nil
	}
	decodeAddressTopic = func(slot string) (string, error) {
		if slot == "user_slot" {
			return "GTAKER", nil
		}
		return "", nil
	}
	decodeTradeAmounts = func(_ string) (tradeAmounts, error) {
		return tradeAmounts{
			SoldAmount:   canonical.NewAmount(big.NewInt(1_000_000_000)),
			BoughtAmount: canonical.NewAmount(big.NewInt(12_420_000)),
			Fee:          canonical.NewAmount(big.NewInt(0)),
		}, nil
	}

	e := &events.Event{
		Topic:          []string{TopicSymbolTrade, "token_in_slot", "token_out_slot", "user_slot"},
		Value:          "stub",
		Ledger:         100,
		TxHash:         "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OperationIndex: 7,
		EventIndex:     3,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
	}
	closedAt, _ := time.Parse(time.RFC3339, e.LedgerClosedAt)
	tr, err := decodeTrade(e, closedAt)
	if err != nil {
		t.Fatalf("decodeTrade: %v", err)
	}
	if !tr.Pair.Base.Equal(xlm) || !tr.Pair.Quote.Equal(usdc) {
		t.Errorf("wrong pair direction: %+v", tr.Pair)
	}
	if tr.BaseAmount.Cmp(canonical.NewAmount(big.NewInt(1_000_000_000))) != 0 {
		t.Errorf("base = %s", tr.BaseAmount)
	}
	if tr.QuoteAmount.Cmp(canonical.NewAmount(big.NewInt(12_420_000))) != 0 {
		t.Errorf("quote = %s", tr.QuoteAmount)
	}
	if tr.Taker != "GTAKER" {
		t.Errorf("taker = %q", tr.Taker)
	}
	if want := canonical.FanoutOpIndex(7, 3); tr.OpIndex != want {
		t.Errorf("op_index = %d, want %d (op 7 fanned out by event index 3)", tr.OpIndex, want)
	}
	if tr.Source != SourceName {
		t.Errorf("source = %q", tr.Source)
	}
}

func TestDecodeTrade_wrongTopicArity(t *testing.T) {
	// Only 3 topics — missing user slot. Surface ErrMalformedPayload.
	e := &events.Event{Topic: []string{TopicSymbolTrade, "t_in", "t_out"}}
	_, err := decodeTrade(e, time.Now())
	if err == nil {
		t.Fatal("expected error on 3-topic event")
	}
}

func TestDecodeTrade_nonPositiveAmount(t *testing.T) {
	prevAmt, prevAsset, prevAddr := decodeTradeAmounts, decodeAssetTopic, decodeAddressTopic
	defer func() {
		decodeTradeAmounts, decodeAssetTopic, decodeAddressTopic = prevAmt, prevAsset, prevAddr
	}()

	usdc, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	decodeAssetTopic = func(_ string) (canonical.Asset, error) { return usdc, nil }
	decodeAddressTopic = func(_ string) (string, error) { return "", nil }
	decodeTradeAmounts = func(_ string) (tradeAmounts, error) {
		return tradeAmounts{
			SoldAmount:   canonical.NewAmount(big.NewInt(0)),
			BoughtAmount: canonical.NewAmount(big.NewInt(42)),
			Fee:          canonical.NewAmount(big.NewInt(0)),
		}, nil
	}

	e := &events.Event{
		Topic: []string{TopicSymbolTrade, "a", "b", "c"},
	}
	_, err := decodeTrade(e, time.Now())
	if err == nil {
		t.Fatal("expected error on zero sold_amount")
	}
}

func TestDecoder_NameMatchesSourceName(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}
