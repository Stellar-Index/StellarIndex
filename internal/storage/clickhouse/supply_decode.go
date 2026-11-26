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

// Supply-flow skip reasons. Only the two not-token-shaped reasons are expected traffic: topic[0] alone does not identify a token, so any
// contract (a pool's one-topic `mint`, an NFT's `mint` with a u32 id) can emit one, and a contract with no supply loses nothing.
const (
	// SupplySkipTopicNotTokenShaped is topics without the address(es) a SEP-41/CAP-67 event carries; the body is never read.
	SupplySkipTopicNotTokenShaped = "topic_not_token_shaped"
	// SupplySkipNotTokenShaped is token-shaped topics with a body that is neither a bare i128 nor a map carrying `amount`.
	SupplySkipNotTokenShaped = "not_token_shaped"
	// SupplySkipTopicUnparseable is a topic that is not ScVal XDR, so the event's shape is unknown: a decoder gap, and it alerts.
	SupplySkipTopicUnparseable = "topic_unparseable"
	SupplySkipAmountNotI128    = "amount_not_i128"
	SupplySkipBodyUnparseable  = "body_unparseable"
	// SupplySkipJudgedNonToken is a skip from a contract judged not a token (judgedNonToken), for that judgement's reason only.
	SupplySkipJudgedNonToken = "judged_non_token"
)

// SupplySkipReasons lists every reason ClassifySupplyEventXDR returns, for metric zero-seeding.
var SupplySkipReasons = []string{
	SupplySkipTopicNotTokenShaped, SupplySkipNotTokenShaped, SupplySkipTopicUnparseable,
	SupplySkipAmountNotI128, SupplySkipBodyUnparseable, SupplySkipJudgedNonToken,
}

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

// supplyTopicsSkip applies the SEP-41/CAP-67 topic shape before any body is read: a transfer is ["transfer", from, to, (asset)]; a mint,
// burn or clawback has an Address at topic[1] (["mint", to, (asset)], legacy ["mint", admin, to, (asset)]). "" when token-shaped.
// Any ScAddress arm passes: the amount, not the holder, is what supply sums, and a skip here never alerts.
func supplyTopicsSkip(topicsXDR []string) string {
	if len(topicsXDR) == 0 {
		return SupplySkipTopicNotTokenShaped
	}
	t0, err := scval.Parse(topicsXDR[0])
	if err != nil {
		return SupplySkipTopicUnparseable
	}
	addrTopics := 1
	if topicName(t0) == "transfer" {
		addrTopics = 2
	}
	if len(topicsXDR) < 1+addrTopics {
		return SupplySkipTopicNotTokenShaped
	}
	for _, t := range topicsXDR[1 : 1+addrTopics] {
		sv, err := scval.Parse(t)
		if err != nil {
			return SupplySkipTopicUnparseable
		}
		if sv.Type != xdr.ScValTypeScvAddress {
			return SupplySkipTopicNotTokenShaped
		}
	}
	return ""
}

func topicName(sv xdr.ScVal) string {
	if s, err := scval.AsSymbol(sv); err == nil {
		return s
	}
	s, _ := scval.AsString(sv)
	return s
}
