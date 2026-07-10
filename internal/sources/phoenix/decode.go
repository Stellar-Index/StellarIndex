package phoenix

import (
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/events"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// RawSwap is the partial set of fields observed for a single swap.
// We fill it as the 8 distinct events arrive (Q1). Arrival order
// is NOT guaranteed; we populate slots by field-name and check
// completeness via [SwapFieldCount].
type RawSwap struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex is the in-op index of the FIRST field event of this
	// swap. A router multi-hop emits several 8-field swaps in one op;
	// the buffer emits-and-clears each before the next, so each swap's
	// first-field index is distinct — used to fan out the trade
	// op_index so the multiple trades don't collide on the trades PK
	// (ADR-0033, same as aquarius/comet/soroswap).
	EventIndex int
	Pool       string // event.ContractID of the first arriving event
	ClosedAt   time.Time

	// Populated slots. A nil-valued slot means we haven't seen that
	// field yet.
	Sender         *events.Event
	SellToken      *events.Event
	OfferAmount    *events.Event
	ActualReceived *events.Event
	BuyToken       *events.Event
	ReturnAmount   *events.Event
	SpreadAmount   *events.Event
	ReferralFee    *events.Event
}

// Complete reports whether all 8 slots are populated.
func (r *RawSwap) Complete() bool {
	return r.Sender != nil &&
		r.SellToken != nil &&
		r.OfferAmount != nil &&
		r.ActualReceived != nil &&
		r.BuyToken != nil &&
		r.ReturnAmount != nil &&
		r.SpreadAmount != nil &&
		r.ReferralFee != nil
}

// fieldsPresent returns the count of populated slots. Diagnostic
// helper used by the orphan reporter.
func (r *RawSwap) fieldsPresent() int {
	n := 0
	for _, p := range [...]*events.Event{
		r.Sender, r.SellToken, r.OfferAmount, r.ActualReceived,
		r.BuyToken, r.ReturnAmount, r.SpreadAmount, r.ReferralFee,
	} {
		if p != nil {
			n++
		}
	}
	return n
}

// assign stores e in the slot identified by topic[1]. Returns
// ErrUnknownField for non-swap-field events — the caller skips
// those.
func (r *RawSwap) assign(e *events.Event, fieldTopic string) error {
	switch fieldTopic {
	case TopicSymbolSender:
		r.Sender = e
	case TopicSymbolSellToken:
		r.SellToken = e
	case TopicSymbolOfferAmount:
		r.OfferAmount = e
	case TopicSymbolActualReceived:
		r.ActualReceived = e
	case TopicSymbolBuyToken:
		r.BuyToken = e
	case TopicSymbolReturnAmount:
		r.ReturnAmount = e
	case TopicSymbolSpreadAmount:
		r.SpreadAmount = e
	case TopicSymbolReferralFee:
		r.ReferralFee = e
	default:
		return fmt.Errorf("%w: %q", ErrUnknownField, fieldTopic)
	}
	return nil
}

// groupKey is the (ledger, tx_hash, op_index) triple. The 8 field
// events of one swap share this key. A router multihop emits
// several 8-field swaps within ONE op (same key) — the buffer
// emits-and-clears each swap before the next, then fans the trade
// op_index out on the swap's first-field event_index (see
// RawSwap.EventIndex) so they don't collide downstream.
type groupKey struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
}

func keyOf(e *events.Event) groupKey {
	return groupKey{Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex)}
}

// classify identifies a Phoenix swap event by matching
// (topic[0], topic[1]). Returns the topic[1] blob when this is a
// swap-field event; returns "" otherwise.
func classify(e *events.Event) (fieldTopic string, isSwap bool) {
	if len(e.Topic) < 2 {
		return "", false
	}
	if e.Topic[0] != TopicSymbolSwap {
		return "", false
	}
	return e.Topic[1], true
}

// action is the family of Phoenix events we recognise: swap, the
// two liquidity actions, or the two stake actions. The dispatcher
// hot path uses classifyAny so a single topic[0] match drives the
// routing without three separate Matches() calls per event.
type action int

const (
	actionUnknown action = iota
	actionSwap
	// actionSwapMap is the NEWER single-event swap schema: one
	// ScvSymbol("swap") event carrying an ScvMap body (decodeSwapMap),
	// vs actionSwap's 8 ScvString-tuple events (Q5).
	actionSwapMap
	actionProvideLiquidity
	actionWithdrawLiquidity
	actionBond
	actionUnbond
	// actionAdmin / actionInitialize are governance/lifecycle events
	// the indexer doesn't act on today — they're surfaced through
	// classifyAny() solely to satisfy the EVERY-event policy
	// (project_every_event_principle, 2026-05-25). The
	// soroban_events landing zone (ADR-0029) captures them at the
	// raw-event level; future per-event decoders can branch on
	// these action enum values.
	actionAdmin
	actionInitialize
	// actionWithdrawRewards / actionDistributeRewards — the stake
	// contract's reward-claim surface (ROADMAP #89 residual). Modelled
	// into StakeChange / phoenix_stake_events same as bond/unbond.
	actionWithdrawRewards
	actionDistributeRewards
)

// classifyAny is the union of classify + liquidity / stake topic
// matching. Returns (action, topic[1] blob) when the event is one
// of the five Phoenix actions; (actionUnknown, "") otherwise.
//
// Keeping the existing two-return classify() alongside this helper
// preserves the existing call-sites (swap tests and the original
// dispatcher path) while letting new code reach for the broader
// classifier — same byte-equality match work, one routing fan-out.
func classifyAny(e *events.Event) (action, string) {
	// NEWER Map-body swap: a SINGLE ScvSymbol("swap") topic whose body
	// is an ScvMap of all fields (post-2026-07-02 pools, e.g.
	// CBENABXP…). Checked before the two-topic String schema below —
	// this event has only one topic. See README Q5 and
	// docs/architecture/contract-schema-evolution.md.
	if len(e.Topic) == 1 {
		if e.Topic[0] == TopicSymbolSwapMap {
			return actionSwapMap, ""
		}
		return actionUnknown, ""
	}
	if len(e.Topic) < 2 {
		return actionUnknown, ""
	}
	switch e.Topic[0] {
	case TopicSymbolSwap:
		return actionSwap, e.Topic[1]
	case TopicSymbolProvideLiquidity:
		return actionProvideLiquidity, e.Topic[1]
	case TopicSymbolWithdrawLiquidity:
		return actionWithdrawLiquidity, e.Topic[1]
	case TopicSymbolBond:
		return actionBond, e.Topic[1]
	case TopicSymbolUnbond:
		return actionUnbond, e.Topic[1]
	case TopicSymbolWithdrawRewards:
		return actionWithdrawRewards, e.Topic[1]
	case TopicSymbolDistributeRewards:
		return actionDistributeRewards, e.Topic[1]
	case TopicSymbolAdmin:
		return actionAdmin, e.Topic[1]
	case TopicSymbolInitialize:
		return actionInitialize, e.Topic[1]
	}
	return actionUnknown, ""
}

// decodeSwap finalises a complete RawSwap into a canonical.Trade.
// Field mapping (per Q3):
//   - Trade.Pair.Base    = asset parsed from SellToken event body
//   - Trade.Pair.Quote   = asset parsed from BuyToken event body
//   - Trade.BaseAmount   = OfferAmount (base sold by the taker)
//   - Trade.QuoteAmount  = ReturnAmount — the buy_token amount the
//     taker actually receives. Verified against the pool contract's
//     do_swap: `return_amount` is `compute_swap.return_amount`, already
//     net of protocol commission + referral fee, and is exactly the
//     amount transferred to the sender. NOT ActualReceived: the pool
//     emits "actual received amount" as the INPUT it received of
//     sell_token (== OfferAmount), so using it made every Phoenix trade
//     base==quote and corrupted all Phoenix prices (Q3).
//   - Trade.Taker        = sender address
func decodeSwap(r *RawSwap) (canonical.Trade, error) {
	if !r.Complete() {
		return canonical.Trade{}, fmt.Errorf("%w: have %d/8 fields",
			ErrIncompleteSwap, r.fieldsPresent())
	}

	sender, err := decodeAddress(r.Sender.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("sender: %w", err)
	}
	sellToken, err := decodeAsset(r.SellToken.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("sell_token: %w", err)
	}
	buyToken, err := decodeAsset(r.BuyToken.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("buy_token: %w", err)
	}
	offer, err := decodeI128(r.OfferAmount.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("offer_amount: %w", err)
	}
	// QuoteAmount is the OUTPUT the taker received of buy_token =
	// return_amount (net of fees; see the doc comment above). The
	// "actual received amount" field is the INPUT the pool received of
	// sell_token (== offer_amount) — using it made base==quote.
	returned, err := decodeI128(r.ReturnAmount.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("return_amount: %w", err)
	}

	if offer.Sign() <= 0 || returned.Sign() <= 0 {
		return canonical.Trade{}, fmt.Errorf("%w: non-positive amount (offer %s / return %s)",
			ErrMalformedPayload, offer, returned)
	}

	pair, err := canonical.NewPair(sellToken, buyToken)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source: SourceName,
		Ledger: r.Ledger,
		TxHash: r.TxHash,
		// Fan out by the swap's first-field event index so router
		// multi-hop (several 8-field swaps in one op) doesn't collide
		// on the trades PK (ADR-0033, same as aquarius/comet/soroswap).
		OpIndex:     canonical.FanoutOpIndex(int(r.OpIndex), r.EventIndex),
		Timestamp:   r.ClosedAt,
		Pair:        pair,
		BaseAmount:  offer,
		QuoteAmount: returned,
		Taker:       sender,
	}, nil
}

// decodeSwapMap decodes the NEWER single-event Phoenix swap schema
// (Q5): one ScvSymbol("swap") event whose body is an ScvMap keyed by
// underscore-spelled Symbols. Unlike decodeSwap this needs NO
// correlation buffer — the whole trade is in one event.
//
// Field mapping is IDENTICAL to decodeSwap: BaseAmount = offer_amount
// (base sold), QuoteAmount = return_amount (buy_token the taker
// received, net of fees — NOT actual_received_amount, which the pool
// emits as the INPUT it received of sell_token, == offer_amount).
// Decode is by Map-field name (contract-schema-evolution.md), so extra
// / reordered fields don't break us.
func decodeSwapMap(ev *events.Event, closedAt time.Time) (canonical.Trade, error) {
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("parse swap map body: %w", err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("swap map body: %w", err)
	}

	// Local field readers keyed by Symbol name. entries' element type
	// stays inferred so this package needn't import xdr (ADR-0013 /
	// lint rule B — scval is the sole xdr boundary).
	addr := func(key string) (string, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return "", ferr
		}
		return scval.AsAddressStrkey(v)
	}
	amount := func(key string) (canonical.Amount, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return canonical.Amount{}, ferr
		}
		return scval.AsAmountFromI128(v)
	}

	// Map keys reuse the swap field names shared with the String
	// schema (sender / sell_token / offer_amount / buy_token /
	// return_amount are spelled identically in both).
	sender, err := addr(FieldSender)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("sender: %w", err)
	}
	sellAddr, err := addr(FieldSellToken)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("sell_token: %w", err)
	}
	buyAddr, err := addr(FieldBuyToken)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("buy_token: %w", err)
	}
	offer, err := amount(FieldOfferAmount)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("offer_amount: %w", err)
	}
	returned, err := amount(FieldReturnAmount)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("return_amount: %w", err)
	}

	if offer.Sign() <= 0 || returned.Sign() <= 0 {
		return canonical.Trade{}, fmt.Errorf("%w: non-positive amount (offer %s / return %s)",
			ErrMalformedPayload, offer, returned)
	}

	sellToken, err := canonical.NewSorobanAsset(sellAddr)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("sell_token asset: %w", err)
	}
	buyToken, err := canonical.NewSorobanAsset(buyAddr)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("buy_token asset: %w", err)
	}
	pair, err := canonical.NewPair(sellToken, buyToken)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source: SourceName,
		Ledger: ev.Ledger,
		TxHash: ev.TxHash,
		// A router multihop can emit several Map swaps in one op; fan
		// out by event index so they don't collide on the trades PK
		// (ADR-0033, same as the String path's RawSwap.EventIndex).
		OpIndex:     canonical.FanoutOpIndex(ev.OperationIndex, ev.EventIndex),
		Timestamp:   closedAt,
		Pair:        pair,
		BaseAmount:  offer,
		QuoteAmount: returned,
		Taker:       sender,
	}, nil
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.
//
// Each Phoenix swap event's body is a **raw single-value SCVal** —
// NOT wrapped in a Vec (like Aquarius's 3-tuple body) or a Map
// (like Reflector/Soroswap). That's because the pool contract
// calls `publish(topics, single_value)` with a scalar, and
// soroban-sdk serializes scalar bodies as the raw ScVal directly.
// Verified 2026-04-23 against mainnet fixtures in
// test/fixtures/phoenix/v1-2026-04-23/.

var (
	decodeAddress = sdkDecodeAddress // SCVal::Address → "G..." / "C..."
	decodeAsset   = sdkDecodeAsset   // SCVal::Address → canonical.Asset
	decodeI128    = sdkDecodeI128    // SCVal::I128 → canonical.Amount
)

// sdkDecodeAddress returns the strkey form (G… / C…) of a body
// that's a bare ScvAddress. Used for the sender field.
func sdkDecodeAddress(valueB64 string) (string, error) {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	return scval.AsAddressStrkey(sv)
}

// sdkDecodeAsset converts a bare ScvAddress body to a canonical
// Soroban asset. Used for sell_token and buy_token fields.
func sdkDecodeAsset(valueB64 string) (canonical.Asset, error) {
	addr, err := sdkDecodeAddress(valueB64)
	if err != nil {
		return canonical.Asset{}, err
	}
	return canonical.NewSorobanAsset(addr)
}

// sdkDecodeI128 converts a bare ScvI128 body to canonical.Amount.
// Used for offer_amount, actual received amount, return_amount,
// spread_amount, referral_fee_amount.
func sdkDecodeI128(valueB64 string) (canonical.Amount, error) {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return canonical.Amount{}, fmt.Errorf("parse: %w", err)
	}
	return scval.AsAmountFromI128(sv)
}

// ─── provide_liquidity / withdraw_liquidity reassembly ──────────
//
// Both follow the same N-event-per-action pattern as swap. Each
// per-field event's body is a bare single-value SCVal (Address for
// the asset/account fields, I128 for the amount fields). The
// dispatcher's serial-call assumption means buffer access is
// single-threaded; the Decoder's mutex is belt-and-braces.

// RawProvideLiquidity is the partial set of 5 fields observed for
// a single provide_liquidity call. Slots populate by topic[1]; the
// record is ready when all 5 are non-nil.
type RawProvideLiquidity struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex is the in-op index of the FIRST field-event of this
	// provide_liquidity. The buffer emits-and-clears each completed
	// action before the next, so each action's first-field index is
	// distinct — the per-event discriminator added to the
	// phoenix_liquidity PK by migration 0060 (F-1324) so two provides
	// in one op don't collide.
	EventIndex int
	Pool       string
	ClosedAt   time.Time

	Sender       *events.Event
	TokenA       *events.Event
	TokenAAmount *events.Event
	TokenB       *events.Event
	TokenBAmount *events.Event
}

// Complete reports whether all 5 slots are populated.
func (r *RawProvideLiquidity) Complete() bool {
	return r.Sender != nil &&
		r.TokenA != nil &&
		r.TokenAAmount != nil &&
		r.TokenB != nil &&
		r.TokenBAmount != nil
}

func (r *RawProvideLiquidity) fieldsPresent() int {
	n := 0
	for _, p := range [...]*events.Event{r.Sender, r.TokenA, r.TokenAAmount, r.TokenB, r.TokenBAmount} {
		if p != nil {
			n++
		}
	}
	return n
}

func (r *RawProvideLiquidity) assign(e *events.Event, fieldTopic string) error {
	switch fieldTopic {
	case TopicSymbolPLSender:
		r.Sender = e
	case TopicSymbolPLTokenA:
		r.TokenA = e
	case TopicSymbolPLTokenAAmt:
		r.TokenAAmount = e
	case TopicSymbolPLTokenB:
		r.TokenB = e
	case TopicSymbolPLTokenBAmt:
		r.TokenBAmount = e
	default:
		return fmt.Errorf("%w: %q", ErrUnknownField, fieldTopic)
	}
	return nil
}

// RawWithdrawLiquidity is the partial set of 4 required fields
// observed for a single withdraw_liquidity call. The optional
// `auto unbonded` 5th event is dropped at classify time — it
// duplicates information the stake-contract `unbond` decoder
// already captures, and not every withdraw emits it.
type RawWithdrawLiquidity struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_liquidity PK,
	// migration 0060 / F-1324); first field-event's in-op index.
	EventIndex int
	Pool       string
	ClosedAt   time.Time

	Sender        *events.Event
	SharesAmount  *events.Event
	ReturnAmountA *events.Event
	ReturnAmountB *events.Event
}

// Complete reports whether all 4 required slots are populated.
func (r *RawWithdrawLiquidity) Complete() bool {
	return r.Sender != nil &&
		r.SharesAmount != nil &&
		r.ReturnAmountA != nil &&
		r.ReturnAmountB != nil
}

func (r *RawWithdrawLiquidity) fieldsPresent() int {
	n := 0
	for _, p := range [...]*events.Event{r.Sender, r.SharesAmount, r.ReturnAmountA, r.ReturnAmountB} {
		if p != nil {
			n++
		}
	}
	return n
}

func (r *RawWithdrawLiquidity) assign(e *events.Event, fieldTopic string) error {
	switch fieldTopic {
	case TopicSymbolWLSender:
		r.Sender = e
	case TopicSymbolWLSharesAmount:
		r.SharesAmount = e
	case TopicSymbolWLReturnAmountA:
		r.ReturnAmountA = e
	case TopicSymbolWLReturnAmountB:
		r.ReturnAmountB = e
	case TopicSymbolWLAutoUnbonded:
		// Optional event — recognised so we don't ErrUnknownField,
		// but not stored. The withdraw record is independent of
		// auto-unbond presence.
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownField, fieldTopic)
	}
	return nil
}

// RawStake is the partial set of 3 fields observed for one
// bond / unbond call from the stake contract. The same shape
// services both — the action (bond vs unbond) is carried alongside
// in the buffer's group key.
type RawStake struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_stake_events PK,
	// migration 0060 / F-1324); first field-event's in-op index.
	EventIndex int
	Contract   string
	ClosedAt   time.Time
	IsBond     bool // true for bond, false for unbond

	User   *events.Event
	Token  *events.Event
	Amount *events.Event
}

// Complete reports whether all 3 slots are populated.
func (r *RawStake) Complete() bool {
	return r.User != nil && r.Token != nil && r.Amount != nil
}

func (r *RawStake) fieldsPresent() int {
	n := 0
	for _, p := range [...]*events.Event{r.User, r.Token, r.Amount} {
		if p != nil {
			n++
		}
	}
	return n
}

func (r *RawStake) assign(e *events.Event, fieldTopic string) error {
	switch fieldTopic {
	case TopicSymbolStakeUser:
		r.User = e
	case TopicSymbolStakeToken:
		r.Token = e
	case TopicSymbolStakeAmount:
		r.Amount = e
	default:
		return fmt.Errorf("%w: %q", ErrUnknownField, fieldTopic)
	}
	return nil
}

// RawWithdrawRewards is the partial set of 2 fields observed for one
// withdraw_rewards call from the stake contract. Real-lake bytes
// (2026-07-10, ledgers 53588319 / 53589647, stake contracts
// CBRGNWGAC25… / CAF3UJ45ZQJ…) confirm exactly two fields — user +
// reward_token — no amount field on this event (see events.go's
// package doc for where the amount actually surfaces).
type RawWithdrawRewards struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_stake_events PK,
	// migration 0060 / F-1324); first field-event's in-op index.
	EventIndex int
	Contract   string
	ClosedAt   time.Time

	User        *events.Event
	RewardToken *events.Event
}

// Complete reports whether both slots are populated.
func (r *RawWithdrawRewards) Complete() bool {
	return r.User != nil && r.RewardToken != nil
}

func (r *RawWithdrawRewards) fieldsPresent() int {
	n := 0
	for _, p := range [...]*events.Event{r.User, r.RewardToken} {
		if p != nil {
			n++
		}
	}
	return n
}

func (r *RawWithdrawRewards) assign(e *events.Event, fieldTopic string) error {
	switch fieldTopic {
	case TopicSymbolWRUser:
		r.User = e
	case TopicSymbolWRRewardToken:
		r.RewardToken = e
	default:
		return fmt.Errorf("%w: %q", ErrUnknownField, fieldTopic)
	}
	return nil
}

// ─── Finalisers ─────────────────────────────────────────────────

// decodeProvideLiquidity turns a complete RawProvideLiquidity into
// the canonical LiquidityChange that the consumer projects onto a
// phoenix_liquidity row.
func decodeProvideLiquidity(r *RawProvideLiquidity) (LiquidityChange, error) {
	if !r.Complete() {
		return LiquidityChange{}, fmt.Errorf("%w: provide_liquidity have %d/%d fields",
			ErrIncompleteLiquidity, r.fieldsPresent(), ProvideLiquidityFieldCount)
	}
	sender, err := decodeAddress(r.Sender.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity sender: %w", err)
	}
	tokenA, err := decodeAddress(r.TokenA.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_a: %w", err)
	}
	tokenB, err := decodeAddress(r.TokenB.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_b: %w", err)
	}
	amountA, err := decodeI128(r.TokenAAmount.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_a-amount: %w", err)
	}
	amountB, err := decodeI128(r.TokenBAmount.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_b-amount: %w", err)
	}
	return LiquidityChange{
		Action:     EventActionProvideLiquidity,
		Pool:       r.Pool,
		Ledger:     r.Ledger,
		TxHash:     r.TxHash,
		OpIndex:    int(r.OpIndex),
		EventIndex: r.EventIndex,
		ClosedAt:   r.ClosedAt,
		Sender:     sender,
		TokenA:     tokenA,
		AmountA:    amountA,
		TokenB:     tokenB,
		AmountB:    amountB,
	}, nil
}

// decodeWithdrawLiquidity turns a complete RawWithdrawLiquidity
// into the canonical LiquidityChange. Shares amount lives on the
// dedicated field; per-token return amounts go in AmountA / AmountB.
// TokenA / TokenB stay empty — the withdraw event does not carry
// the pool's asset addresses (only the share-token); downstream can
// join against the provide_liquidity rows that established the pool.
func decodeWithdrawLiquidity(r *RawWithdrawLiquidity) (LiquidityChange, error) {
	if !r.Complete() {
		return LiquidityChange{}, fmt.Errorf("%w: withdraw_liquidity have %d/%d fields",
			ErrIncompleteLiquidity, r.fieldsPresent(), WithdrawLiquidityFieldCount)
	}
	sender, err := decodeAddress(r.Sender.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity sender: %w", err)
	}
	shares, err := decodeI128(r.SharesAmount.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity shares_amount: %w", err)
	}
	returnA, err := decodeI128(r.ReturnAmountA.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity return_amount_a: %w", err)
	}
	returnB, err := decodeI128(r.ReturnAmountB.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity return_amount_b: %w", err)
	}
	return LiquidityChange{
		Action:       EventActionWithdrawLiquidity,
		Pool:         r.Pool,
		Ledger:       r.Ledger,
		TxHash:       r.TxHash,
		OpIndex:      int(r.OpIndex),
		EventIndex:   r.EventIndex,
		ClosedAt:     r.ClosedAt,
		Sender:       sender,
		SharesAmount: shares,
		AmountA:      returnA,
		AmountB:      returnB,
	}, nil
}

// decodeStake turns a complete RawStake into the canonical
// StakeChange. Same shape services bond / unbond — the action
// label is set from the buffer's group key.
func decodeStake(r *RawStake) (StakeChange, error) {
	if !r.Complete() {
		return StakeChange{}, fmt.Errorf("%w: have %d/%d fields",
			ErrIncompleteStake, r.fieldsPresent(), StakeFieldCount)
	}
	user, err := decodeAddress(r.User.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("stake user: %w", err)
	}
	token, err := decodeAddress(r.Token.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("stake token: %w", err)
	}
	amount, err := decodeI128(r.Amount.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("stake amount: %w", err)
	}
	action := EventActionUnbond
	if r.IsBond {
		action = EventActionBond
	}
	return StakeChange{
		Action:     action,
		Contract:   r.Contract,
		Ledger:     r.Ledger,
		TxHash:     r.TxHash,
		OpIndex:    int(r.OpIndex),
		EventIndex: r.EventIndex,
		ClosedAt:   r.ClosedAt,
		User:       user,
		LPToken:    token,
		Amount:     amount,
	}, nil
}

// decodeWithdrawRewards turns a complete RawWithdrawRewards into a
// StakeChange. Reuses the StakeChange/phoenix_stake_events shape:
// LPToken holds the reward-token address (not an LP share token —
// see events.go doc). Amount is left at its zero value; the sink
// (internal/pipeline/sink.go::persistPhoenixStake) writes NULL rather
// than the zero-value "0" string for reward actions.
func decodeWithdrawRewards(r *RawWithdrawRewards) (StakeChange, error) {
	if !r.Complete() {
		return StakeChange{}, fmt.Errorf("%w: withdraw_rewards have %d/%d fields",
			ErrIncompleteStake, r.fieldsPresent(), WithdrawRewardsFieldCount)
	}
	user, err := decodeAddress(r.User.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("withdraw_rewards user: %w", err)
	}
	rewardToken, err := decodeAddress(r.RewardToken.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("withdraw_rewards reward_token: %w", err)
	}
	return StakeChange{
		Action:     EventActionWithdrawRewards,
		Contract:   r.Contract,
		Ledger:     r.Ledger,
		TxHash:     r.TxHash,
		OpIndex:    int(r.OpIndex),
		EventIndex: r.EventIndex,
		ClosedAt:   r.ClosedAt,
		User:       user,
		LPToken:    rewardToken,
	}, nil
}

// decodeDistributeRewards decodes a single distribute_rewards event
// directly — no correlation buffer needed, the action carries exactly
// one field (asset) per real-lake confirmation. Pool-wide: User stays
// empty (the wire carries no user field for this action).
func decodeDistributeRewards(ev *events.Event, closedAt time.Time) (StakeChange, error) {
	asset, err := decodeAddress(ev.Value)
	if err != nil {
		return StakeChange{}, fmt.Errorf("distribute_rewards asset: %w", err)
	}
	return StakeChange{
		Action:     EventActionDistributeRewards,
		Contract:   ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    ev.OperationIndex,
		EventIndex: ev.EventIndex,
		ClosedAt:   closedAt,
		LPToken:    asset,
	}, nil
}
