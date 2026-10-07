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
// Every topic an Aquarius pool emits must appear here, and
// TestClassify_completenessVsUpstream enumerates the closed set. The AMM
// surface was verified against the upstream Rust source while it was
// public; the rewards-gauge and governance surfaces against r1 lake
// bytes. The router's own `swap` / `deposit` / `withdraw` do not match:
// a documented known gap (README "Known gap"), pinned by
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

// decodeTrade decodes an Aquarius `trade` event into a single
// canonical.Trade. It needs NO pool-info cache: token identities are
// carried directly in the event topics.
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

	// NEGATIVE amounts are a schema violation — refuse. ZERO amounts are
	// NOT: the lake proves genuine dust swaps whose output (or input)
	// rounds to zero — e.g. ledger 53,626,410 event 2, body
	// (sold=2, bought=0, fee=0) from registered pool CCY2PXGM… — and the
	// pool contract emits the trade event regardless. canonical.Trade
	// forbids non-positive amounts (Validate: a zero side breaks price
	// derivation), so a zero-amount swap can never become a served trade
	// row. Classifying it as a malformed-payload error blinds the
	// completeness re-derive on those ledgers: they were 40 of the 41
	// undecodable-but-matched events on the first full-range reconcile
	// (the 41st was the set_privileged_addrs v2 arity, decode_admin.go).
	// It is a RECOGNIZED NO-OP (ErrZeroAmountTrade), the same shape as
	// redstone's empty write_prices batch: zero rows project and the
	// reconcile sees expected == served == 0.
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
		// BaseAmount is sold_amount unmodified. Real mainnet fixtures
		// settle this (TestTradeAmounts_feeIsGrossOfSoldAmount): every
		// captured trade satisfies
		// fee == ceil(sold_amount * pool_fee_bps / 10000), which only
		// holds if sold_amount is the taker's gross input. A net
		// interpretation (gross = sold_amount + fee) does not match any
		// fixture. So sold_amount needs no fee adjustment here, matching
		// Comet's gross-input convention.
		BaseAmount:  amounts.SoldAmount,
		QuoteAmount: amounts.BoughtAmount,
		Taker:       userAddr,
	}, nil
}

// decodeReserves decodes an Aquarius `update_reserves` or `reserves_sync`
// event — both share a `Vec<i128>` body of per-token values. `kind` is
// the classify() result (EventUpdateReserves | EventReservesSync); it is
// carried onto the ReservesEvent so the sink routes it to the right table
// and appears in error messages.
//
// Verified against the r1 lake for update_reserves: topic[0] is the only
// topic (Symbol("update_reserves"), no token addresses), and the body is
// the pool's POST-STATE reserves in canonical token order —
// [reserve_0, …, reserve_{n-1}]. n is the pool's token count (2 for a
// volatile pool, N for stableswap), so we read a variable-length vec
// rather than a fixed tuple.
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

// decodeLiquidity decodes an Aquarius `deposit_liquidity` /
// `withdraw_liquidity` event into a LiquidityEvent.
//
// Verified against the r1 lake. Wire shape (both events share it):
//
//	topics: [Symbol(action), Address(token_0), …, Address(token_{n-1})]
//	body:   Vec<i128> of length n+1 =
//	        [amount_0, …, amount_{n-1}, share_amount]
//
// where n = len(topics) - 1 is the pool's token count (2 for a
// volatile pool, 3–4 for stableswap; real fixtures cover 2 and 3,
// the 4-token width is covered by an SDK-built test only).
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
// i128 (ADR-0003; verified against the live lake); a non-i128 element is
// a schema violation we reject rather than truncate.
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
// pool-scoped events with body `Vec[Address(pool), …]`. Verified against
// the r1 lake: all 338 add_pool bodies (and every router
// swap/deposit/withdraw body) decoded this way with zero parse failures
// (docs/protocols/aquarius.md). The announced address must be a contract
// (C-strkey); anything else is malformed.
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

// decodeFee decodes an Aquarius `set_protocol_fee` or
// `claim_protocol_fee` treasury event into a FeeEvent. `kind` (the
// classify() result) selects the decode:
//
//	set_protocol_fee   — TWO real body shapes, both handled by
//	                     decodeSetProtocolFee:
//	                       Map[ fee_protocol{0,1}_{new,old}: u32 ] and
//	                       Vec[ u32 ] (a single pool-wide new fraction).
//	claim_protocol_fee — Vec[ recipient: Address, amount: i128 ]
//
// Decode-by-field-name for the Map (schema-evolution safe); the Vecs are
// positional per the contract. Verified against real lake bodies.
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

// decodeSetProtocolFee fills the set_protocol_fee fields from EITHER of
// the two real on-chain body shapes (branch on the parsed SCVal kind):
//
//	Map[ fee_protocol{0,1}_{new,old}: u32 ]
//	    the per-token old→new transition, decoded by field name
//	    (schema-evolution safe). This is the shape the migration-0129
//	    pass sampled (values 0→4 / 0→10). NOTE: the lake holds this Map
//	    shape ONLY on contracts that are NOT registered Aquarius pools,
//	    so contract-identity gating means it is not reached in
//	    production — kept because the wire shape is real and the decode
//	    is cheap and lossless.
//
//	Vec[ u32 ]
//	    a SINGLE pool-wide NEW protocol-fee fraction — the shape EVERY
//	    REGISTERED Aquarius pool emits. All 163 lake-wide occurrences are
//	    the byte-identical body Vec[u32(5000)] (the governance sweep
//	    that set 160 registered pools in one tx — ledger 57,697,910 —
//	    plus later stragglers). The pool contract's fee API is a SINGLE
//	    `set_protocol_fee_fraction` / `get_protocol_fee_fraction` with a
//	    `new_fraction` topic — verified across every pool-WASM
//	    generation's disassembly in docs/operations/wasm-audits/evidence/
//	    (the strings `set_protocol_fee_fraction`, `new_fraction`,
//	    `get_protocol_fee_fraction`, and NO per-token `fee_protocol*`
//	    keys in any Aquarius pool WASM). So the one u32 is
//	    the new fraction for the WHOLE pool; it maps to BOTH token sides
//	    (Fee0New == Fee1New == fraction). The body carries NO old value
//	    and NO per-token split — HasOldFee stays false so the sink lands
//	    the fee_protocol*_old columns NULL rather than a fabricated 0
//	    (do-not-invent: the prior fraction is genuinely not on the wire).
//	    The raw u32 is stored verbatim; the Aquarius fee-fraction
//	    denominator is a downstream interpretation, not asserted here.
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

// decodeClaimFee fills the claim_protocol_fee fields: recipient +
// amount from the body Vec, and the claimed token from topic[1] (an
// ScvAddress). The token is NOT in the body, and joining a recent trade
// cannot recover it: ledger 63,698,651 has two same-tx claims with
// different topic[1] tokens and near-identical amounts, so per-pool sums
// without the token mix token scales.
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
