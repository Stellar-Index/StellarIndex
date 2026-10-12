package clickhouse

import (
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SupplyFlowTopic0Syms are the topic[0] symbols that change token supply under CAP-67/SEP-41 (mint adds, burn/clawback removes); supply = Σmint − Σburn − Σclawback over all history.
var SupplyFlowTopic0Syms = []string{"mint", "burn", "clawback"}

// IsSupplyFlowSym reports whether a topic[0] symbol is a supply-affecting event.
func IsSupplyFlowSym(sym string) bool {
	switch sym {
	case "mint", "burn", "clawback":
		return true
	default:
		return false
	}
}

// DecodeSupplyAmount extracts the positive magnitude from a mint/burn/clawback data body (bare i128 or the SEP-41/CAP-67 map) as a *big.Int (ADR-0003); ok=false with a reason when undecodable.
func DecodeSupplyAmount(sv xdr.ScVal) (*big.Int, string, bool) {
	if amt, err := scval.AsAmountFromI128(sv); err == nil {
		return amt.BigInt(), "", true
	}
	if sv.Type == xdr.ScValTypeScvMap {
		entries, merr := scval.AsMap(sv)
		if merr != nil {
			return nil, "map-parse-error", false
		}
		amtVal, ok := scval.MapField(entries, "amount")
		if !ok {
			return nil, "map-no-amount", false
		}
		if amt, aerr := scval.AsAmountFromI128(amtVal); aerr == nil {
			return amt.BigInt(), "", true
		}
		return nil, "map-amount-not-i128", false
	}
	return nil, sv.Type.String(), false
}

// DecodeSupplyAmountXDR is DecodeSupplyAmount for a base64 scval data body, the form the CH-reading supply tools use.
func DecodeSupplyAmountXDR(dataXDR string) (*big.Int, string, bool) {
	sv, err := scval.Parse(dataXDR)
	if err != nil {
		return nil, "parse-error", false
	}
	return DecodeSupplyAmount(sv)
}
