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
	"CBFY4Q26RW5TYMWFFIVBP3CRMWHWNUD2GUFRQWNKRG4U53VELSSZ2OFQ": {
		Reason:  SupplySkipAmountNotI128,
		Verdict: "concentrated-liquidity pool, not a SEP-41 token: its `amount` is a u128 liquidity figure",
		Evidence: "mainnet ledger 65660039, tx ef2478a489ebee5eea183e884465f2c874c0ab12987098801b75e093339e9a87: one-topic `burn` " +
			"whose body is a map {amount u128, amount0 u128, amount1 u128, owner, tick_lower i32, tick_upper i32}; the contract " +
			"emits only init, swap, mint, burn and collect, never a transfer",
		Judged: "2026-11-25",
	},
}

// ClassifySupplyEventXDR classifies one contract's mint/burn/clawback/transfer event (base64 topics incl. topic[0], and body): topics
// that are no token's shape are topic_not_token_shaped whatever the body, else [ClassifySupplyBodyXDR] decides. A skip equal to the
// contract's judged Reason becomes judged_non_token; any other skip, and any amount that decodes, is returned unchanged.
func ClassifySupplyEventXDR(contractID string, topicsXDR []string, dataXDR string) (*big.Int, string) {
	var amt *big.Int
	skip := supplyTopicsSkip(topicsXDR)
	if skip == "" {
		amt, skip = ClassifySupplyBodyXDR(dataXDR)
	}
	if j, ok := judgedNonToken[contractID]; ok && skip != "" && skip == j.Reason {
		return nil, SupplySkipJudgedNonToken
	}
	return amt, skip
}

// SupplySkipAlerts reports whether a skip may short a token's supply or movements, so it alerts and names its contract.
func SupplySkipAlerts(reason string) bool {
	switch reason {
	case SupplySkipTopicNotTokenShaped, SupplySkipNotTokenShaped, SupplySkipJudgedNonToken:
		return false
	default:
		return true
	}
}
