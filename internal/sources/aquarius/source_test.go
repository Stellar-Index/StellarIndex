package aquarius

import (
	"encoding/base64"
	"errors"
	"math/big"
	"regexp"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
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

// eventsExcludedFromClassify are Event* constants deliberately NOT in classify()'s closed set.
// EventAddPool is the ROUTER's pool-registration topic, recognised by dispatcher_adapter.go's own
// topic check rather than kindByTopicSymbol/classify (see events.go's EventAddPool doc).
var eventsExcludedFromClassify = map[string]bool{
	"EventAddPool": true,
}

// packageEventConstants AST-parses every non-test .go file in this package directory and returns
// every top-level string constant named Event* (name -> value). Unlike a hand-copied list, a
// future Event* constant is picked up automatically.
func packageEventConstants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range astguard.PackageConsts(t, ".") {
		if c.IsString && eventConstNamePattern.MatchString(c.Name) {
			out[c.Name] = c.Value
		}
	}
	return out
}

// TestClassify_completenessVsUpstream is a forcing function: an Event* constant not wired into
// kindByTopicSymbol/classify() fails it. It AST-enumerates the package constants (packageEventConstants) and documents
// the closed set of aquarius topics.
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

// TestDecodeTrade_withFakeDecoders substitutes the package-level decoder hooks to exercise direction assignment
// without XDR; real_fixture_test.go covers the real codec.
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

func TestDecodeTrade_zeroAmountIsRecognizedNoOpSentinel(t *testing.T) {
	ev := zeroAmountTradeEvent()
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeTrade(&ev, closedAt)
	if !errors.Is(err, ErrZeroAmountTrade) {
		t.Fatalf("decodeTrade err = %v, want ErrZeroAmountTrade", err)
	}
	if errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("zero-amount trade must NOT classify as malformed: %v", err)
	}
}

// Negative amounts remain a hard schema violation — the no-op carve-out
// is for ZERO only.
func TestDecodeTrade_negativeAmountStillMalformed(t *testing.T) {
	ev := zeroAmountTradeEvent()
	// body (sold=-1, bought=1, fee=0).
	ev.Value = encodeTradeBody(t, big.NewInt(-1), big.NewInt(1), big.NewInt(0))
	closedAt := time.Date(2024, 9, 23, 11, 34, 58, 0, time.UTC)
	_, err := decodeTrade(&ev, closedAt)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("negative amount err = %v, want ErrMalformedPayload", err)
	}
	if errors.Is(err, ErrZeroAmountTrade) {
		t.Fatalf("negative amount must not be the zero-amount no-op: %v", err)
	}
}

func i128Val(n *big.Int) xdr.ScVal {
	hi, lo := splitBigInt128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func contractAddrVal(t *testing.T, c string) xdr.ScVal {
	t.Helper()
	sv := decodeB64ScVal(t, encodeContractAddrFromStrkey(t, c))
	return sv
}

func marshalB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func vecVal(elts ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(elts)
	pvec := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}
}

// claimAmountEvents builds the three Aquarius claim events whose i128 amount lands in a CHECK (amount >= 0) column.
func claimAmountEvents(t *testing.T, amount *big.Int) map[string]events.Event {
	t.Helper()
	token := makeContractStrkey(t, 0x11)
	user := makeAccountStrkey(t, 0x22)
	return map[string]events.Event{
		EventClaimReward: {
			Topic: []string{TopicSymbolClaimReward, encodeContractAddrFromStrkey(t, token), encodeAccountAddrFromStrkey(t, user)},
			Value: marshalB64(t, vecVal(i128Val(amount))),
		},
		EventGaugeClaim: {
			Topic: []string{TopicSymbolGaugeClaim, encodeAccountAddrFromStrkey(t, user)},
			Value: marshalB64(t, i128Val(amount)),
		},
		EventClaimProtocolFee: {
			Topic: []string{TopicSymbolClaimProtocolFee, encodeContractAddrFromStrkey(t, token)},
			Value: marshalB64(t, vecVal(contractAddrVal(t, makeContractStrkey(t, 0x33)), i128Val(amount))),
		},
	}
}

// decodeClaimAmount runs the production decode arm for kind and returns
// the decoded amount as a decimal string.
func decodeClaimAmount(t *testing.T, kind string, e events.Event) (string, error) {
	t.Helper()
	if got := classify(&e); got != kind {
		t.Fatalf("classify = %q, want %q", got, kind)
	}
	if kind == EventClaimProtocolFee {
		fe, err := decodeFee(&e, closedAtTest, kind)
		if err != nil {
			return "", err
		}
		return fe.Amount.String(), nil
	}
	rv, err := decodeRewardsEvent(&e, kind, closedAtTest)
	if err != nil {
		return "", err
	}
	if rv.Amount == nil {
		t.Fatalf("%s: Amount nil on a successful decode", kind)
	}
	return rv.Amount.String(), nil
}

// A negative i128 claim is a schema violation: the promoted Amount is
// documented always-non-negative and the served column CHECKs >= 0, so a
// decode that accepts it hands the sink a row it can only drop.
func TestClaimAmounts_negativeRejectedAtDecode(t *testing.T) {
	for _, amt := range []*big.Int{
		big.NewInt(-1),
		mustBig(t, "-170141183460469231731687303715884105728"), // i128 min
	} {
		for kind, e := range claimAmountEvents(t, amt) {
			got, err := decodeClaimAmount(t, kind, e)
			if !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("%s amount=%s: got (%q, %v), want ErrMalformedPayload", kind, amt, got, err)
			}
		}
	}
}

// Zero and the i128 max both satisfy the CHECK and must decode exactly.
func TestClaimAmounts_nonNegativeDecodeExactly(t *testing.T) {
	for _, amt := range []*big.Int{
		big.NewInt(0),
		mustBig(t, "170141183460469231731687303715884105727"), // i128 max
	} {
		for kind, e := range claimAmountEvents(t, amt) {
			got, err := decodeClaimAmount(t, kind, e)
			if err != nil {
				t.Fatalf("%s amount=%s: %v", kind, amt, err)
			}
			if got != amt.String() {
				t.Errorf("%s: amount = %s, want %s", kind, got, amt)
			}
		}
	}
}

// Real lake bytes (ledger 53,626,410): a trade from a registered pool with body (sold=2, bought=0, fee=0), a dust
// swap. It must decode as a recognised no-op (nil rows, nil error): an error blinds the projection reconcile, and a
// zero side breaks price derivation.
const (
	zeroAmountTradeContract = "CCY2PXGMKNQHO7WNYXEWX76L2C5BH3JUW3RCATGUYKY7QQTRILBZIFWV"
	zeroAmountTradeValue    = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAAAAAAIAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAAoAAAAAAAAAAAAAAAAAAAAA"
)

var zeroAmountTradeTopics = []string{
	"AAAADwAAAAV0cmFkZQAAAA==",
	"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
	"AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw==",
	"AAAAEgAAAAAAAAAA06CkWF2KJY6l5M8FwHOcDBTyiduhNO0HT1eyazNchIM=",
}

func zeroAmountTradeEvent() events.Event {
	return events.Event{
		Type:           "contract",
		Ledger:         53626410,
		LedgerClosedAt: "2024-09-23T11:34:58Z",
		ContractID:     zeroAmountTradeContract,
		TxHash:         "3870e2fc05a8a37f7ec7b01faaf222f91a353e8ca5273afe03c86b56963ba760",
		OperationIndex: 0,
		EventIndex:     2,
		Topic:          zeroAmountTradeTopics,
		Value:          zeroAmountTradeValue,
	}
}

func TestDecoderDecode_zeroAmountTradeIsNoOp(t *testing.T) {
	dec := NewDecoder()
	ev := zeroAmountTradeEvent()
	if !dec.Matches(ev) {
		t.Fatal("gated decoder must match the registered pool's trade event")
	}
	outs, err := dec.Decode(ev)
	if err != nil {
		t.Fatalf("Decode = %v, want recognized no-op (nil error)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode emitted %d events, want 0 (zero-amount trade projects nothing)", len(outs))
	}
}

// A self-pair trade (token_sold == token_bought) maps to zero rows (canonical.ErrPairMismatch), a recognised no-op as
// in comet; as an error it would count as undecodable-but-matched and fail the source's verdict closed.
func TestDecoderDecode_selfPairTradeIsNoOp(t *testing.T) {
	dec := NewDecoder()
	ev := zeroAmountTradeEvent()
	ev.Topic = append([]string(nil), zeroAmountTradeTopics...)
	ev.Topic[2] = ev.Topic[1]
	ev.Value = encodeTradeBody(t, big.NewInt(2), big.NewInt(1), big.NewInt(0))
	if !dec.Matches(ev) {
		t.Fatal("gated decoder must match the registered pool's trade event")
	}
	outs, err := dec.Decode(ev)
	if err != nil {
		t.Fatalf("Decode = %v, want recognized no-op (nil error)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode emitted %d events, want 0 (self-pair trade projects nothing)", len(outs))
	}
}
