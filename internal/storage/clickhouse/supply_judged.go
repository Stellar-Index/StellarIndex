package clickhouse

import "math/big"

// Judgement records why one contract's supply-topic skips are not a token losing supply.
type Judgement struct {
	Reason   string // the only skip reason the judgement covers
	Verdict  string
	Evidence string // ledger, tx and body that decided it
	Judged   string // YYYY-MM-DD
}

// judgedNonToken is added to by PR, one entry per contract, with evidence a reviewer can re-check in the lake. A judgement covers a
// token-shaped event's body only: topics no token carries are already topic_not_token_shaped and never reach it.
var judgedNonToken = map[string]Judgement{}

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
