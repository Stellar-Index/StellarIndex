package supply

import (
	"context"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// MetadataResolver reads SEP-1 `[[CURRENCIES]]` max_supply declarations for [Overlay];
// production is sep1DeclaredMaxResolver in internal/api/v1. SEP1MaxSupply returns the raw
// decimal string, ok=false when none is declared. Errors are treated as "no overlay" by callers.
// The resolver owns its caching.
type MetadataResolver interface {
	SEP1MaxSupply(ctx context.Context, asset canonical.Asset) (raw string, ok bool, err error)
}

// Overlay applies the SEP-1 max_supply precedence (ADR-0011): operator override (already in
// snap.MaxSupply) wins, then the SEP-1 declaration, else nil. XLM is hard-capped at total and
// never overlaid. Junk declarations are ignored (display value, not source of truth); only a
// SEP1MaxSupply error with ok=true is returned. On apply, MaxSupply and MaxSupplyBasis change.
func Overlay(ctx context.Context, snap Supply, asset canonical.Asset, resolver MetadataResolver) (Supply, bool, error) {
	// XLM and assets with operator-override max already set —
	// nothing to do.
	if snap.AssetKey == xlmAssetKey || snap.MaxSupply != nil {
		return snap, false, nil
	}
	if resolver == nil {
		return snap, false, nil
	}

	raw, ok, err := resolver.SEP1MaxSupply(ctx, asset)
	if err != nil {
		return snap, false, err
	}
	if !ok || raw == "" {
		return snap, false, nil
	}

	val, parsed := new(big.Int).SetString(raw, 10)
	if !parsed {
		return snap, false, nil
	}
	if val.Sign() <= 0 {
		// A zero or negative declared max would publish max < circulating; ignore it rather than serve
		// an issuer-poisoned number.
		return snap, false, nil
	}
	// A max_supply below realised supply is self-contradictory; treat it as unset. TOTAL is the
	// floor, not circulating (DOM-02): a value between the two would still assert max < total. Each
	// bound is checked because either may be nil.
	if snap.TotalSupply != nil && val.Cmp(snap.TotalSupply) < 0 {
		return snap, false, nil
	}
	if snap.CirculatingSupply != nil && val.Cmp(snap.CirculatingSupply) < 0 {
		return snap, false, nil
	}

	snap.MaxSupply = val
	// The max's provenance gets its own carrier: Basis still names the
	// policy behind total/circulating, and LowerBound is derived from it.
	snap.MaxSupplyBasis = BasisSEP1DeclaredMax
	return snap, true, nil
}
