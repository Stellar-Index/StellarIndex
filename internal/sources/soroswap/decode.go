package soroswap

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// RawPair is what a dispatched Soroban event tells us about a
// swap/sync pair BEFORE we decode the SCVal payload. We collect
// these by (ledger, tx_hash, op_index) key, then finalise once we
// have both the swap + its sync.
type RawPair struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	Pair     string    // pair contract address (C…)
	ClosedAt time.Time // from event.ledgerClosedAt

	Swap *events.Event // populated when we see a swap
	Sync *events.Event // populated when we see a sync
}

// Complete reports whether this pair has both events and is ready
// to emit as a trade.
func (r RawPair) Complete() bool { return r.Swap != nil && r.Sync != nil }

// groupKey is the (ledger, tx_hash, op_index, pair_contract) tuple we
// use to correlate swap + sync. It's ordered so Go-map-ready.
//
// COR-08: the pair contract is part of the key. A router multi-hop
// swaps through SEVERAL pools inside ONE operation, so (ledger, tx,
// op) alone puts every pool's swap+sync in the same buffer slot. As
// long as each pool emitted swap-then-sync contiguously that happened
// to work — the slot completed and cleared before the next pool's
// swap. But nothing in the protocol guarantees that interleaving:
// pool A's swap followed by pool B's swap OVERWROTE the slot's Swap
// while leaving Pair pinned to A, so A's sync then completed a trade
// carrying B's amounts under A's pair identity — and therefore under
// A's token0/token1 mapping. Wrong assets, wrong price, silently.
// Keying on the emitter makes the correlation correct regardless of
// emission order; both correlated kinds are gated to the same pair
// contract by Matches, so this can never split a real pair.
type groupKey struct {
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	Pair    string
}

func keyOf(e *events.Event) groupKey {
	return groupKey{Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex), Pair: e.ContractID}
}

// classify decides what kind of Soroswap event this is. Soroswap
// topics are 2-tuples:
//
//	topic[0] = String("SoroswapPair" | "SoroswapFactory")
//	topic[1] = Symbol("swap" | "sync" | "new_pair" | …)
//
// Both positions are compared as byte-equal base64 against the
// constants computed at package init.
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
	// LP-share (pair-token) SEP-41 events: a Soroswap pair is also the
	// SEP-41 token of its own LP shares and emits Symbol-topic[0]
	// transfer/mint/burn/approve alongside the String-prefixed protocol
	// namespace above. Enumerated so classify() covers the pair WASM's
	// full event surface (EVERY-event principle) — Matches() maps this
	// kind to false on purpose; see its EventPairToken arm.
	switch e.Topic[0] {
	case TopicSymbolLPTransfer, TopicSymbolLPMint, TopicSymbolLPBurn, TopicSymbolLPApprove:
		return EventPairToken
	}
	return ""
}

// decodeSwap turns a swap+sync RawPair into a canonical.Trade.
//
// Contract reference (pair/src/event.rs):
//
//	SwapEvent  { to: Address, amount_0_in: i128, amount_1_in: i128,
//	             amount_0_out: i128, amount_1_out: i128 }
//	SyncEvent  { new_reserve_0: i128, new_reserve_1: i128 }
//
// Both are #[contracttype] structs → on the wire they're ScvMap with
// field-name Symbol keys. We pull the four swap amount fields by
// name (per contract-schema-evolution.md's decode-by-name rule) and
// derive trade direction from which side has a positive `in` +
// matching positive `out`.
//
// Token0 / token1 asset identities come from the factory's new_pair
// event for this pair; the consumer holds that mapping in an
// in-memory cache.
func decodeSwap(r RawPair, tok0, tok1 canonical.Asset) (canonical.Trade, error) {
	if !r.Complete() {
		return canonical.Trade{}, ErrSwapWithoutSync
	}
	if r.Swap == nil {
		return canonical.Trade{}, fmt.Errorf("%w: swap nil", ErrMalformedPayload)
	}

	amounts, err := decodeSwapAmounts(r.Swap.Value)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	// Taker: SwapEvent.to — the swap recipient, on-chain in EVERY
	// soroswap swap since genesis but dropped by this decoder until
	// 2026-07-30 (found by the address-intelligence build: soroswap was
	// the ONE venue with 0% trades.taker coverage while every sibling
	// had 100%). Best-effort per the Trade.Taker contract: a decode
	// failure leaves it empty rather than failing the trade.
	taker := decodeSwapTaker(r.Swap.Value)

	// Trade direction: whichever side had non-zero `in` is the base
	// asset the trader sold; the other side is what they bought. A
	// PRICE-FORMING swap has a cross-token pair non-zero — 0→1 or
	// 1→0. The chain does NOT guarantee that shape: pair.swap() is
	// directly invokable (Uniswap-v2-style) and accepts any argument
	// combination that keeps K non-decreasing, so a swap whose value
	// movement is confined to one token side settles successfully
	// (real case: ledger 57,403,300). Those decode cleanly but are
	// not trades — surfaced as ErrNonDirectionalSwap, which Decode
	// treats as a recognized no-op rather than a decode error.
	var base, quote canonical.Asset
	var baseAmt, quoteAmt canonical.Amount
	// A price-forming swap has EXACTLY one non-zero `in` and one non-zero
	// `out`, on OPPOSITE token sides (0→1 or 1→0). Match that shape
	// exactly. Any other combination has no single derivable direction,
	// so refuse the whole event as a recognized no-op (honest-blind beats
	// half-decoded):
	//   - single-sided / no cross-token move → ErrNonDirectionalSwap
	//     (real case: ledger 57,403,300).
	//   - three OR four amounts non-zero → the first matching arm below
	//     would silently drop the extra leg and report a GROSS `in` as if
	//     it were the net trade, fabricating a price into VWAP/OHLC. The
	//     four-leg case was closed 2026-07-31; the three-leg sibling
	//     (e.g. 0_in,1_in,1_out non-zero) slipped past that all-four-only
	//     guard until audit 2026-08-03. Both → ErrAmbiguousSwapDirection.
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
		// Fan out by the swap event's index: a router multi-hop invokes
		// several pairs as sub-calls of ONE op, so each pair's trade
		// shares op_index and would collide on the trades PK (ADR-0033,
		// same class as aquarius/comet). r.Swap is the swap event.
		OpIndex:     canonical.FanoutOpIndex(int(r.OpIndex), r.Swap.EventIndex),
		Timestamp:   r.ClosedAt,
		Pair:        pair,
		BaseAmount:  baseAmt,
		QuoteAmount: quoteAmt,
		Taker:       taker,
	}, nil
}

// decodeSwapTaker pulls SwapEvent.to as a strkey. Empty on any failure —
// Taker is optional on canonical.Trade and an unreadable recipient must
// not drop the trade itself.
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
)

// sdkDecodeSwapAmounts decodes the SwapEvent body. Pulls all four
// amount fields by name from the top-level Map — positional decode
// would break silently if Soroswap adds a field in a future upgrade.
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

// NewPairFields is the decoded NewPairEvent — emitted by the factory
// each time a pair contract is deployed. We use it to populate the
// pair→(token0, token1) registry that decodeSwap depends on.
//
// Contract reference (factory/src/event.rs:19-43):
//
//	NewPairEvent {
//	    token_0: Address,
//	    token_1: Address,
//	    pair:    Address,
//	    new_pairs_length: u32,
//	}
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

// SkimFields is the decoded SkimEvent — emitted by a pair contract
// when a caller invokes `skim()` to claim the excess balance the
// pool's reserves don't account for (Uniswap-v2-style mechanism;
// the difference between the contract's actual token balance and the
// stored reserves goes to the caller's chosen address).
//
// Contract reference (pair/src/event.rs — Phase-1 capture in
// docs/discovery/dexes-amms/soroswap.md §"SoroswapPair, skim"):
//
//	struct SkimEvent { skimmed_0: i128, skimmed_1: i128 }
//
// To stay decode-by-name (per contract-schema-evolution.md), the
// decoder tolerates both the documented field names AND the common
// Uniswap-v2-style `amount_0` / `amount_1` aliases — if a future
// WASM upgrade renames the fields the decoder still produces a row
// rather than silently dropping. A `to` Address field is decoded
// when present (some Uniswap-v2 ports surface the recipient in the
// body) and left empty when absent — current Soroswap WASM omits
// it.
type SkimFields struct {
	// Amount0 is the token0 excess transferred out of the pool.
	Amount0 canonical.Amount
	// Amount1 is the token1 excess transferred out of the pool.
	Amount1 canonical.Amount
	// To is the C-strkey (contract) / G-strkey (account) of the
	// skim recipient, when the contract surfaces it in the body.
	// Empty string when the field is absent — discovery-doc Soroswap
	// `SkimEvent` carries no `to`; nullable in storage so we don't
	// fabricate a value.
	To string
}

// sdkDecodeSkim decodes a SkimEvent body. Same Map-by-field-name
// path as sdkDecodeSwapAmounts; tolerant of both the
// `skimmed_0`/`skimmed_1` field names from the Phase-1 audit and the
// `amount_0`/`amount_1` alias the upstream Uniswap-v2 derivative
// uses in some forks. The `to` Address is optional — absent on
// today's Soroswap WASM, populated if a future upgrade adds it.
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

	// Optional `to` — silently absent on current Soroswap WASM, so a
	// missing field is fine; only a present-but-wrong-shape value is
	// an error worth surfacing.
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

// skimAmountField looks up an i128 amount field by its primary name
// and, if absent, falls back to an alias (the Uniswap-v2 derivative
// naming). Either resolves to an i128 amount or returns an error
// when both names are missing or the value isn't an i128.
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
