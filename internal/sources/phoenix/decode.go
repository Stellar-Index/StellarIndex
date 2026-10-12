package phoenix

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// RawSwap is the partial set of fields seen for one swap, filled as the 8 distinct events arrive in any order; slots are keyed by field name and completeness is checked against [SwapFieldCount].
type RawSwap struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex is the in-op index of the FIRST field event of this swap. A router multi-hop
	// emits several 8-field swaps in one op; each swap's distinct first-field index fans out the
	// trade op_index so the trades do not collide on the trades PK (ADR-0033).
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

// Complete reports whether all 8 slots are populated, the CURRENT (post-upgrade) full event set and the trigger for the eager emit path.
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

// Decodable reports whether the slots decodeSwap CONSUMES are populated (Sender, SellToken, OfferAmount, BuyToken, ReturnAmount); it is the sweep-time gate because pre-upgrade pool WASM emitted only 7 field-events (no "actual received amount") and gating on Complete() would drop those swaps; ActualReceived is the pool's INPUT echo and corrupts prices, so it is unread.
func (r *RawSwap) Decodable() bool {
	return r.Sender != nil &&
		r.SellToken != nil &&
		r.OfferAmount != nil &&
		r.BuyToken != nil &&
		r.ReturnAmount != nil
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

// assign stores e in the slot named by topic[1]; ErrUnknownField for non-swap-field events, which the caller skips.
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

// slot returns the event stored for fieldTopic, or nil if empty or not a swap field; absorb uses it to detect a second swap
// reusing the same groupKey before it overwrites the first.
func (r *RawSwap) slot(fieldTopic string) *events.Event {
	switch fieldTopic {
	case TopicSymbolSender:
		return r.Sender
	case TopicSymbolSellToken:
		return r.SellToken
	case TopicSymbolOfferAmount:
		return r.OfferAmount
	case TopicSymbolActualReceived:
		return r.ActualReceived
	case TopicSymbolBuyToken:
		return r.BuyToken
	case TopicSymbolReturnAmount:
		return r.ReturnAmount
	case TopicSymbolSpreadAmount:
		return r.SpreadAmount
	case TopicSymbolReferralFee:
		return r.ReferralFee
	}
	return nil
}

// groupKey is (ledger, tx_hash, op_index, contract_id): the 8 field events of one swap come from one pool. ContractID matters for
// router multihops, where different pools share (ledger, tx, op) and would hijack each other's legs. EventIndex is NOT in the key:
// each field event has a distinct one, which would shatter the grouping.
type groupKey struct {
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	ContractID string
}

func keyOf(e *events.Event) groupKey {
	return groupKey{
		Ledger:     e.Ledger,
		TxHash:     e.TxHash,
		OpIndex:    uint32(e.OperationIndex),
		ContractID: e.ContractID,
	}
}

// classify identifies a Phoenix swap event by (topic[0], topic[1]); returns the topic[1] blob for a swap-field event, else "".
func classify(e *events.Event) (fieldTopic string, isSwap bool) {
	if len(e.Topic) < 2 {
		return "", false
	}
	if e.Topic[0] != TopicSymbolSwap {
		return "", false
	}
	return e.Topic[1], true
}

// action is the family of Phoenix events we recognise; the dispatcher hot path uses classifyAny so one topic[0] match drives routing.
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
	// actionProvideLiquidityMap / actionWithdrawLiquidityMap are the Map
	// schema's single-event liquidity actions (decode*LiquidityMap).
	actionProvideLiquidityMap
	actionWithdrawLiquidityMap
	actionBond
	actionUnbond
	// actionAdmin / actionInitialize are governance/lifecycle events,
	// at most one row each: decodeAdminEvent → phoenix_admin_events and
	// decodeInitializeEvent → phoenix_initialize. The soroban_events
	// landing zone (ADR-0029) also keeps them at the raw-event level.
	actionAdmin
	actionInitialize
	// actionWithdrawRewards / actionDistributeRewards — the stake
	// contract's reward-claim surface. Modelled
	// into StakeChange / phoenix_stake_events same as bond/unbond.
	actionWithdrawRewards
	actionDistributeRewards
	// actionCreatePool is the FACTORY's ("create","liquidity_pool") pool announcement, the only
	// action emitted by the factory. It projects no row; it only admits the announced pool into
	// the identity gate (ADR-0040 section 1).
	actionCreatePool
	// Single-event shapes, each fully specified by its topic pair (or
	// single Symbol topic); see events.go EventActionCreateDistributionFlow.
	actionCreateDistributionFlow
	actionStakeMigration
	actionFactoryConfig
	actionBlendPoolAdmin
	actionToggleTrading
)

type topicPair struct{ t0, t1 string }

// topicPairActions lists every accepted (topic[0], topic[1]) for single-event shapes; any other topic[1] under these stays
// actionUnknown, a visible recognition gap rather than a guess.
var topicPairActions = map[topicPair]action{
	{TopicCreateDistributionFlow, TopicSymbolDRAsset}: actionCreateDistributionFlow,
	{TopicStakeMigration, TopicMigrationStarted}:      actionStakeMigration,
	{TopicStakeMigration, TopicMigrationQueried}:      actionStakeMigration,
	{TopicStake, TopicMigrationCompleted}:             actionStakeMigration,
	{TopicFactory, TopicFactoryUpdatedConfig}:         actionFactoryConfig,
	{TopicBlendPool, TopicBlendSetDelegate}:           actionBlendPoolAdmin,
	{TopicBlendPool, TopicBlendSetMinTradingA}:        actionBlendPoolAdmin,
	{TopicBlendPool, TopicBlendSetMinTradingB}:        actionBlendPoolAdmin,
	{TopicToggleTrading, TopicToggleTradingEnabled}:   actionToggleTrading,
}

// classifyAny is classify plus liquidity / stake topic matching: (action, topic[1] blob), "" blob for single-topic Map shapes,
// else (actionUnknown, "").
func classifyAny(e *events.Event) (action, string) {
	// Newer Map-body swap: a single ScvSymbol("swap") topic with an ScvMap body of all fields.
	// Checked before the two-topic String schema below. See README Q5 and
	// docs/architecture/ingest-pipeline.md#contract-schema-evolution.
	if len(e.Topic) == 1 {
		switch e.Topic[0] {
		case TopicSymbolSwapMap:
			return actionSwapMap, ""
		case TopicSymbolProvideLiquidityMap:
			return actionProvideLiquidityMap, ""
		case TopicSymbolWithdrawLiquidityMap:
			return actionWithdrawLiquidityMap, ""
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
		// The only audited shape is ("distribute_rewards","asset"); any
		// other topic[1] is unaudited and must surface as a recognition
		// gap, not decode its body as an asset address.
		if e.Topic[1] != TopicSymbolDRAsset {
			return actionUnknown, ""
		}
		return actionDistributeRewards, e.Topic[1]
	case TopicSymbolAdmin:
		return actionAdmin, e.Topic[1]
	case TopicSymbolInitialize:
		return actionInitialize, e.Topic[1]
	case TopicSymbolCreate:
		// Only the factory's single documented announcement pair classifies. Any other
		// ("create", ...) shape is unaudited: it stays actionUnknown and fail-closes into a
		// visible ADR-0033 recognition gap instead of reaching admission. The emitter is
		// checked separately (Matches, reg.IsFactory) because topics alone are forgeable.
		if e.Topic[1] != TopicCreateLiquidityPool {
			return actionUnknown, ""
		}
		return actionCreatePool, e.Topic[1]
	}
	if a, ok := topicPairActions[topicPair{e.Topic[0], e.Topic[1]}]; ok {
		return a, e.Topic[1]
	}
	return actionUnknown, ""
}

// decodeAnnouncedPool extracts the pool address the factory's ("create","liquidity_pool") event announces (ADR-0035/0040 seam).
// The body is one ScvAddress, the return value of the factory's deploy_v2, so it cannot be caller-supplied; that makes
// self-registration safe (docs/operations/wasm-audits/phoenix.md). Must be a C-strkey contract, else malformed.
func decodeAnnouncedPool(e *events.Event) (string, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return "", fmt.Errorf("%w: create body: %w", ErrMalformedPayload, err)
	}
	pool, err := scval.AsAddressStrkey(body)
	if err != nil {
		return "", fmt.Errorf("%w: create announced pool address: %w", ErrMalformedPayload, err)
	}
	if len(pool) == 0 || pool[0] != 'C' {
		return "", fmt.Errorf("%w: create announced a non-contract address %q", ErrMalformedPayload, pool)
	}
	return pool, nil
}

// decodeSwap finalises a complete RawSwap into a canonical.Trade.
// Field mapping (per Q3):
//   - Trade.Pair.Base    = asset parsed from SellToken event body
//   - Trade.Pair.Quote   = asset parsed from BuyToken event body
//   - Trade.BaseAmount   = OfferAmount (base sold by the taker). The pool prices off
//     offer_amount before the transfer, so return/offer is the executed price even for a
//     fee-on-transfer sell token; that divergence is surfaced, not stored
//     (noteReceivedDivergence).
//   - Trade.QuoteAmount  = ReturnAmount, the buy_token amount the taker receives (net of
//     commission and referral fee). NOT ActualReceived: the pool emits it as the INPUT it
//     received of sell_token, so using it made every trade base==quote and corrupted prices (Q3).
//   - Trade.Taker        = sender address
func decodeSwap(r *RawSwap) (canonical.Trade, error) {
	// Decodable, not Complete: the guard's job is "can this function
	// read every slot it touches", and it touches five. Requiring the
	// unread ActualReceived here silently zeroed the 7-event
	// pre-upgrade era (see Decodable's doc).
	if !r.Decodable() {
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
	// QuoteAmount is the OUTPUT the taker received of buy_token (return_amount, net of fees).
	// "actual received amount" is the INPUT the pool received; using it made base==quote.
	returned, err := decodeI128(r.ReturnAmount.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("return_amount: %w", err)
	}

	if err := checkSwapAmounts(offer, returned); err != nil {
		return canonical.Trade{}, err
	}

	pair, err := canonical.NewPair(sellToken, buyToken)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	if r.ActualReceived != nil {
		if received, rerr := decodeI128(r.ActualReceived.Value); rerr == nil {
			noteReceivedDivergence(r.Pool, r.TxHash, r.Ledger, offer, received)
		}
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

// checkSwapAmounts rejects a negative leg as malformed and a zero leg as ErrZeroAmountSwap (a determinate dust swap, no row).
func checkSwapAmounts(offer, returned canonical.Amount) error {
	if offer.Sign() < 0 || returned.Sign() < 0 {
		return fmt.Errorf("%w: negative amount (offer %s / return %s)",
			ErrMalformedPayload, offer, returned)
	}
	if offer.Sign() == 0 || returned.Sign() == 0 {
		return fmt.Errorf("%w: offer %s / return %s", ErrZeroAmountSwap, offer, returned)
	}
	return nil
}

// mapFieldActualReceived is the Map-schema spelling of FieldActualReceived.
const mapFieldActualReceived = "actual_received_amount"

// noteReceivedDivergence surfaces a swap whose pool-received sell amount differs from offer_amount (fee-on-transfer sell token)
// without changing the stored base leg (see decodeSwap).
func noteReceivedDivergence(pool, txHash string, ledger uint32, offer, received canonical.Amount) {
	if received.Equal(offer) {
		return
	}
	obs.AMMSwapReceivedDivergenceTotal.WithLabelValues(SourceName).Inc()
	slog.Warn("phoenix: swap actual received amount differs from offer_amount",
		"pool", pool, "ledger", ledger, "tx_hash", txHash,
		"offer_amount", offer.String(), "actual_received", received.String())
}

// decodeSwapMap decodes the newer single-event swap schema (Q5): one ScvSymbol("swap") event with an ScvMap body, no correlation
// buffer. Mapping matches decodeSwap (offer_amount / return_amount, never actual_received_amount); decoded by Map-field name, so
// extra or reordered fields are safe.
func decodeSwapMap(ev *events.Event, closedAt time.Time) (canonical.Trade, error) {
	addr, amount, err := mapBodyReaders(ev.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("swap %w", err)
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

	if err := checkSwapAmounts(offer, returned); err != nil {
		return canonical.Trade{}, err
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

	if received, rerr := amount(mapFieldActualReceived); rerr == nil {
		noteReceivedDivergence(ev.ContractID, ev.TxHash, ev.Ledger, offer, received)
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

// mapBodyReaders parses a Map-schema event body and returns readers for
// its Address and i128 fields, keyed by Symbol name.
func mapBodyReaders(valueB64 string) (
	addr func(string) (string, error),
	amount func(string) (canonical.Amount, error),
	err error,
) {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return nil, nil, fmt.Errorf("map body parse: %w", err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, nil, fmt.Errorf("map body: %w", err)
	}
	// entries' element type stays inferred so this package needn't import
	// xdr (ADR-0013 / lint rule B — scval is the sole xdr boundary).
	addr = func(key string) (string, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return "", ferr
		}
		return scval.AsAddressStrkey(v)
	}
	amount = func(key string) (canonical.Amount, error) {
		v, ferr := scval.MustMapField(entries, key)
		if ferr != nil {
			return canonical.Amount{}, ferr
		}
		return scval.AsAmountFromI128(v)
	}
	return addr, amount, nil
}

// Map-schema liquidity keys. sender / token_a / token_b / shares_amount /
// return_amount_a / return_amount_b share the String schema's spelling.
const (
	mapFieldActualReceivedA = "actual_received_a"
	mapFieldActualReceivedB = "actual_received_b"
)

// decodeProvideLiquidityMap decodes the Map schema's single-event provide_liquidity into the LiquidityChange decodeProvideLiquidity
// builds from the String schema's 5 events; decoded by field name, so extra or reordered keys are safe.
func decodeProvideLiquidityMap(ev *events.Event, closedAt time.Time) (LiquidityChange, error) {
	addr, amount, err := mapBodyReaders(ev.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity %w", err)
	}
	sender, err := addr(FieldPLSender)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity sender: %w", err)
	}
	tokenA, err := addr(FieldPLTokenA)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_a: %w", err)
	}
	tokenB, err := addr(FieldPLTokenB)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity token_b: %w", err)
	}
	amountA, err := amount(mapFieldActualReceivedA)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity %s: %w", mapFieldActualReceivedA, err)
	}
	amountB, err := amount(mapFieldActualReceivedB)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("provide_liquidity %s: %w", mapFieldActualReceivedB, err)
	}
	return LiquidityChange{
		Action:     EventActionProvideLiquidity,
		Pool:       ev.ContractID,
		Ledger:     ev.Ledger,
		TxHash:     ev.TxHash,
		OpIndex:    ev.OperationIndex,
		EventIndex: ev.EventIndex,
		ClosedAt:   closedAt,
		Sender:     sender,
		TokenA:     tokenA,
		AmountA:    amountA,
		TokenB:     tokenB,
		AmountB:    amountB,
	}, nil
}

// decodeWithdrawLiquidityMap is the withdraw counterpart of decodeProvideLiquidityMap; the Option-typed auto_unstake_* keys are
// not read, mirroring the String schema's dropped `auto unbonded` event.
func decodeWithdrawLiquidityMap(ev *events.Event, closedAt time.Time) (LiquidityChange, error) {
	addr, amount, err := mapBodyReaders(ev.Value)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity %w", err)
	}
	sender, err := addr(FieldWLSender)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity sender: %w", err)
	}
	shares, err := amount(FieldWLSharesAmount)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity shares_amount: %w", err)
	}
	returnA, err := amount(FieldWLReturnAmountA)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity return_amount_a: %w", err)
	}
	returnB, err := amount(FieldWLReturnAmountB)
	if err != nil {
		return LiquidityChange{}, fmt.Errorf("withdraw_liquidity return_amount_b: %w", err)
	}
	return LiquidityChange{
		Action:       EventActionWithdrawLiquidity,
		Pool:         ev.ContractID,
		Ledger:       ev.Ledger,
		TxHash:       ev.TxHash,
		OpIndex:      ev.OperationIndex,
		EventIndex:   ev.EventIndex,
		ClosedAt:     closedAt,
		Sender:       sender,
		SharesAmount: shares,
		AmountA:      returnA,
		AmountB:      returnB,
	}, nil
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.
//
// Each Phoenix swap event's body is a raw single-value SCVal, not wrapped in a Vec or Map: the
// pool publishes a scalar and soroban-sdk serializes it directly (see test/fixtures/phoenix/).

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

// sdkDecodeI128 converts a bare ScvI128 body (offer, received, return, spread, referral amounts) to canonical.Amount.
func sdkDecodeI128(valueB64 string) (canonical.Amount, error) {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return canonical.Amount{}, fmt.Errorf("parse: %w", err)
	}
	return scval.AsAmountFromI128(sv)
}

// ─── provide_liquidity / withdraw_liquidity reassembly ──────────
//
// Both follow the same N-event-per-action pattern as swap, with bare single-value SCVal bodies
// (Address for asset/account fields, I128 for amounts). The dispatcher calls serially, so buffer
// access is single-threaded; the Decoder's mutex is belt-and-braces.

// RawProvideLiquidity is the partial set of 5 fields for one provide_liquidity call, slotted by topic[1]; ready when all 5 are non-nil.
type RawProvideLiquidity struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex is the in-op index of the FIRST field-event of this provide_liquidity. It is
	// distinct per action and is the phoenix_liquidity PK discriminator, so two provides in one
	// op do not collide.
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

func (r *RawProvideLiquidity) slot(fieldTopic string) *events.Event {
	switch fieldTopic {
	case TopicSymbolPLSender:
		return r.Sender
	case TopicSymbolPLTokenA:
		return r.TokenA
	case TopicSymbolPLTokenAAmt:
		return r.TokenAAmount
	case TopicSymbolPLTokenB:
		return r.TokenB
	case TopicSymbolPLTokenBAmt:
		return r.TokenBAmount
	}
	return nil
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

// RawWithdrawLiquidity is the partial set of 4 required fields for one withdraw_liquidity call; the optional `auto unbonded` 5th event is dropped at classify time because the stake-contract `unbond` decoder already captures it.
type RawWithdrawLiquidity struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_liquidity PK,
	// migration 0060); first field-event's in-op index.
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

func (r *RawWithdrawLiquidity) slot(fieldTopic string) *events.Event {
	switch fieldTopic {
	case TopicSymbolWLSender:
		return r.Sender
	case TopicSymbolWLSharesAmount:
		return r.SharesAmount
	case TopicSymbolWLReturnAmountA:
		return r.ReturnAmountA
	case TopicSymbolWLReturnAmountB:
		return r.ReturnAmountB
	}
	return nil
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

// RawStake is the partial set of 3 fields for one bond / unbond call from the stake contract; the action is carried in the buffer's group key.
type RawStake struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_stake_events PK,
	// migration 0060); first field-event's in-op index.
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

func (r *RawStake) slot(fieldTopic string) *events.Event {
	switch fieldTopic {
	case TopicSymbolStakeUser:
		return r.User
	case TopicSymbolStakeToken:
		return r.Token
	case TopicSymbolStakeAmount:
		return r.Amount
	}
	return nil
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

// RawWithdrawRewards is the partial set of 2 fields (user, reward_token) for one withdraw_rewards call from the stake contract; the event has no amount field.
type RawWithdrawRewards struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex — per-event discriminator (phoenix_stake_events PK,
	// migration 0060); first field-event's in-op index.
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

func (r *RawWithdrawRewards) slot(fieldTopic string) *events.Event {
	switch fieldTopic {
	case TopicSymbolWRUser:
		return r.User
	case TopicSymbolWRRewardToken:
		return r.RewardToken
	}
	return nil
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

// decodeProvideLiquidity turns a complete RawProvideLiquidity into the LiquidityChange projected onto a phoenix_liquidity row.
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

// decodeWithdrawLiquidity turns a complete RawWithdrawLiquidity into a LiquidityChange: shares on the dedicated field, per-token
// returns in AmountA / AmountB. TokenA / TokenB stay empty (the event carries only the share-token); join provide_liquidity rows.
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

// decodeStake turns a complete RawStake into a StakeChange; bond and unbond share the shape, the action label comes from the group key.
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

// decodeWithdrawRewards turns a complete RawWithdrawRewards into a StakeChange. LPToken holds the reward-token address; Amount is
// left zero and persistPhoenixStake writes NULL rather than "0" for reward actions.
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

// decodeDistributeRewards decodes a single distribute_rewards event (one field, asset; no correlation buffer). Pool-wide: User
// stays empty.
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
