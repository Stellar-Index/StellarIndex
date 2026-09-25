package explorer

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SearchResultView classifies a free-text explorer query and points at the
// canonical detail endpoint for it. The UI's single search box (ADR-0038)
// uses Kind + Href to route. Supported=false marks kinds with no mounted
// detail endpoint at all (currently only "unknown").
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
// the most specific shapes first (tx hash, ledger seq, account/contract/muxed
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
		// Full contract detail (AccountState's contract counterpart) has
		// been mounted at /v1/contracts/{contract_id} since server.go's
		// explorer wiring; route there instead of the transfer-trail-only
		// sub-resource.
		res.Kind, res.Canonical, res.Href, res.Supported = "contract", q, "/v1/contracts/"+q, true

	case canonical.IsAccountID(q):
		// AccountState (explorer/account_state.go) is mounted at
		// /v1/accounts/{g_strkey} and covers ordinary accounts as well as
		// issuers, so route there directly instead of the issuer-only view.
		res.Kind, res.Canonical, res.Href, res.Supported = "account", q, "/v1/accounts/"+q, true

	case canonical.IsMuxedAccount(q):
		// A muxed M-address is a G-account plus an off-chain routing id (the
		// shape exchanges hand out as deposit addresses); the ledger state
		// belongs to the G, so resolve to it and keep the M in Query.
		g, _ := canonical.MuxedAccountID(q)
		res.Kind, res.Canonical, res.Href, res.Supported = "account", g, "/v1/accounts/"+g, true
		res.Note = "muxed address resolved to its underlying account " + g

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
