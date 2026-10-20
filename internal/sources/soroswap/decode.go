package soroswap

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// RawPair is what dispatched Soroban events tell us about a swap/sync pair before SCVal decode, collected by (ledger, tx_hash, op_index) and finalised once both the swap and its sync are present.
type RawPair struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	Pair     string    // pair contract address (C…)
	ClosedAt time.Time // from event.ledgerClosedAt

	Swap *events.Event // populated when we see a swap
	Sync *events.Event // populated when we see a sync
}

// Complete reports whether the pair has both swap and sync and is ready to emit as a trade.
func (r RawPair) Complete() bool { return r.Swap != nil && r.Sync != nil }

// groupKey is the (ledger, tx_hash, op_index, pair_contract) tuple that correlates swap + sync.
// The pair contract must be in the key: a router multi-hop swaps through SEVERAL pools in ONE op, with no guarantee of
// contiguous swap-then-sync per pool; without it pool B's swap would overwrite A's slot and A's sync would complete a
// trade with B's amounts. Two swaps through the SAME pair in one op share a key; buffer.absorb rotates on an occupied slot.
type groupKey struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	Pair    string
}

func keyOf(e *events.Event) groupKey {
	return groupKey{Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex), Pair: e.ContractID}
}

// classify decides the Soroswap event kind. topic[0] is String("SoroswapPair" | "SoroswapFactory"), topic[1] the event
// Symbol, both compared as byte-equal base64 against constants computed at package init.
func classify(e *events.Event) string {
	if len(e.Topic) < 2 {
		return ""
	}
	// Only pair-contract events are per-pair; factory events go
	// through classifyFactory.
	if e.Topic[0] == TopicPrefixPair {
		switch e.Topic[1] {
		case TopicSymbolSwap:
			return EventSwap
		case TopicSymbolSync:
			return EventSync
		case TopicSymbolDeposit:
			return EventDeposit
		case TopicSymbolWithdraw:
			return EventWithdraw
		case TopicSymbolSkim:
			return EventSkim
		}
		return ""
	}
	if e.Topic[0] == TopicPrefixFactory && e.Topic[1] == TopicSymbolNewPair {
		return EventNewPair
	}
	// A pair is also the SEP-41 token of its own LP shares and emits Symbol-topic[0]
	// transfer/mint/burn/approve. They are classified to cover the full event surface; Matches()
	// returns false for this kind on purpose (see its EventPairToken arm).
	switch e.Topic[0] {
	case TopicSymbolLPTransfer, TopicSymbolLPMint, TopicSymbolLPBurn, TopicSymbolLPApprove:
		return EventPairToken
	}
	return ""
}

// decodeSwap turns a swap+sync RawPair into a canonical.Trade. SwapEvent {to, amount_{0,1}_{in,out}: i128} and SyncEvent
// {new_reserve_{0,1}: i128} are ScvMaps with Symbol keys, so amounts are pulled by name (decode-by-name rule,
// ingest-pipeline.md#contract-schema-evolution); direction is the side with a positive `in` and matching positive `out`.
// Token identities come from the registry of new_pair events.
func decodeSwap(r RawPair, tok0, tok1 canonical.Asset) (canonical.Trade, error) {
	if !r.Complete() {
		return canonical.Trade{}, ErrSwapWithoutSync
	}
	return decodeSwapLeg(r, tok0, tok1)
}

// decodeSwapLeg decodes the trade from r.Swap alone; sync only carries post-state reserves. The buffer yields a swap-only
// group when a second swap through the same pair in one op rotates it out (buffer.absorb).
func decodeSwapLeg(r RawPair, tok0, tok1 canonical.Asset) (canonical.Trade, error) {
	if r.Swap == nil {
		return canonical.Trade{}, fmt.Errorf("%w: swap nil", ErrMalformedPayload)
	}

	amounts, err := decodeSwapAmounts(r.Swap.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	// Taker is SwapEvent.to, present in every swap. Best-effort per the Trade.Taker contract: a
	// decode failure leaves it empty rather than failing the trade.
	taker := decodeSwapTaker(r.Swap.Value)

	// The side with non-zero `in` is the base the trader sold. A price-forming swap has EXACTLY one
	// non-zero `in` and one non-zero `out` on OPPOSITE token sides; the chain does not guarantee
	// that, since pair.swap() is directly invokable and settles any mix that keeps K
	// non-decreasing. Any other shape has no single direction, so the event is refused
	// (honest-blind beats half-decoded):
	//   - single-sided / no cross-token move: ErrNonDirectionalSwap, a recognized no-op that Decode
	//     does not treat as an error (real case: ledger 57,403,300).
	//   - three OR four amounts non-zero: a matching arm would drop the extra leg and report a
	//     GROSS `in` as the net trade, fabricating a price into VWAP/OHLC. Returns
	//     ErrAmbiguousSwapDirection.
	var base, quote canonical.Asset
	var baseAmt, quoteAmt canonical.Amount
	in0, in1 := amounts.Amount0In.Sign() > 0, amounts.Amount1In.Sign() > 0
	out0, out1 := amounts.Amount0Out.Sign() > 0, amounts.Amount1Out.Sign() > 0
	switch {
	case in0 && out1 && !in1 && !out0:
		base, baseAmt = tok0, amounts.Amount0In
		quote, quoteAmt = tok1, amounts.Amount1Out
	case in1 && out0 && !in0 && !out1:
		base, baseAmt = tok1, amounts.Amount1In
		quote, quoteAmt = tok0, amounts.Amount0Out
	default:
		refuse := ErrNonDirectionalSwap
		legs := 0
		for _, nz := range []bool{in0, in1, out0, out1} {
			if nz {
				legs++
			}
		}
		if legs >= 3 {
			refuse = ErrAmbiguousSwapDirection
		}
		return canonical.Trade{}, fmt.Errorf("%w: in=(%s,%s) out=(%s,%s)",
			refuse,
			amounts.Amount0In, amounts.Amount1In,
			amounts.Amount0Out, amounts.Amount1Out)
	}

	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source: SourceName,
		Ledger: r.Ledger,
		TxHash: r.TxHash,
		// Fan out by the swap event's index: a router multi-hop invokes several pairs as sub-calls of ONE op, so each pair's
		// trade shares op_index and would collide on the trades PK (ADR-0033, same class as aquarius/comet). r.Swap is the swap
		// event.
		OpIndex:     canonical.FanoutOpIndex(int(r.OpIndex), r.Swap.EventIndex),
		Timestamp:   r.ClosedAt,
		Pair:        pair,
		BaseAmount:  baseAmt,
		QuoteAmount: quoteAmt,
		Taker:       taker,
	}, nil
}

// decodeSwapTaker pulls SwapEvent.to as a strkey; empty on any failure, since Taker is optional and must not drop the trade.
func decodeSwapTaker(valueB64 string) string {
	sv, err := scval.Parse(valueB64)
	if err != nil {
		return ""
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		return ""
	}
	toSv, ok := scval.MapField(entries, "to")
	if !ok {
		return ""
	}
	addr, err := scval.AsAddressStrkey(toSv)
	if err != nil {
		return ""
	}
	return addr
}

// swapAmounts holds the four i128 amounts from a SwapEvent body.
type swapAmounts struct {
	Amount0In  canonical.Amount
	Amount1In  canonical.Amount
	Amount0Out canonical.Amount
	Amount1Out canonical.Amount
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.

var (
	decodeSwapAmounts = sdkDecodeSwapAmounts
	decodeNewPair     = sdkDecodeNewPair
	decodeSkim        = sdkDecodeSkim
	decodeLiquidity   = sdkDecodeLiquidity
)

// LiquidityFields is the decoded body shared by pair `deposit` and `withdraw` (one #[contracttype] struct: amount_0, amount_1, liquidity, new_reserve_0, new_reserve_1 as i128, plus `to` Address).
type LiquidityFields struct {
	Amount0     canonical.Amount // token0 added (deposit) / removed (withdraw)
	Amount1     canonical.Amount // token1 added (deposit) / removed (withdraw)
	Liquidity   canonical.Amount // LP shares minted (deposit) / burned (withdraw)
	NewReserve0 canonical.Amount // post-state reserve0
	NewReserve1 canonical.Amount // post-state reserve1
	To          string           // LP provider strkey
}

// sdkDecodeLiquidity decodes a pair deposit/withdraw body by field name; a positional decode would
// break silently if a WASM upgrade reorders or adds a field (decode-by-name rule).
func sdkDecodeLiquidity(valueB64 string) (LiquidityFields, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return LiquidityFields{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return LiquidityFields{}, fmt.Errorf("body not a Map: %w", err)
	}
	var out LiquidityFields
	for _, field := range []struct {
		name string
		dst  *canonical.Amount
	}{
		{"amount_0", &out.Amount0},
		{"amount_1", &out.Amount1},
		{"liquidity", &out.Liquidity},
		{"new_reserve_0", &out.NewReserve0},
		{"new_reserve_1", &out.NewReserve1},
	} {
		sv, err := scval.MustMapField(entries, field.name)
		if err != nil {
			return LiquidityFields{}, fmt.Errorf("LiquidityEvent.%s: %w", field.name, err)
		}
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return LiquidityFields{}, fmt.Errorf("LiquidityEvent.%s: %w", field.name, err)
		}
		*field.dst = amt
	}
	// `to` (the LP provider) is always present here, so a missing value is an error, unlike skim.
	toSv, err := scval.MustMapField(entries, "to")
	if err != nil {
		return LiquidityFields{}, fmt.Errorf("LiquidityEvent.to: %w", err)
	}
	toStr, err := scval.AsAddressStrkey(toSv)
	if err != nil {
		return LiquidityFields{}, fmt.Errorf("LiquidityEvent.to: %w", err)
	}
	out.To = toStr
	return out, nil
}

// sdkDecodeSwapAmounts decodes the SwapEvent body, pulling the four amounts by name because a
// positional decode would break silently if Soroswap adds a field.
func sdkDecodeSwapAmounts(valueB64 string) (swapAmounts, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return swapAmounts{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return swapAmounts{}, fmt.Errorf("body not a Map: %w", err)
	}
	var out swapAmounts
	for _, field := range []struct {
		name string
		dst  *canonical.Amount
	}{
		{"amount_0_in", &out.Amount0In},
		{"amount_1_in", &out.Amount1In},
		{"amount_0_out", &out.Amount0Out},
		{"amount_1_out", &out.Amount1Out},
	} {
		sv, err := scval.MustMapField(entries, field.name)
		if err != nil {
			return swapAmounts{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return swapAmounts{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		*field.dst = amt
	}
	return out, nil
}

// NewPairFields is the decoded factory NewPairEvent (token_0, token_1, pair, new_pairs_length: u32), which populates the pair->(token0, token1) registry that decodeSwap depends on.
type NewPairFields struct {
	Token0 canonical.Asset // from Address
	Token1 canonical.Asset
	Pair   string // C-strkey
}

// sdkDecodeNewPair decodes a factory NewPairEvent body. Same
// Map-by-field-name path as sdkDecodeSwapAmounts.
func sdkDecodeNewPair(valueB64 string) (NewPairFields, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return NewPairFields{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return NewPairFields{}, fmt.Errorf("body not a Map: %w", err)
	}
	decodeAddrField := func(name string) (string, error) {
		sv, err := scval.MustMapField(entries, name)
		if err != nil {
			return "", err
		}
		return scval.AsAddressStrkey(sv)
	}
	t0, err := decodeAddrField("token_0")
	if err != nil {
		return NewPairFields{}, fmt.Errorf("NewPairEvent.token_0: %w", err)
	}
	t1, err := decodeAddrField("token_1")
	if err != nil {
		return NewPairFields{}, fmt.Errorf("NewPairEvent.token_1: %w", err)
	}
	pairAddr, err := decodeAddrField("pair")
	if err != nil {
		return NewPairFields{}, fmt.Errorf("NewPairEvent.pair: %w", err)
	}
	// Soroban tokens → canonical.NewSorobanAsset. Native XLM has
	// its own SAC contract but callers handle the native case via
	// asset resolution; at the decoder layer every address is a
	// contract, so NewSorobanAsset is the right constructor.
	a0, err := canonical.NewSorobanAsset(t0)
	if err != nil {
		return NewPairFields{}, fmt.Errorf("token_0 asset: %w", err)
	}
	a1, err := canonical.NewSorobanAsset(t1)
	if err != nil {
		return NewPairFields{}, fmt.Errorf("token_1 asset: %w", err)
	}
	return NewPairFields{Token0: a0, Token1: a1, Pair: pairAddr}, nil
}

// SkimFields is the decoded pair SkimEvent (skimmed_0, skimmed_1: i128), decoded by name tolerating the `amount_0` / `amount_1` aliases so a WASM rename still yields a row; `to` is empty when absent, as current WASM omits it.
type SkimFields struct {
	// Amount0 is the token0 excess transferred out of the pool.
	Amount0 canonical.Amount
	// Amount1 is the token1 excess transferred out of the pool.
	Amount1 canonical.Amount
	// To is the skim recipient C- or G-strkey when the body carries it, else empty. It is
	// nullable in storage so no value is fabricated.
	To string
}

// sdkDecodeSkim decodes a SkimEvent body by field name, accepting `skimmed_0`/`skimmed_1` and the
// `amount_0`/`amount_1` alias. The `to` Address is optional.
func sdkDecodeSkim(valueB64 string) (SkimFields, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return SkimFields{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SkimFields{}, fmt.Errorf("body not a Map: %w", err)
	}

	amt0, err := skimAmountField(entries, "skimmed_0", "amount_0")
	if err != nil {
		return SkimFields{}, fmt.Errorf("SkimEvent.skimmed_0: %w", err)
	}
	amt1, err := skimAmountField(entries, "skimmed_1", "amount_1")
	if err != nil {
		return SkimFields{}, fmt.Errorf("SkimEvent.skimmed_1: %w", err)
	}

	// Optional `to`: absence is fine; only a present-but-wrong-shape value is an error.
	var to string
	if sv, ok := scval.MapField(entries, "to"); ok {
		toStr, err := scval.AsAddressStrkey(sv)
		if err != nil {
			return SkimFields{}, fmt.Errorf("SkimEvent.to: %w", err)
		}
		to = toStr
	}

	return SkimFields{Amount0: amt0, Amount1: amt1, To: to}, nil
}

// skimAmountField looks up an i128 amount by its primary name, falling back to an alias (Uniswap-v2 derivative naming);
// errors when both are missing or the value isn't an i128.
func skimAmountField(entries []scval.ScMapEntry, primary, alias string) (canonical.Amount, error) {
	sv, ok := scval.MapField(entries, primary)
	if !ok {
		sv, ok = scval.MapField(entries, alias)
	}
	if !ok {
		return canonical.Amount{}, fmt.Errorf("neither %q nor %q present", primary, alias)
	}
	return scval.AsAmountFromI128(sv)
}
