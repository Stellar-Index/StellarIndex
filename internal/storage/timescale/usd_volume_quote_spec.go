package timescale

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// USDVolumeQuoteSpec lets the trade-insert path recognise on-chain quote assets the operator
// declared USD-pegged and supplies their decimals for usd_volume.
//
// Off-chain CEX/FX sources use a uniform 10^8 scale and `aggregate.FiatProxy`, but on-chain
// trades use per-asset decimals and the quote is either `AssetClassic{Code, Issuer}` or
// `AssetSoroban{ContractID}`. Neither maps to a global ticker, so trust is keyed on the declared
// (CODE, ISSUER) pairs, and the SAC form resolves back to its classic asset via
// `[supply.sac_wrappers]`, never by code alone.
//
// Scope: USD-pegged, classic-decimal (7) only. Non-USD pegs live in [VWAPUSDFXResolver];
// SEP-41 tokens with other decimals are unsupported.
type USDVolumeQuoteSpec struct {
	// classicUSDPegs holds the operator-declared USD-pegged classic keys in "CODE-ISSUER" form.
	classicUSDPegs map[string]struct{}

	// sacToClassic maps SAC contract id to the classic "CODE-ISSUER" key (normalised from the
	// supply package's "CODE:ISSUER"), so a Soroban-form quote resolves to its classic asset for
	// the peg and decimals lookup.
	sacToClassic map[string]string
}

// NewUSDVolumeQuoteSpec composes the lookup. classicUSDPegs is the operator's
// `[trades].usd_pegged_classic_assets` list ("CODE-ISSUER" per entry); sacWrappers is the supply
// pipeline's map of SAC contract id to "CODE:ISSUER".
//
// It errors on an unparseable classicUSDPegs entry so a typo'd asset_key fails at startup instead
// of silently matching no trade.
func NewUSDVolumeQuoteSpec(classicUSDPegs []string, sacWrappers map[string]string) (*USDVolumeQuoteSpec, error) {
	out := &USDVolumeQuoteSpec{
		classicUSDPegs: make(map[string]struct{}, len(classicUSDPegs)),
		sacToClassic:   make(map[string]string, len(sacWrappers)),
	}
	for _, raw := range classicUSDPegs {
		asset, err := canonical.ParseAsset(raw)
		if err != nil {
			return nil, fmt.Errorf("usd-volume spec: classic peg %q: %w", raw, err)
		}
		if asset.Type != canonical.AssetClassic {
			return nil, fmt.Errorf("usd-volume spec: classic peg %q must be a classic asset (got %s)", raw, asset.Type)
		}
		out.classicUSDPegs[classicKey(asset)] = struct{}{}
	}
	for sacID, supplyKey := range sacWrappers {
		// supplyKey is the "CODE:ISSUER" colon form; normalise to the dash form of classicUSDPegs.
		asset, err := canonical.ParseAsset(supplyKey)
		if err != nil {
			return nil, fmt.Errorf("usd-volume spec: sac_wrapper for %s (%q): %w", sacID, supplyKey, err)
		}
		if asset.Type != canonical.AssetClassic {
			return nil, fmt.Errorf("usd-volume spec: sac_wrapper for %s (%q) must point at a classic asset (got %s)", sacID, supplyKey, asset.Type)
		}
		out.sacToClassic[sacID] = classicKey(asset)
	}
	return out, nil
}

// QuoteUSDPegInfo with ok=true says the quote asset is on the operator's USD-pegged list, so
// quote_amount divided by 10^decimals is its USD value at the trusted 1.0 peg. With ok=false the
// caller stores usd_volume as NULL. That includes a nil spec and SEP-41 contracts absent from
// sacWrappers (pure SEP-41 stablecoins are unsupported).
func (s *USDVolumeQuoteSpec) QuoteUSDPegInfo(asset canonical.Asset) (decimals int, ok bool) {
	if s == nil {
		return 0, false
	}
	// Only Classic and Soroban (SAC) assets can resolve to a pegged classic; the rest fall through
	// to (0, false).
	//exhaustive:ignore
	switch asset.Type {
	case canonical.AssetClassic:
		if _, pegged := s.classicUSDPegs[classicKey(asset)]; pegged {
			// Stellar classic credits are uniformly 7-decimal.
			return 7, true
		}
	case canonical.AssetSoroban:
		// A SAC inherits the peg and 7-decimal scale of its underlying classic asset.
		if classicKeyStr, ok := s.sacToClassic[asset.ContractID]; ok {
			if _, pegged := s.classicUSDPegs[classicKeyStr]; pegged {
				return 7, true
			}
		}
	}
	return 0, false
}

// classicKey renders a classic Asset as "CODE-ISSUER", the form operators write in TOML.
func classicKey(a canonical.Asset) string {
	return a.Code + "-" + a.Issuer
}
