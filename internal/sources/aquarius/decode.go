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

// kindByTopicSymbol maps the pre-encoded topic[0] SCVal::Symbol blob
// of every recognized Aquarius event to its event-kind name. Built
// once at package init from the TopicSymbol*/Event* constant pairs
// in events.go (uniqueness of keys holds because each TopicSymbol*
// encodes a distinct Event* string).
//
// Every topic Aquarius emits (the original AMM surface verified
// 2026-05-27 against the then-public upstream Rust source; the
// rewards-gauge + governance surfaces verified 2026-07-10 against
// real r1 lake bytes — the upstream repo is no longer public) must
// appear here — the EVERY-event policy
// (memory: project_every_event_principle) treats classify() as the
// authoritative completeness gate for BackfillSafe, and
// TestClassify_completenessVsUpstream enumerates the closed set.
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

// decodeTrade decodes an Aquarius `trade` event into a single
// canonical.Trade. Unlike the earlier stub, this decoder needs NO
// pool-info cache — token identities are carried directly in the
// event topics.
//
// Verified against aquarius-amm/liquidity_pool_events/src/lib.rs:122-150
// (soroban-sdk 25.0.2):
//
//	e.events().publish(
//	    (Symbol::new(e, "trade"), token_in, token_out, user),
//	    (in_amount as i128, out_amount as i128, fee_amount as i128),
//	);
//
// Topics (4):
//
//	topic[0] = Symbol("trade")
//	topic[1] = Address(token_in)  — sold_asset
//	topic[2] = Address(token_out) — bought_asset
//	topic[3] = Address(user)      — trader (often a router contract)
//
// Body: Vec<ScVal> of length 3 = [i128, i128, i128] —
// (sold_amount, bought_amount, fee). soroban-sdk serializes
// tuple-shaped event bodies as ScvVec (NOT Map, which is only used
// for named-field struct bodies via #[contracttype]).
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

	if amounts.SoldAmount.Sign() <= 0 || amounts.BoughtAmount.Sign() <= 0 {
		return canonical.Trade{}, fmt.Errorf("%w: non-positive amounts sold=%s bought=%s",
			ErrMalformedPayload, amounts.SoldAmount, amounts.BoughtAmount)
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
		OpIndex:     canonical.FanoutOpIndex(e.OperationIndex, e.EventIndex),
		Timestamp:   closedAt,
		Pair:        pair,
		BaseAmount:  amounts.SoldAmount,
		QuoteAmount: amounts.BoughtAmount,
		Taker:       userAddr,
	}, nil
}

// decodeReserves decodes an Aquarius `update_reserves` event into a
// ReservesEvent carrying the pool's POST-STATE reserve vector.
//
// Verified against the r1 lake 2026-07-06: topic[0] is the only topic
// (Symbol("update_reserves"), no token addresses), and the body is a
// Vec<i128> of the pool's reserves in canonical token order —
// [reserve_0, …, reserve_{n-1}]. n is the pool's token count (2 for a
// volatile pool, N for stableswap), so we read a variable-length vec
// rather than a fixed tuple.
func decodeReserves(e *events.Event, closedAt time.Time) (ReservesEvent, error) {
	reserves, err := decodeAmountVec(e.Value)
	if err != nil {
		return ReservesEvent{}, fmt.Errorf("%w: update_reserves body: %w", ErrMalformedPayload, err)
	}
	if len(reserves) == 0 {
		return ReservesEvent{}, fmt.Errorf("%w: update_reserves empty reserve vector", ErrMalformedPayload)
	}
	for i, r := range reserves {
		if r.Sign() < 0 {
			return ReservesEvent{}, fmt.Errorf("%w: update_reserves reserve[%d] negative: %s", ErrMalformedPayload, i, r)
		}
	}
	return ReservesEvent{
		ContractID: e.ContractID,
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex), //nolint:gosec // OperationIndex is non-negative by Soroban spec.
		EventIndex: uint32(e.EventIndex),     //nolint:gosec // EventIndex is non-negative by Soroban spec.
		ObservedAt: closedAt,
		Reserves:   reserves,
	}, nil
}

// decodeLiquidity decodes an Aquarius `deposit_liquidity` /
// `withdraw_liquidity` event into a LiquidityEvent.
//
// Verified against the r1 lake 2026-07-06. Wire shape (both events
// share it):
//
//	topics: [Symbol(action), Address(token_0), …, Address(token_{n-1})]
//	body:   Vec<i128> of length n+1 =
//	        [amount_0, …, amount_{n-1}, share_amount]
//
// where n = len(topics) - 1 is the pool's token count (2 for a
// volatile pool, 3–4 for stableswap — all three widths observed live).
// The trailing body element is the LP-share amount minted (deposit) /
// burned (withdraw). Decode by the (topic-count, body-length)
// relationship rather than a fixed 2-token assumption so N-token
// stableswap events are captured, not dropped.
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

// sdkDecodeAmountVec unpacks a body that is a Vec of i128 values of
// arbitrary length (the update_reserves reserve vector and the
// deposit/withdraw [amounts…, shares] vector). Unlike the trade body
// (a fixed 3-tuple) these vectors are variable-length — one element
// per pool token (+1 for the liquidity share amount) — so we read the
// vec and decode each element positionally. Every element MUST be an
// i128 (ADR-0003 / verified live 2026-07-06); a non-i128 element is a
// schema violation we reject rather than truncate.
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

// sdkDecodeTradeAmounts unpacks the body Vec of 3 i128s.
//
// The contract emits the body as a Rust tuple `(i128, i128, i128)` —
// soroban-sdk serializes this as ScvVec of length 3, in positional
// order (sold, bought, fee). Unlike Map-based bodies we cannot
// decode by field name here; we rely on arity to detect a future
// contract upgrade that changes the tuple shape.
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

// decodeAnnouncedPool extracts the pool address a ROUTER `add_pool`
// event announces (ADR-0035/0040 fan-out seam). The router emits its
// pool-scoped events with body `Vec[Address(pool), …]` — verified
// against the r1 lake on 2026-07-05: all 338 add_pool bodies (and
// every router swap/deposit/withdraw body) decode this way with zero
// parse failures (docs/protocols/aquarius.md). The announced address
// must be a contract (C-strkey); anything else is malformed.
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
