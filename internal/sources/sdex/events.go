// Package sdex decodes classic-Stellar DEX trades from LedgerCloseMeta operation results.
// SDEX emits no contract events: trades are implicit in the results of ManageSellOffer /
// ManageBuyOffer / CreatePassiveSellOffer / PathPayment* operations. The dispatcher hands
// over the (op, result) pair via OpContext and we iterate `OffersClaimed` to yield one
// canonical.Trade per ClaimAtom (OrderBook, V0 and LiquidityPool), parsing XDR directly
// rather than via Horizon (ADR-0001).
package sdex

import "errors"

// SourceName is the canonical identifier for SDEX-produced trades.
// Stamped into canonical.Trade.Source and used for per-source
// metrics labels + lint-allowlist placement.
const SourceName = "sdex"

// Errors returned by the decode path.
var (
	// ErrUnknownClaimAtomType — a ClaimAtom.Type outside the three
	// known arms (OrderBook, LiquidityPool, V0 — all decoded, see
	// decode.go). Surfaced as a per-entry decode error so
	// sustained-rate alerts fire when a future claim type ships.
	ErrUnknownClaimAtomType = errors.New("sdex: unknown ClaimAtom type")

	// ErrMalformedClaimAtom — claim atom fields don't match the
	// expected shape (invalid asset, self-cross, unreachable
	// account). Usually indicates a protocol bump we haven't
	// audited.
	ErrMalformedClaimAtom = errors.New("sdex: malformed ClaimAtom")

	// ErrNoOpClaim — a claim atom with both legs non-positive. Not a decode
	// failure: the drop is deterministic and mirrored by sdexclaim.IsRealTrade.
	ErrNoOpClaim = errors.New("sdex: both-zero no-op claim")

	// ErrClaimIndexOverflow — a claim atom's position reached opIndexFanoutStride. OpIndex is
	// opIdx*stride+tradeIndex (decode.go), so an index at or past the stride aliases into the
	// next op's range, and because the trades upsert is ON CONFLICT DO UPDATE it would
	// silently overwrite an unrelated trade. Surfaced as a per-entry decode error so
	// SourceDecodeErrorsTotal catches it.
	ErrClaimIndexOverflow = errors.New("sdex: claim index exceeds op_index fanout stride")
)
