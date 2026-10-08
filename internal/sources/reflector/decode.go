package reflector

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// opIndexFanoutStride spaces the synthetic op_index values of one event's price vector; 1024
// holds any observed vector (dozens of assets) with room to grow.
const opIndexFanoutStride = 1024

// eventFanoutStride bounds the contract events ONE operation can emit before their OpIndex blocks
// collide (and lose to ON CONFLICT DO NOTHING). OperationIndex alone collides two events in one op;
// EventIndex alone (per-operation) collides two ops. Packing both stays under uint32: ≈ 6.6M at 100 ops/tx.
const eventFanoutStride = 64

// opIndexFanoutMax bounds e.OperationIndex so the packed op_index stays
// within uint32: 2^32 / (eventFanoutStride * opIndexFanoutStride) = 65536.
const opIndexFanoutMax = (1 << 32) / (eventFanoutStride * opIndexFanoutStride)

// reflectorTopicArity is the minimum UpdateEvent topic count: ["REFLECTOR", "update", <timestamp: u64>].
const reflectorTopicArity = 3

// classify reports whether this is a REFLECTOR.update event. Topic arity is checked later, so a
// malformed one surfaces as ErrMalformedPayload rather than "not our event".
func classify(e *events.Event) bool {
	if len(e.Topic) < 2 {
		return false
	}
	return e.Topic[0] == TopicSymbolReflector &&
		e.Topic[1] == TopicSymbolUpdate
}

// decodeUpdate converts one REFLECTOR.update event into one canonical.OracleUpdate per (asset,
// price) slot, each with an OpIndex from its vector position. decimals is the contract's price scale.
// observer is empty in production: no NewDecoder call passes WithDecoderObserver, and events.Event
// carries no tx-source account to supply the relayer.
func decodeUpdate(e *events.Event, variant Variant, decimals uint8, observer string, closedAt time.Time) ([]canonical.OracleUpdate, error) {
	if !classify(e) {
		return nil, ErrNotReflectorEvent
	}
	if len(e.Topic) < reflectorTopicArity {
		return nil, fmt.Errorf("%w: expected %d topics (REFLECTOR, update, timestamp), got %d",
			ErrMalformedPayload, reflectorTopicArity, len(e.Topic))
	}

	prices, err := decodeUpdateBody(e.Value)
	if err != nil {
		// Double-%w: callers can errors.Is both ErrMalformedPayload and the specific cause.
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	if len(prices) == 0 {
		// An empty on-wire vector carries nothing to project: a recognised
		// no-op (as in redstone), not a decode error that blinds the ledger.
		return nil, nil
	}
	if err := checkFanoutBounds(e, len(prices)); err != nil {
		return nil, err
	}

	// topic[2] is u64 MILLISECONDS (verified on a mainnet capture; price_oracle.rs:74 divides by 1000
	// for seconds). On decode failure fall back to ledger close rather than drop the event's prices;
	// SafeUnixMillis clamps garbage to the ledger close so it cannot error the timestamptz INSERT.
	ts := closedAt
	if tsMs, terr := decodeUpdateTimestamp(e.Topic[2]); terr == nil {
		ts = canonical.SafeUnixMillis(tsMs, closedAt)
	}

	sourceName := variant.SourceName()
	out := make([]canonical.OracleUpdate, 0, len(prices))
	for i, entry := range prices {
		// Capture-totality: an unmapped symbol is a raw:<symbol> entry at its own position `i`, so no
		// row's OpIndex depends on the allow-list (see PriceEntry).
		if entry.Price.Sign() <= 0 {
			// The contract filters zero prices (oracle/src/events.rs:24); this is defensive. WARN, not
			// SourceUnknownSymbolsTotal: a non-positive price breaks a contract invariant, not allow-list
			// coverage, and must not send operators to extend an allow-list.
			slog.Warn("reflector: skipping non-positive oracle price",
				"source", sourceName,
				"contract_id", e.ContractID,
				"ledger", e.Ledger,
				"tx_hash", e.TxHash,
				"asset", entry.Asset.String(),
			)
			continue
		}
		u := canonical.OracleUpdate{
			Source:     sourceName,
			ContractID: e.ContractID,
			Ledger:     e.Ledger,
			TxHash:     e.TxHash,
			// See eventFanoutStride for why both index dimensions are packed.
			OpIndex:   (uint32(e.OperationIndex)*eventFanoutStride+uint32(e.EventIndex))*opIndexFanoutStride + uint32(i),
			Timestamp: ts,
			Asset:     entry.Asset,
			Quote:     quoteForVariant(variant),
			Price:     entry.Price,
			Decimals:  decimals,
			Observer:  observer,
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		// Only when EVERY slot was non-positive; unmapped symbols are raw rows, not skips.
		return nil, ErrEmptyPrices
	}
	return out, nil
}

// checkFanoutBounds validates the three inputs to the synthetic OpIndex
// packing (OperationIndex*eventFanoutStride+EventIndex)*opIndexFanoutStride+i
// so it cannot wrap uint32 onto another event's block; redstone's twin
// applies the same three bounds.
func checkFanoutBounds(e *events.Event, priceCount int) error {
	if priceCount > opIndexFanoutStride {
		return fmt.Errorf("%w: got %d prices", ErrPriceVectorOverflow, priceCount)
	}
	if e.EventIndex < 0 || e.EventIndex >= eventFanoutStride {
		return fmt.Errorf("%w: got %d", ErrEventIndexOverflow, e.EventIndex)
	}
	if e.OperationIndex < 0 || e.OperationIndex >= opIndexFanoutMax {
		return fmt.Errorf("%w: got %d", ErrOperationIndexOverflow, e.OperationIndex)
	}
	return nil
}

// quoteForVariant returns the contract's SEP-40 base(): USD for CEX (CAFJ…) and FX (CBKG…)
// (ADR-0010 fiat sentinel); for DEX the pubnet USDC SAC, confirmed via simulateTransaction. The DEX
// quote stays that SAC, never fiat:USD: normalising a stablecoin at ingest would hide a depeg.
func quoteForVariant(v Variant) canonical.Asset {
	switch v {
	case VariantDEX:
		return dexBaseUSDC
	case VariantCEX, VariantFX:
		return usdFiat
	default:
		// Unreachable; default to the USD quote every real Reflector oracle uses.
		return usdFiat
	}
}

// dexBaseContractID is the pubnet USDC SAC the Reflector DEX oracle's
// base() returns.
const dexBaseContractID = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

// usdFiat / dexBaseUSDC are the CEX/FX and DEX quotes, parsed at init so a regression panics
// loudly instead of writing zero-asset oracle updates.
var (
	usdFiat     = mustUSDFiat()
	dexBaseUSDC = mustSorobanAsset(dexBaseContractID)
)

func mustSorobanAsset(contractID string) canonical.Asset {
	a, err := canonical.NewSorobanAsset(contractID)
	if err != nil {
		panic("reflector: NewSorobanAsset(" + contractID + ") must succeed: " + err.Error())
	}
	return a
}

func mustUSDFiat() canonical.Asset {
	a, err := canonical.NewFiatAsset("USD")
	if err != nil {
		panic("reflector: NewFiatAsset(\"USD\") must succeed: " + err.Error())
	}
	return a
}

// PriceEntry is one (asset, price) slot of update_data. sdkDecodeUpdateBody keeps exactly one per
// raw position (an unmapped symbol becomes `raw:<symbol>`): OpIndex derives from POSITION, so compacting
// would shift other rows' keys on an allow-list change; instead a re-derive promotes raw:X → crypto:X in place.
type PriceEntry struct {
	Asset canonical.Asset
	Price canonical.Amount
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.

var (
	decodeUpdateBody      = sdkDecodeUpdateBody
	decodeUpdateTimestamp = sdkDecodeUpdateTimestamp
)

// sdkDecodeUpdateBody decodes Event.Value (base64 SCVal) of Reflector's UpdateEvent
// (reflector-contract/oracle/src/events.rs:4-10, soroban-sdk 25.3.0). #[contractevent] wraps the
// non-topic field in a Map even when it is alone (pinned by test/fixtures/reflector/v6-*/):
//
//	Map { "update_data": Vec<(Val, i128)> }
//
// The field is looked up by name to survive benign additions across upgrades. Val is an ScAddress
// (a Soroban asset) or an ScSymbol, mapped via canonical.MapOracleSymbol or recorded as `raw:<symbol>`.
// Per ADR-0013 this is the only decoder path; tests override the package-level var.
func sdkDecodeUpdateBody(valueB64 string) ([]PriceEntry, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	// Outer shape: Map { "update_data": Vec<(Val, i128)> }.
	entries, err := scval.AsMap(body)
	if err != nil {
		return nil, fmt.Errorf("body not a Map: %w", err)
	}
	updateDataSv, err := scval.MustMapField(entries, "update_data")
	if err != nil {
		return nil, fmt.Errorf("body map missing update_data: %w", err)
	}
	pairs, err := scval.AsVec(updateDataSv)
	if err != nil {
		return nil, fmt.Errorf("update_data not a Vec: %w", err)
	}
	out := make([]PriceEntry, 0, len(pairs))
	for i, pair := range pairs {
		entry, err := decodeUpdateDataEntry(pair)
		if err != nil {
			return nil, fmt.Errorf("update_data[%d]: %w", i, err)
		}
		if !entry.Asset.IsMapped() {
			// Unmapped symbol: recorded as raw:<symbol> (see PriceEntry) and counted, since the alert on
			// this counter tells the allow-list owner. Label "reflector" because the decoder is shared by all three variants.
			obs.SourceUnknownSymbolsTotal.WithLabelValues("reflector").Inc()
		}
		out = append(out, entry)
	}
	return out, nil
}

// decodeUpdateDataEntry turns one (Val, i128) tuple into a PriceEntry, so per-slot failures carry their index.
func decodeUpdateDataEntry(pair scval.ScVal) (PriceEntry, error) {
	elts, err := scval.AsTupleN(pair, 2)
	if err != nil {
		return PriceEntry{}, fmt.Errorf("not a 2-tuple: %w", err)
	}
	kind, err := scval.DecodeAddressOrSymbol(elts[0])
	if err != nil {
		return PriceEntry{}, fmt.Errorf("asset: %w", err)
	}
	var asset canonical.Asset
	switch {
	case kind.Address != "":
		asset, err = canonical.NewSorobanAsset(kind.Address)
		if err != nil {
			return PriceEntry{}, fmt.Errorf("soroban asset from %q: %w", kind.Address, err)
		}
	case kind.Symbol != "":
		// Asset::Other covers fiat (FX) and crypto (CEX) tickers; MapOracleSymbol tries fiat, crypto,
		// then RWA, else returns a raw: asset. Its only error (unrepresentable symbol) is malformed.
		asset, err = canonical.MapOracleSymbol(kind.Symbol)
		if err != nil {
			return PriceEntry{}, fmt.Errorf("%w: symbol %q: %w", ErrMalformedPayload, kind.Symbol, err)
		}
	default:
		return PriceEntry{}, fmt.Errorf("%w: asset slot is neither Address nor Symbol",
			ErrMalformedPayload)
	}
	price, err := scval.AsAmountFromI128(elts[1])
	if err != nil {
		return PriceEntry{}, fmt.Errorf("price: %w", err)
	}
	return PriceEntry{Asset: asset, Price: price}, nil
}

// sdkDecodeUpdateTimestamp decodes Event.Topic[2] (SCVal U64) to the raw MILLISECONDS the contract
// emits; the caller passes it through canonical.SafeUnixMillis. Do NOT treat it as seconds.
func sdkDecodeUpdateTimestamp(topicB64 string) (uint64, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return 0, fmt.Errorf("parse topic[2]: %w", err)
	}
	return scval.AsU64(sv)
}
