package canonical

import (
	"fmt"
	"math/big"
	"time"
)

// Trade is one executed trade, observed on a specific venue in a specific ledger.
//
// Storage identity is (Source, Ledger, TxHash, OpIndex, Timestamp), mirroring the `trades`
// hypertable primary key, so a persisted row always produces the same ID.
//
// Amounts are [Amount] (big.Int-backed). Price is NOT stored: it is derived from
// QuoteAmount / BaseAmount at query time, since a stored price would force a precision choice
// that belongs at the display layer.
//
// Invariant: BaseAmount >= 0, QuoteAmount >= 0, not both zero. A single zero leg is a real
// (unpriceable) fill; both zero is an ingestion bug.
type Trade struct {
	// Source is the connector name ("sdex", "soroswap", "aquarius",
	// "binance", …). Must be stable — it's part of the trade ID and
	// appears in API responses.
	Source string `json:"source"`

	// Ledger is the pubnet ledger sequence in which this trade closed.
	// Zero is invalid.
	Ledger uint32 `json:"ledger"`

	// TxHash is the 32-byte transaction hash, hex-encoded
	// (64 lowercase hex chars). Zero or malformed is invalid.
	TxHash string `json:"tx_hash"`

	// OpIndex is the 0-based operation index, fanned via FanoutOpIndex to also encode the in-op
	// event/claim index, so several trades from one operation never collide on the primary key.
	OpIndex uint32 `json:"op_index"`

	// Timestamp is the ledger close time (UTC). Millisecond precision
	// where the upstream event supplies it; otherwise second-precision
	// from ledger headers.
	Timestamp time.Time `json:"ts"`

	// Pair is the (base, quote) the trade executed against.
	// Direction matches the on-chain event — we do not normalise here.
	Pair Pair `json:"pair"`

	// BaseAmount is the quantity of the base asset that changed hands,
	// in the base asset's smallest unit (stroops for XLM,
	// 10^-decimals for SEP-41).
	BaseAmount Amount `json:"base_amount"`

	// QuoteAmount is the quantity of the quote asset paid for the
	// base, in the quote asset's smallest unit.
	QuoteAmount Amount `json:"quote_amount"`

	// Maker is the account that placed the resting offer (SDEX). Soroban DEX decoders leave it
	// empty ("unknown / not applicable") and carry only Taker. Not part of trade identity.
	Maker string `json:"maker,omitempty"`

	// Taker is the account that consumed the offer (SDEX), or on a
	// Soroban swap the on-chain recipient/caller address, which may be
	// a contract (C...) such as a router. Optional.
	Taker string `json:"taker,omitempty"`

	// RoutedVia is READ-SIDE attribution: the router (routers.name) whose same-tx invocation
	// drove this trade; empty means direct or not yet swept. Only storage readers populate it
	// from trades.routed_via. Decoders and ingest writers MUST leave it empty, because the
	// routed-via tagger applies it after insert (first-wins). Not part of trade identity.
	RoutedVia string `json:"routed_via,omitempty"`
}

// FanoutOpIndex packs an operation index (high 16 bits) and an in-operation event index (low 16)
// into the uint32 OpIndex slot, so several trades from ONE operation (a multi-hop swap) get
// distinct identities instead of colliding on the (source, ledger, tx_hash, op_index, ts) key
// and being silently dropped by the writer's ON CONFLICT. Both inputs are smallint on the wire,
// so neither overflows its half, and a projection re-derive reproduces the result exactly.
//
// Sources emitting one trade per op, or spacing op_index themselves (SDEX's *1024 stride), need
// not use it. Sources emitting one trade per Soroban contract event within an op (aquarius,
// comet) MUST, since the op index alone is not unique.
//
// It panics if either input is negative or exceeds 0xFFFF: masking an out-of-range value would
// manufacture the trade-ID collisions this exists to prevent, and is a decoder bug to surface.
func FanoutOpIndex(opIndex, eventIndex int) uint32 {
	if opIndex < 0 || opIndex > 0xFFFF {
		panic(fmt.Sprintf("canonical: FanoutOpIndex opIndex %d out of range [0, 65535]", opIndex))
	}
	if eventIndex < 0 || eventIndex > 0xFFFF {
		panic(fmt.Sprintf("canonical: FanoutOpIndex eventIndex %d out of range [0, 65535]", eventIndex))
	}
	return uint32(opIndex)<<16 | uint32(eventIndex)
}

// ID is the stable unique identifier used as the primary key in the
// trades hypertable and as the dedup key across region replicas.
//
// Format: `<source>:<ledger>:<tx_hash>:<op_index>:<unix_nanos_utc>`.
func (t Trade) ID() string {
	return fmt.Sprintf("%s:%d:%s:%d:%d",
		t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC().UnixNano())
}

// Validate returns nil iff every invariant holds; ErrInvalidTrade is always an upstream bug.
//
// Ledger is NOT required to be non-zero: off-chain venues have no ledger concept, stamp 0 and
// rely on Source + TxHash + OpIndex + Timestamp for uniqueness. A zero-ledger check would reject
// valid off-chain inserts, and TxHash validation already catches stub decoders.
//
// Amounts must be non-negative and not both zero. Exactly one zero leg is valid: SDEX settles
// fills whose base or quote rounded to zero stroops, real on-chain effects the served tier
// stores (unpriceable, so every price path filters them out).
func (t Trade) Validate() error {
	if t.Source == "" {
		return fmt.Errorf("%w: empty source", ErrInvalidTrade)
	}
	if !validTxHash(t.TxHash) {
		return fmt.Errorf("%w: tx_hash %q is not a 64-char hex string", ErrInvalidTrade, t.TxHash)
	}
	if t.Timestamp.IsZero() {
		return fmt.Errorf("%w: zero timestamp", ErrInvalidTrade)
	}
	if err := t.Pair.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTrade, err)
	}
	bs, qs := t.BaseAmount.Sign(), t.QuoteAmount.Sign()
	if bs < 0 {
		return fmt.Errorf("%w: base_amount must be non-negative, got %s", ErrInvalidTrade, t.BaseAmount)
	}
	if qs < 0 {
		return fmt.Errorf("%w: quote_amount must be non-negative, got %s", ErrInvalidTrade, t.QuoteAmount)
	}
	if bs == 0 && qs == 0 {
		return fmt.Errorf("%w: base_amount and quote_amount are both zero", ErrInvalidTrade)
	}
	return nil
}

// PriceRatio returns (numerator, denominator) = (QuoteAmount, BaseAmount), never nil. The
// *big.Int pair loses no precision; float conversion belongs only at the display edge.
func (t Trade) PriceRatio() (num, den *big.Int) {
	return t.QuoteAmount.BigInt(), t.BaseAmount.BigInt()
}

// Equal compares two trades by storage identity only. Fields like
// Maker/Taker are observational — two records of the same persisted row
// may differ on Maker if only one region's event included it.
func (t Trade) Equal(o Trade) bool {
	return t.Source == o.Source &&
		t.Ledger == o.Ledger &&
		t.TxHash == o.TxHash &&
		t.OpIndex == o.OpIndex &&
		t.Timestamp.UTC().Equal(o.Timestamp.UTC())
}

// ─── Internal helpers ──────────────────────────────────────────────

// validTxHash enforces the canonical lowercase 64-hex tx-hash form. Postgres treats "DEADBEEF..."
// and "deadbeef..." as distinct primary-key values, so mixed casing would land one on-chain tx
// as two rows. Sources MUST lowercase first; a rejection here is always an upstream bug.
func validTxHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
