package clickhouse

import "math/big"

// Judgement records why one contract's supply-topic skips are not a token losing supply.
type Judgement struct {
	Reason   string // the only skip reason the judgement covers
	Verdict  string
	Evidence string // ledger, tx and body that decided it
	Judged   string // YYYY-MM-DD
}

// judgedNonToken is added to by PR, one entry per contract, with evidence a reviewer can re-check in the lake.
var judgedNonToken = map[string]Judgement{
	"CAOAVZOUDGYDI3QJTCDEQ3GJPWFN7QHM632EQZLWAIUWKILEFTKZUPE6": {
		Reason:  SupplySkipAmountNotI128,
		Verdict: "concentrated-liquidity pool, not a SEP-41 token: its `amount` is a u128 liquidity figure",
		Evidence: "mainnet ledger 65648178, tx f9b5c31a749a33c304010489dfbec5336f6b43809d1ef70fc0564d8dcb693b80: one-topic `burn` " +
			"whose body is a map {amount u128, amount0 u128, amount1 u128, owner, tick_lower i32, tick_upper i32}; its `mint` adds sender",
		Judged: "2026-11-24",
	},
}

// ClassifySupplyEventXDR is [ClassifySupplyBodyXDR] for one contract's event. A skip equal to the contract's judged Reason becomes
// judged_non_token; any other skip, and any body that decodes, is returned unchanged.
func ClassifySupplyEventXDR(contractID, dataXDR string) (*big.Int, string) {
	amt, skip := ClassifySupplyBodyXDR(dataXDR)
	if j, ok := judgedNonToken[contractID]; ok && skip != "" && skip == j.Reason {
		return nil, SupplySkipJudgedNonToken
	}
	return amt, skip
}

// SupplySkipAlerts reports whether a skip may short a token's supply or movements, so it alerts and names its contract.
func SupplySkipAlerts(reason string) bool {
	return reason != SupplySkipNotTokenShaped && reason != SupplySkipJudgedNonToken
}
