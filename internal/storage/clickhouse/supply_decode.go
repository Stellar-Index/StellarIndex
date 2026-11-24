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

// Supply-flow skip reasons. Only SupplySkipNotTokenShaped is expected traffic: topic[0] alone does not identify a token, so any contract
// (an NFT's `mint` with a u32 id, say) can emit one, and a contract with no supply loses nothing.
const (
	SupplySkipNotTokenShaped  = "not_token_shaped"
	SupplySkipAmountNotI128   = "amount_not_i128"
	SupplySkipBodyUnparseable = "body_unparseable"
	// SupplySkipJudgedNonToken is a skip from a contract judged not a token (judgedNonToken), for that judgement's reason only.
	SupplySkipJudgedNonToken = "judged_non_token"
)

// SupplySkipReasons lists every reason ClassifySupplyEventXDR returns, for metric zero-seeding.
var SupplySkipReasons = []string{SupplySkipNotTokenShaped, SupplySkipAmountNotI128, SupplySkipBodyUnparseable, SupplySkipJudgedNonToken}

// ClassifySupplyBodyXDR type-tests a mint/burn/clawback body before reading its amount: a bare i128 or a map carrying `amount` is
// token-shaped; skip is "" when the amount decoded, else a SupplySkip* reason.
func ClassifySupplyBodyXDR(dataXDR string) (amount *big.Int, skip string) {
	sv, err := scval.Parse(dataXDR)
	if err != nil {
		return nil, SupplySkipBodyUnparseable
	}
	if sv.Type == xdr.ScValTypeScvI128 {
		amt, aerr := scval.AsAmountFromI128(sv)
		if aerr != nil {
			return nil, SupplySkipAmountNotI128
		}
		return amt.BigInt(), ""
	}
	switch sv.Type {
	case xdr.ScValTypeScvU128, xdr.ScValTypeScvI256, xdr.ScValTypeScvU256:
		// An amount-shaped value we cannot decode: a token's supply would be short, so it is not foreign traffic.
		return nil, SupplySkipAmountNotI128
	case xdr.ScValTypeScvMap:
	default:
		return nil, SupplySkipNotTokenShaped
	}
	entries, merr := scval.AsMap(sv)
	if merr != nil {
		return nil, SupplySkipNotTokenShaped
	}
	amtVal, ok := scval.MapField(entries, "amount")
	if !ok {
		return nil, SupplySkipNotTokenShaped
	}
	amt, aerr := scval.AsAmountFromI128(amtVal)
	if aerr != nil {
		return nil, SupplySkipAmountNotI128
	}
	return amt.BigInt(), ""
}

// DecodeSupplyAmountXDR is DecodeSupplyAmount for a base64 scval data body, the form the CH-reading supply tools use.
func DecodeSupplyAmountXDR(dataXDR string) (*big.Int, string, bool) {
	sv, err := scval.Parse(dataXDR)
	if err != nil {
		return nil, "parse-error", false
	}
	return DecodeSupplyAmount(sv)
}
