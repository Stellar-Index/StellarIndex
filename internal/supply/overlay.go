package supply

import (
	"context"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// MetadataResolver reads SEP-1 `[[CURRENCIES]]` max_supply declarations for [Overlay] as a raw decimal string (ok=false when none); callers treat errors as "no overlay".
type MetadataResolver interface {
	SEP1MaxSupply(ctx context.Context, asset canonical.Asset) (raw string, ok bool, err error)
}

// Overlay applies the SEP-1 max_supply precedence (ADR-0011): operator override wins, then the SEP-1 declaration, else nil; XLM is never overlaid and junk declarations are ignored.
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
