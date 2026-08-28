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

// opIndexFanoutStride spaces the synthetic op_index values emitted
// from one Reflector event's price vector. 1024 comfortably holds
// any vector size we've observed (dozens of assets) with room to
// grow; well within uint32 since Stellar caps ops/tx at 100.
const opIndexFanoutStride = 1024

// eventFanoutStride bounds how many contract events ONE operation can
// emit before their per-event op_index blocks would collide.
// DAT-06/trap-15 (audit-2026-07-23): the fanout base used to be
// OperationIndex ALONE, so two Reflector update events within the
// SAME operation collided on their whole 1024-wide OpIndex block —
// the second event's rows silently landed on top of (or lost to
// ON CONFLICT DO NOTHING against) the first's. EventIndex ALONE isn't
// a safe replacement either: per events.Event's own doc it is scoped
// PER-OPERATION (resets to 0 for each new op), so swapping it straight
// in would instead collide two DIFFERENT operations that each emit a
// single event. Combining both dimensions — OperationIndex and
// EventIndex — keeps every event's OpIndex block disjoint regardless
// of whether the collision risk is same-op or cross-op. 64 is well
// beyond any observed Reflector-adjacent event fanout (single digits)
// and keeps the whole synthesized value comfortably inside uint32
// even at Stellar's 100-ops/tx cap: (100*64+63)*1024+1023 ≈ 6.6M.
const eventFanoutStride = 64

// reflectorTopicArity is the minimum topic count on a Reflector
// UpdateEvent: ["REFLECTOR", "update", <timestamp: u64>]. Anything
// shorter is by definition not our event.
const reflectorTopicArity = 3

// classify reports whether this is a Reflector "update" event. We
// match topic[0]=REFLECTOR + topic[1]=update; anything else returns
// false and the caller skips. We do NOT require topic[2] at this
// stage — a malformed topic arity is surfaced later as
// ErrMalformedPayload so the decode-errors metric catches it
// separately from the common "not our event" case.
func classify(e *events.Event) bool {
	if len(e.Topic) < 2 {
		return false
	}
	return e.Topic[0] == TopicSymbolReflector &&
		e.Topic[1] == TopicSymbolUpdate
}

// decodeUpdate converts one REFLECTOR.update event into a slice of
// canonical.OracleUpdate — one per (asset, price) pair in the
// event's update_data vector.
//
// Each OracleUpdate shares the same (ledger, tx_hash) but gets a
// distinct OpIndex derived from the vector index so identity stays
// unique in the oracle_updates hypertable.
//
// variant determines the source-name to stamp; decimals is the
// contract-declared price scale (typically 14); observer is an
// optional relayer strkey stamped on every emitted row.
//
// NOTE: observer is NOT populated in production. No production
// NewDecoder call passes WithDecoderObserver (see
// internal/pipeline/dispatcher.go), and the per-tx relayer — the
// update tx's source account — is not currently available to decoders
// at all: internal/events/event.go's Event carries no
// tx-source-account field. Capturing the relayer would first require
// plumbing that field through events.Event (a noted follow-up); until
// then observer is empty on every production OracleUpdate.
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
		// Double-%w preserves both sentinels — callers can
		// errors.Is against ErrMalformedPayload (for the "any
		// decode problem" gate) AND against the specific wrapped
		// cause (for targeted ops tooling).
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	if len(prices) == 0 {
		return nil, ErrEmptyPrices
	}
	if len(prices) > opIndexFanoutStride {
		// Refuse rather than silently emitting PK-colliding rows.
		// See ErrPriceVectorOverflow for rationale.
		return nil, fmt.Errorf("%w: got %d prices", ErrPriceVectorOverflow, len(prices))
	}
	if e.EventIndex < 0 || e.EventIndex >= eventFanoutStride {
		// See ErrEventIndexOverflow for rationale.
		return nil, fmt.Errorf("%w: got %d", ErrEventIndexOverflow, e.EventIndex)
	}

	// Timestamp: the contract puts it in topic[2] as u64
	// MILLISECONDS (not seconds — verified against mainnet capture
	// 2026-04-23 + reflector-contract/oracle/src/price_oracle.rs:74,
	// which divides by 1000 to expose seconds via `last_timestamp`.
	// Internal storage is ms; the event carries the raw internal
	// value).
	//
	// Fall back to ledger close time if the topic decode fails so an
	// isolated encoding quirk doesn't drop an entire event's worth
	// of price updates. canonical.SafeUnixMillis clamps sentinel /
	// garbage values (pre-epoch or far-future, incl. the >MaxInt64
	// wrap class of the router deadline_ts overflow) to the ledger
	// close so they can't error the timestamptz INSERT.
	ts := closedAt
	if tsMs, terr := decodeUpdateTimestamp(e.Topic[2]); terr == nil {
		ts = canonical.SafeUnixMillis(tsMs, closedAt)
	}

	sourceName := variant.SourceName()
	out := make([]canonical.OracleUpdate, 0, len(prices))
	for i, entry := range prices {
		// Oracle capture-totality (PR-2): there is no unknown-symbol
		// skip here any more. An unmapped symbol arrives from
		// sdkDecodeUpdateBody as a raw:<symbol> PriceEntry and is
		// emitted like any other slot, at the SAME vector position
		// `i` the pre-totality Skip placeholder used to hold (DAT-03:
		// no existing row's OpIndex moves — the raw row fills the
		// gap the placeholder left).
		if entry.Price.Sign() <= 0 {
			// Reflector filters zero-price entries at the contract
			// level (oracle/src/events.rs:24 — zero prices skipped
			// before publish), so in practice we should never see
			// one. Defensive skip keeps us correct if the contract
			// relaxes that filter.
			//
			// Surface the skip so a non-positive slot isn't dropped
			// invisibly (cf. the unmapped-symbol path, which records
			// the slot as raw: and increments
			// obs.SourceUnknownSymbolsTotal). A WARN log rather than
			// that counter: a non-positive price is a
			// contract-invariant violation, NOT an allow-list/coverage
			// gap, so it doesn't belong on the coverage-drift metric
			// that drives that counter's alert. Loud if it ever fires,
			// without misrouting operators toward extending an
			// allow-list.
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
			// OpIndex packs (OperationIndex, EventIndex, vector
			// position) — see eventFanoutStride's godoc for why both
			// index dimensions are needed, not just one.
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
		// Only reachable when EVERY slot was non-positive: since the
		// oracle capture-totality change an unmapped symbol is a raw
		// row, not a skip, so an all-unknown vector no longer lands
		// here.
		return nil, ErrEmptyPrices
	}
	return out, nil
}

// quoteForVariant returns the implicit quote-currency for a given
// Reflector contract. All three Reflector mainnet oracles denominate
// in USD-equivalent, so every variant stamps fiat:USD:
//
//   - CEX (CAFJ…) and FX (CBKG…) publish an explicit USD base per the
//     Reflector docs (ADR-0010 fiat sentinel).
//   - DEX (CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M)
//     denominates in USDC, NOT XLM. Confirmed 2026-07-07 by calling
//     the contract's SEP-40 base() method via simulateTransaction:
//     it returns Asset::Stellar(CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75)
//     — the pubnet USDC SAC (decimals()=14, resolution()=300 also
//     confirmed). Corroborating live evidence: the native-XLM SAC
//     (CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA) is
//     served at ~0.2002 (XLM in USD), not the 1.0 a true XLM-in-XLM
//     self-price would be; the USD stablecoins cluster at ~1.0; and
//     USDC itself is absent from the 41-asset feed (a base never
//     prices itself).
//
// Before this fix quoteForVariant(VariantDEX) returned
// canonical.NativeAsset(), mislabelling every DEX row's denominator
// as XLM (right magnitude, wrong quote). We stamp fiat:USD rather
// than crypto:USDC so all three variants share one quote
// representation and the /v1/oracle divergence path — which compares
// against our USD VWAP and does NOT translate USDC→USD in
// oracleAssetKeys (internal/divergence/oracle.go) — matches the
// stored rows. The aggregator's stablecoin map already treats USDC
// as USD, so this loses no information. Existing rows need a
// projector-replay to pick up the corrected quote.
func quoteForVariant(v Variant) canonical.Asset {
	switch v {
	case VariantDEX, VariantCEX, VariantFX:
		return usdFiat
	default:
		// Unknown variant should never occur; default to the USD
		// quote every real Reflector oracle uses rather than XLM.
		return usdFiat
	}
}

// usdFiat is the implicit USD quote for CEX/FX Reflector variants.
// Parsed once at package init: a regression that drops USD from the
// fiat allow-list fires a loud init panic instead of silently
// writing zero-asset oracle updates on every Reflector event.
var usdFiat = mustUSDFiat()

func mustUSDFiat() canonical.Asset {
	a, err := canonical.NewFiatAsset("USD")
	if err != nil {
		panic("reflector: NewFiatAsset(\"USD\") must succeed: " + err.Error())
	}
	return a
}

// PriceEntry is one (asset, price) pair from the update_data vector.
//
// sdkDecodeUpdateBody keeps the SLOT COUNT stable — exactly one
// PriceEntry per raw update_data[] position. An unmapped symbol
// yields a canonical.AssetOracleRaw entry (`raw:<symbol>`) rather
// than being compacted out (DAT-03, audit-2026-07-23): decodeUpdate's
// synthetic OpIndex is derived from vector POSITION, so dropping a
// slot from the slice would shift every OTHER entry's OpIndex
// whenever the allow-list changed, orphaning or duplicating rows on a
// re-derive instead of updating them in place. Before the oracle
// capture-totality change (PR-2) that stability came from a
// `Skip: true` placeholder that consumed the slot and emitted
// nothing; the raw row now consumes the same slot and IS emitted, so
// a later allow-list extension re-derives the same PK and promotes
// `raw:X` → `crypto:X` in place.
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

// sdkDecodeUpdateBody decodes Event.Value (base64 SCVal) into the
// payload emitted by Reflector's UpdateEvent.
//
// Contract reference: reflector-contract/oracle/src/events.rs:4-10
// (soroban-sdk 25.3.0 at VERSIONS.md's pinned SHA):
//
//	#[contractevent(topics = ["REFLECTOR", "update"])]
//	pub struct UpdateEvent {
//	    #[topic] timestamp: u64,
//	    update_data: Vec<(Val, i128)>,
//	}
//
// On the wire (verified 2026-04-23 against four mainnet DEX-oracle
// captures in test/fixtures/reflector/v6-2026-04-23/), the
// soroban-sdk #[contractevent] macro wraps non-topic fields in a
// Map keyed by field name — even when there is only one such
// field. So the body we receive is:
//
//	Map { "update_data": Vec<(Val, i128)> }
//
// NOT the raw Vec. We look up the field by name (per
// docs/architecture/contract-schema-evolution.md — decode-by-name-
// not-position lets us survive benign field additions across
// upgrades).
//
// `Val` in each pair is either:
//   - `ScAddress` — for Asset::Stellar(address); yields a
//     canonical.NewSorobanAsset(C-strkey).
//   - `ScSymbol`  — for Asset::Other(symbol); resolved through
//     canonical.MapOracleSymbol (fiat ADR-0010 → crypto ADR-0014 →
//     RWA ADR-0028), else recorded VERBATIM as `raw:<symbol>` so the
//     record layer stays total while operators decide whether to
//     extend an allow-list (docs/design/oracle-capture-totality-
//     design.md).
//
// Per ADR-0013 + contract-schema-evolution.md this is the ONLY
// decoder path; tests override via the package-level var, not by
// editing this function.
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
			// Unmapped symbol = gap in our canonical asset model,
			// not a structural event problem. The slot is recorded
			// verbatim as raw:<symbol> at its own vector position
			// (DAT-03 — see PriceEntry's godoc). F-1234 (codex
			// audit-2026-05-12): still count it on
			// stellarindex_source_unknown_symbols_total — a raw row
			// is a mapping gap the allow-list owner has to close,
			// and the alert on this counter is how they learn of
			// it. The metric label uses the generic "reflector"
			// because the decoder is shared across
			// reflector-dex/cex/fx; the parent dispatcher
			// attributes per-contract context if needed.
			obs.SourceUnknownSymbolsTotal.WithLabelValues("reflector").Inc()
		}
		out = append(out, entry)
	}
	return out, nil
}

// decodeUpdateDataEntry turns one 2-tuple of the Vec<(Val, i128)>
// payload into a PriceEntry. Splits the union-dispatch on the asset
// slot (Address | Symbol) out from the outer loop so per-slot
// failures carry a clear index in their wrapping error.
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
		// Reflector's Asset::Other(Symbol) covers both fiat tickers
		// (FX oracle: "USD", "EUR", "ARS" …) and crypto tickers
		// (CEX oracle: "BTC", "ETH", "USDT" …). The shared mapper
		// tries fiat (ADR-0010), then crypto (ADR-0014), then RWA
		// (ADR-0028); anything matching none is returned as a raw:
		// asset so the slot is recorded rather than dropped. The
		// only error is a symbol the raw validator cannot represent
		// (impossible for an ScSymbol) — refused as malformed.
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

// sdkDecodeUpdateTimestamp decodes Event.Topic[2] (base64 SCVal U64)
// into the raw uint64 the contract emits — MILLISECONDS, the
// contract's internal scale (see the topic[2] comment in decodeUpdate
// and oracle/src/price_oracle.rs, which divides by 1000 to expose
// seconds via last_timestamp). The caller passes the value through
// canonical.SafeUnixMillis; do NOT treat this return as seconds.
// Matches the #[topic] declaration on UpdateEvent.timestamp.
func sdkDecodeUpdateTimestamp(topicB64 string) (uint64, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return 0, fmt.Errorf("parse topic[2]: %w", err)
	}
	return scval.AsU64(sv)
}
