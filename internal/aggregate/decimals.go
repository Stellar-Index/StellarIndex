package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// StandardDecimals is the policy-default decimals of every DEX-traded Stellar asset; mirrors decimalsguard.StandardDecimals (duplicated to avoid an import).
const StandardDecimals = 7

// DecimalsLookup resolves an asset's confirmed non-standard decimals; found=false or a nil lookup means [StandardDecimals], never an error.
type DecimalsLookup interface {
	Lookup(assetID string) (decimals int, found bool)
}

// ResolveDecimals returns lookup's confirmed decimals for asset, or [StandardDecimals] when lookup is nil or has no row; it never errors.
func ResolveDecimals(lookup DecimalsLookup, asset canonical.Asset) int {
	if lookup == nil {
		return StandardDecimals
	}
	if d, ok := lookup.Lookup(asset.String()); ok {
		return d
	}
	return StandardDecimals
}

// DecimalsAdjustment returns the exact *big.Rat factor 10^(baseDecimals-quoteDecimals) that turns a raw quote/base ratio into the true price (ADR-0003).
func DecimalsAdjustment(baseDecimals, quoteDecimals int) *big.Rat {
	delta := baseDecimals - quoteDecimals
	if delta == 0 {
		return big.NewRat(1, 1)
	}
	abs := delta
	if abs < 0 {
		abs = -abs
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs)), nil)
	if delta > 0 {
		return new(big.Rat).SetInt(scale)
	}
	return new(big.Rat).SetFrac(big.NewInt(1), scale)
}

// AdjustPrice scales a raw quote/base ratio by [DecimalsAdjustment]; nil stays nil and equal decimals return raw itself, not a copy.
func AdjustPrice(raw *big.Rat, baseDecimals, quoteDecimals int) *big.Rat {
	if raw == nil {
		return nil
	}
	if baseDecimals == quoteDecimals {
		return raw
	}
	return new(big.Rat).Mul(raw, DecimalsAdjustment(baseDecimals, quoteDecimals))
}
