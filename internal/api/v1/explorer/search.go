package explorer

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// SearchResultView classifies a free-text explorer query and points at the
// canonical detail endpoint for it. The UI's single search box (ADR-0038)
// uses Kind + Href to route. Supported=false marks kinds whose detail
// endpoint isn't fully built yet (e.g. account state, Phase C).
type SearchResultView struct {
	Query     string `json:"query"`
	Kind      string `json:"kind"` // transaction|ledger|account|contract|asset|unknown
	Canonical string `json:"canonical,omitempty"`
	Href      string `json:"href,omitempty"`
	Supported bool   `json:"supported"`
	Note      string `json:"note,omitempty"`
}

// Search serves GET /v1/search?q= — classify a query by strkey/hash/seq
// shape and return the canonical detail endpoint. Pure classification (no lake
// read), so it works regardless of the explorer reader's availability.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-query",
			"Missing query", http.StatusBadRequest, "the q parameter is required")
		return
	}
	h.WriteJSON(w, classifySearch(q), false)
}

// classifySearch maps a query to its entity kind + detail href. Order matters:
// the most specific shapes first (tx hash, ledger seq, account/contract
// strkeys), then the generic asset-id parse, then unknown.
func classifySearch(q string) SearchResultView {
	res := SearchResultView{Query: q}

	switch {
	case txHashRe.MatchString(normalizeHexHash(q)):
		h := normalizeHexHash(q)
		res.Kind, res.Canonical, res.Href, res.Supported = "transaction", h, "/v1/tx/"+h, true

	case isLedgerSeq(q):
		res.Kind, res.Canonical, res.Href, res.Supported = "ledger", q, "/v1/ledgers/"+q, true

	case canonical.IsContractID(q):
		// Contract: SEP-41 transfer trail is available today; full contract
		// detail (events/invocations/state) lands with ADR-0038 unit 3 / Phase C.
		res.Kind, res.Canonical, res.Href, res.Supported = "contract", q, "/v1/contracts/"+q+"/transfers", true

	case canonical.IsAccountID(q):
		// Issuer view is the available account surface today; full account
		// state (balances/history) is ADR-0038 Phase B/C.
		res.Kind, res.Canonical, res.Href, res.Supported = "account", q, "/v1/issuers/"+q, true
		res.Note = "full account view (balances, history) is coming; showing issuer view"

	default:
		if a, err := canonical.ParseAsset(q); err == nil {
			id := a.String()
			res.Kind, res.Canonical, res.Href, res.Supported = "asset", id, "/v1/assets/"+id, true
		} else {
			res.Kind, res.Supported = "unknown", false
			res.Note = "not a recognised tx hash, ledger sequence, account, contract, or asset id"
		}
	}
	return res
}

// isLedgerSeq reports whether q is a plain decimal that fits a uint32 ledger
// sequence.
func isLedgerSeq(q string) bool {
	if q == "" || len(q) > 10 {
		return false
	}
	n, err := strconv.ParseUint(q, 10, 32)
	return err == nil && n > 0
}
