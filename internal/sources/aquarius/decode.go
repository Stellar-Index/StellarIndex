package aquarius

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// aquariusTopicArity is the topic-count on every Aquarius trade
// event: [Symbol("trade"), Address(token_in), Address(token_out),
// Address(user)].
const aquariusTopicArity = 4

// kindByTopicSymbol maps the pre-encoded topic[0] SCVal::Symbol blob of every recognized Aquarius
// event to its event-kind name. Every topic a pool emits must appear here
// (TestClassify_completenessVsUpstream enumerates the closed set). The router's own `swap` /
// `deposit` / `withdraw` do not match: a documented known gap, pinned by
// TestRouterCensusTopics_matchedOrKnownGap.
var kindByTopicSymbol = map[string]string{
	TopicSymbolTrade:                      EventTrade,
	TopicSymbolDepositLiquidity:           EventDepositLiquidity,
	TopicSymbolWithdrawLiquidity:          EventWithdrawLiquidity,
	TopicSymbolUpdateReserves:             EventUpdateReserves,
	TopicSymbolReservesSync:               EventReservesSync,
	TopicSymbolSetProtocolFee:             EventSetProtocolFee,
	TopicSymbolClaimProtocolFee:           EventClaimProtocolFee,
	TopicSymbolKillDeposit:                EventKillDeposit,
	TopicSymbolUnkillDeposit:              EventUnkillDeposit,
	TopicSymbolKillSwap:                   EventKillSwap,
	TopicSymbolUnkillSwap:                 EventUnkillSwap,
	TopicSymbolKillClaim:                  EventKillClaim,
	TopicSymbolUnkillClaim:                EventUnkillClaim,
	TopicSymbolKillGaugesClaim:            EventKillGaugesClaim,
	TopicSymbolUnkillGaugesClaim:          EventUnkillGaugesClaim,
	TopicSymbolPoolState:                  EventPoolState,
	TopicSymbolClaimReward:                EventClaimReward,
	TopicSymbolSetRewardsConfig:           EventSetRewardsConfig,
	TopicSymbolPositionUpdate:             EventPositionUpdate,
	TopicSymbolGaugeDeposit:               EventGaugeDeposit,
	TopicSymbolClaimFees:                  EventClaimFees,
	TopicSymbolRewardsGaugeClaim:          EventRewardsGaugeClaim,
	TopicSymbolGaugeClaim:                 EventGaugeClaim,
	TopicSymbolRewardsGaugeScheduleReward: EventRewardsGaugeScheduleReward,
	TopicSymbolSetRewardsState:            EventSetRewardsState,
	TopicSymbolRewardsGaugeAdd:            EventRewardsGaugeAdd,
	TopicSymbolConfigRewards:              EventConfigRewards,
	TopicSymbolApplyUpgrade:               EventApplyUpgrade,
	TopicSymbolCommitUpgrade:              EventCommitUpgrade,
	TopicSymbolSetPrivilegedAddrs:         EventSetPrivilegedAddrs,
	TopicSymbolApplyTransferOwnership:     EventApplyTransferOwnership,
	TopicSymbolCommitTransferOwnership:    EventCommitTransferOwnership,
	TopicSymbolEnableEmergencyMode:        EventEnableEmergencyMode,
	TopicSymbolDisableEmergencyMode:       EventDisableEmergencyMode,
	TopicSymbolPoolGaugeSwitchToken:       EventPoolGaugeSwitchToken,
}

// classify picks the event kind from topic[0]. Returns "" for
// non-Aquarius events so the caller skips cheaply. See
// kindByTopicSymbol for the closed-set enumeration contract.
func classify(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	return kindByTopicSymbol[e.Topic[0]]
}

// decodeTrade decodes an Aquarius `trade` event into a single canonical.Trade. It needs NO
// pool-info cache: token identities are carried in the topics ("trade", token_in, token_out,
// user). The body is a tuple-shaped ScvVec [sold_amount, bought_amount, fee] of i128 (not a Map,
// which soroban-sdk uses only for named-field structs).
func decodeTrade(e *events.Event, closedAt time.Time) (canonical.Trade, error) {
	if len(e.Topic) != aquariusTopicArity {
		return canonical.Trade{}, fmt.Errorf("%w: expected %d topics, got %d",
			ErrMalformedPayload, aquariusTopicArity, len(e.Topic))
	}
	soldAsset, err := decodeAssetTopic(e.Topic[1])
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: token_in: %w", ErrMalformedPayload, err)
	}
	boughtAsset, err := decodeAssetTopic(e.Topic[2])
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: token_out: %w", ErrMalformedPayload, err)
	}
	userAddr, err := decodeAddressTopic(e.Topic[3])
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}

	amounts, err := decodeTradeAmounts(e.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}

	// NEGATIVE amounts are a schema violation and refused. ZERO amounts are not: genuine dust
	// swaps round one side to zero and the pool still emits the event. canonical.Trade forbids
	// non-positive amounts (a zero side breaks price derivation), so such a swap never becomes a
	// served row. It is a RECOGNIZED NO-OP (ErrZeroAmountTrade), not a malformed payload, so the
	// completeness re-derive sees expected == served == 0 instead of an undecodable event.
	if amounts.SoldAmount.Sign() < 0 || amounts.BoughtAmount.Sign() < 0 {
		return canonical.Trade{}, fmt.Errorf("%w: negative amounts sold=%s bought=%s",
			ErrMalformedPayload, amounts.SoldAmount, amounts.BoughtAmount)
	}
	if amounts.SoldAmount.Sign() == 0 || amounts.BoughtAmount.Sign() == 0 {
		return canonical.Trade{}, fmt.Errorf("%w: sold=%s bought=%s",
			ErrZeroAmountTrade, amounts.SoldAmount, amounts.BoughtAmount)
	}

	pair, err := canonical.NewPair(soldAsset, boughtAsset)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source: SourceName,
		Ledger: e.Ledger,
		TxHash: e.TxHash,
		// Fan out by event index: one op can emit several trade events
		// (multi-pool swap), which otherwise collide on the trades PK and
		// get dropped (ADR-0033 — confirmed via reconciliation: 5 events
		// → 2 rows at ledger 62848858).
		OpIndex:   canonical.FanoutOpIndex(e.OperationIndex, e.EventIndex),
		Timestamp: closedAt,
		Pair:      pair,
		// BaseAmount is sold_amount unmodified: every captured trade satisfies
		// fee == ceil(sold_amount * pool_fee_bps / 10000), so sold_amount is the taker's gross
		// input (TestTradeAmounts_feeIsGrossOfSoldAmount), matching Comet's convention.
		BaseAmount:  amounts.SoldAmount,
		QuoteAmount: amounts.BoughtAmount,
		Taker:       userAddr,
	}, nil
}

// decodeReserves decodes an Aquarius `update_reserves` or `reserves_sync` event, which share a
// `Vec<i128>` body. `kind` (the classify() result) is carried onto the ReservesEvent so the sink
// routes it to the right table. topic[0] is the only topic; the body is the pool's POST-STATE
// reserves in canonical token order, variable-length (2 for volatile, N for stableswap).
func decodeReserves(e *events.Event, closedAt time.Time, kind string) (ReservesEvent, error) {
	reserves, err := decodeAmountVec(e.Value)
	if err != nil {
		return ReservesEvent{}, fmt.Errorf("%w: %s body: %w", ErrMalformedPayload, kind, err)
	}
	if len(reserves) == 0 {
		return ReservesEvent{}, fmt.Errorf("%w: %s empty reserve vector", ErrMalformedPayload, kind)
	}
	for i, r := range reserves {
		if r.Sign() < 0 {
			return ReservesEvent{}, fmt.Errorf("%w: %s reserve[%d] negative: %s", ErrMalformedPayload, kind, i, r)
		}
	}
	return ReservesEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // OperationIndex is non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // EventIndex is non-negative by Soroban spec.
		ObservedAt: closedAt,
		Kind:       kind,
		Reserves:   reserves,
	}, nil
}

// decodeLiquidity decodes an Aquarius `deposit_liquidity` / `withdraw_liquidity` event into a
// LiquidityEvent. Wire shape (both events share it):
//
//	topics: [Symbol(action), Address(token_0), …, Address(token_{n-1})]
//	body:   Vec<i128> of length n+1 = [amount_0, …, amount_{n-1}, share_amount]
//
// n = len(topics) - 1 is the pool's token count (2 volatile, 3-4 stableswap); the trailing
// element is the LP-share amount minted (deposit) or burned (withdraw). Decoding by the
// (topic-count, body-length) relationship, not a fixed 2-token layout, keeps N-token stableswap
// events from being dropped.
func decodeLiquidity(e *events.Event, action LiquidityAction, closedAt time.Time) (LiquidityEvent, error) {
	if len(e.Topic) < 2 {
		return LiquidityEvent{}, fmt.Errorf("%w: %s needs >=2 topics (symbol + >=1 token), got %d",
			ErrMalformedPayload, action, len(e.Topic))
	}
	nTokens := len(e.Topic) - 1
	tokens := make([]string, nTokens)
	for i := 0; i < nTokens; i++ {
		addr, err := decodeAddressTopic(e.Topic[i+1])
		if err != nil {
			return LiquidityEvent{}, fmt.Errorf("%w: %s token[%d]: %w", ErrMalformedPayload, action, i, err)
		}
		tokens[i] = addr
	}

	vals, err := decodeAmountVec(e.Value)
	if err != nil {
		return LiquidityEvent{}, fmt.Errorf("%w: %s body: %w", ErrMalformedPayload, action, err)
	}
	// n token amounts + 1 trailing share amount.
	if len(vals) != nTokens+1 {
		return LiquidityEvent{}, fmt.Errorf("%w: %s body length %d != %d tokens + 1 (shares)",
			ErrMalformedPayload, action, len(vals), nTokens)
	}
	amounts := vals[:nTokens]
	shares := vals[nTokens]
	for i, a := range amounts {
		if a.Sign() < 0 {
			return LiquidityEvent{}, fmt.Errorf("%w: %s amount[%d] negative: %s", ErrMalformedPayload, action, i, a)
		}
	}
	if shares.Sign() < 0 {
		return LiquidityEvent{}, fmt.Errorf("%w: %s shares negative: %s", ErrMalformedPayload, action, shares)
	}

	return LiquidityEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // OperationIndex is non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // EventIndex is non-negative by Soroban spec.
		ObservedAt: closedAt,
		Action:     action,
		Tokens:     tokens,
		Amounts:    amounts,
		Shares:     shares,
	}, nil
}

// tradeAmounts holds the three i128 values from a trade body.
type tradeAmounts struct {
	SoldAmount   canonical.Amount
	BoughtAmount canonical.Amount
	Fee          canonical.Amount
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.

var (
	decodeTradeAmounts = sdkDecodeTradeAmounts
	decodeAssetTopic   = sdkDecodeAssetTopic
	decodeAddressTopic = sdkDecodeAddressTopic
	decodeAmountVec    = sdkDecodeAmountVec
)

// sdkDecodeAmountVec unpacks a variable-length Vec of i128 (the update_reserves vector and the
// deposit/withdraw [amounts..., shares] vector), one element per pool token (+1 for shares).
// Every element MUST be an i128 (ADR-0003); a non-i128 element is a schema violation rejected
// rather than truncated.
func sdkDecodeAmountVec(valueB64 string) ([]canonical.Amount, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return nil, fmt.Errorf("body not a vec: %w", err)
	}
	out := make([]canonical.Amount, len(elts))
	for i, el := range elts {
		amt, err := scval.AsAmountFromI128(el)
		if err != nil {
			return nil, fmt.Errorf("element %d not i128: %w", i, err)
		}
		out[i] = amt
	}
	return out, nil
}

// sdkDecodeTradeAmounts unpacks the body Vec of 3 i128s (sold, bought, fee). The tuple body has
// no field names, so arity is the only signal of a future contract upgrade changing its shape.
func sdkDecodeTradeAmounts(valueB64 string) (tradeAmounts, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return tradeAmounts{}, fmt.Errorf("parse body: %w", err)
	}
	elts, err := scval.AsTupleN(body, 3)
	if err != nil {
		return tradeAmounts{}, fmt.Errorf("body not a 3-tuple: %w", err)
	}
	sold, err := scval.AsAmountFromI128(elts[0])
	if err != nil {
		return tradeAmounts{}, fmt.Errorf("sold_amount: %w", err)
	}
	bought, err := scval.AsAmountFromI128(elts[1])
	if err != nil {
		return tradeAmounts{}, fmt.Errorf("bought_amount: %w", err)
	}
	fee, err := scval.AsAmountFromI128(elts[2])
	if err != nil {
		return tradeAmounts{}, fmt.Errorf("fee: %w", err)
	}
	return tradeAmounts{SoldAmount: sold, BoughtAmount: bought, Fee: fee}, nil
}

// sdkDecodeAssetTopic converts a topic-slot Address into a
// canonical.Asset. Aquarius only lists Soroban tokens (SAC-wrapped
// or contract-deployed), never symbolic/fiat references, so the
// conversion is unconditional Soroban.
func sdkDecodeAssetTopic(topicB64 string) (canonical.Asset, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return canonical.Asset{}, fmt.Errorf("parse topic: %w", err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return canonical.Asset{}, err
	}
	return canonical.NewSorobanAsset(addr)
}

// sdkDecodeAddressTopic decodes a topic-slot Address into its
// strkey form. Used for the trader slot — may be a G-strkey (user
// account) or C-strkey (router/contract).
func sdkDecodeAddressTopic(topicB64 string) (string, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return "", fmt.Errorf("parse topic: %w", err)
	}
	return scval.AsAddressStrkey(sv)
}

// decodeAnnouncedPool extracts the pool address a ROUTER `add_pool` event announces (the
// ADR-0035/0040 fan-out seam). Router pool-scoped events carry body `Vec[Address(pool), …]`; the
// address must be a contract (C-strkey), anything else is malformed.
func decodeAnnouncedPool(e *events.Event) (string, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return "", fmt.Errorf("%w: add_pool body: %w", ErrMalformedPayload, err)
	}
	elts, err := scval.AsVec(body)
	if err != nil {
		return "", fmt.Errorf("%w: add_pool body not a vec: %w", ErrMalformedPayload, err)
	}
	if len(elts) == 0 {
		return "", fmt.Errorf("%w: add_pool body vec is empty", ErrMalformedPayload)
	}
	pool, err := scval.AsAddressStrkey(elts[0])
	if err != nil {
		return "", fmt.Errorf("%w: add_pool pool address: %w", ErrMalformedPayload, err)
	}
	if len(pool) == 0 || pool[0] != 'C' {
		return "", fmt.Errorf("%w: add_pool announced a non-contract address %q", ErrMalformedPayload, pool)
	}
	return pool, nil
}

// decodeFee decodes an Aquarius `set_protocol_fee` or `claim_protocol_fee` treasury event into a
// FeeEvent. `kind` (the classify() result) selects the decode:
//
//	set_protocol_fee   - two real body shapes, both handled by decodeSetProtocolFee:
//	                     Map[ fee_protocol{0,1}_{new,old}: u32 ] and Vec[ u32 ]
//	claim_protocol_fee - Vec[ recipient: Address, amount: i128 ]
//
// The Map is decoded by field name (schema-evolution safe); the Vecs are positional.
func decodeFee(e *events.Event, closedAt time.Time, kind string) (FeeEvent, error) {
	fe := FeeEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // OperationIndex is non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // EventIndex is non-negative by Soroban spec.
		ObservedAt: closedAt,
		Kind:       kind,
	}
	sv, err := scval.Parse(e.Value)
	if err != nil {
		return FeeEvent{}, fmt.Errorf("%w: %s body: %w", ErrMalformedPayload, kind, err)
	}
	switch kind {
	case EventSetProtocolFee:
		if err := decodeSetProtocolFee(sv, &fe); err != nil {
			return FeeEvent{}, err
		}
	case EventClaimProtocolFee:
		if err := decodeClaimFee(e, sv, &fe); err != nil {
			return FeeEvent{}, err
		}
	default:
		return FeeEvent{}, fmt.Errorf("decodeFee: unexpected kind %q", kind)
	}
	return fe, nil
}

// decodeSetProtocolFee fills the set_protocol_fee fields from EITHER real body shape
// (branch on the parsed SCVal kind):
//
//	Map[ fee_protocol{0,1}_{new,old}: u32 ]
//	    per-token old->new transition, decoded by field name. Only non-registered contracts
//	    emit it, so identity gating never reaches it in production; kept because the decode
//	    is lossless.
//
//	Vec[ u32 ]
//	    a SINGLE pool-wide NEW fraction, the shape EVERY REGISTERED pool emits. It maps to
//	    BOTH sides (Fee0New == Fee1New). There is NO old value, so HasOldFee stays false and
//	    the sink lands fee_protocol*_old NULL rather than a fabricated 0. The raw u32 is
//	    stored verbatim; its denominator is a downstream interpretation.
func decodeSetProtocolFee(sv scval.ScVal, fe *FeeEvent) error {
	// Map form — the per-token old→new transition.
	if entries, err := scval.AsMap(sv); err == nil {
		for _, f := range []struct {
			name string
			dst  *uint32
		}{
			{"fee_protocol0_new", &fe.Fee0New},
			{"fee_protocol0_old", &fe.Fee0Old},
			{"fee_protocol1_new", &fe.Fee1New},
			{"fee_protocol1_old", &fe.Fee1Old},
		} {
			msv, err := scval.MustMapField(entries, f.name)
			if err != nil {
				return fmt.Errorf("%w: set_protocol_fee.%s: %w", ErrMalformedPayload, f.name, err)
			}
			v, err := scval.AsU32(msv)
			if err != nil {
				return fmt.Errorf("%w: set_protocol_fee.%s: %w", ErrMalformedPayload, f.name, err)
			}
			*f.dst = v
		}
		fe.HasOldFee = true
		return nil
	}
	// Vec form — a single pool-wide new protocol-fee fraction.
	vec, err := scval.AsVec(sv)
	if err != nil {
		return fmt.Errorf("%w: set_protocol_fee body is neither Map nor Vec: %w", ErrMalformedPayload, err)
	}
	if len(vec) != 1 {
		return fmt.Errorf("%w: set_protocol_fee Vec length %d, want 1 (pool-wide new fraction)", ErrMalformedPayload, len(vec))
	}
	fraction, err := scval.AsU32(vec[0])
	if err != nil {
		return fmt.Errorf("%w: set_protocol_fee new fraction: %w", ErrMalformedPayload, err)
	}
	// Pool-wide fraction ⇒ both token sides carry it; no old on the wire.
	fe.Fee0New, fe.Fee1New = fraction, fraction
	fe.HasOldFee = false
	return nil
}

// decodeClaimFee fills the claim_protocol_fee fields: recipient + amount from the body Vec, and
// the claimed token from topic[1] (an ScvAddress). The token is NOT in the body and cannot be
// recovered by joining a trade (same-tx claims can differ only in token), and per-pool sums
// without the token would mix token scales.
func decodeClaimFee(e *events.Event, sv scval.ScVal, fe *FeeEvent) error {
	vec, err := scval.AsVec(sv)
	if err != nil {
		return fmt.Errorf("%w: claim_protocol_fee not a Vec: %w", ErrMalformedPayload, err)
	}
	if len(vec) != 2 {
		return fmt.Errorf("%w: claim_protocol_fee vec len %d, want 2", ErrMalformedPayload, len(vec))
	}
	if fe.Recipient, err = scval.AsAddressStrkey(vec[0]); err != nil {
		return fmt.Errorf("%w: claim_protocol_fee recipient: %w", ErrMalformedPayload, err)
	}
	if fe.Amount, err = scval.AsAmountFromI128(vec[1]); err != nil {
		return fmt.Errorf("%w: claim_protocol_fee amount: %w", ErrMalformedPayload, err)
	}
	if fe.Amount.Sign() < 0 {
		return fmt.Errorf("%w: claim_protocol_fee amount negative: %s", ErrMalformedPayload, fe.Amount)
	}
	if len(e.Topic) < 2 {
		return fmt.Errorf("%w: claim_protocol_fee has %d topics, want token address at topic[1]", ErrMalformedPayload, len(e.Topic))
	}
	tokenSv, err := scval.Parse(e.Topic[1])
	if err != nil {
		return fmt.Errorf("%w: claim_protocol_fee topic[1]: %w", ErrMalformedPayload, err)
	}
	if fe.Token, err = scval.AsAddressStrkey(tokenSv); err != nil {
		return fmt.Errorf("%w: claim_protocol_fee topic[1] not an address: %w", ErrMalformedPayload, err)
	}
	return nil
}
