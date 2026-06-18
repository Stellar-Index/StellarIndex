package v1

import (
	"net/http"
	"strconv"
)

// AccountStateView is the wire response for GET /v1/accounts/{g_strkey}.
// Balances are strings (ADR-0003 — stroop amounts past 2^53 lose precision as
// JSON numbers).
type AccountStateView struct {
	AccountID     string             `json:"account_id"`
	Exists        bool               `json:"exists"`
	Balance       string             `json:"balance,omitempty"`
	SeqNum        string             `json:"seq_num,omitempty"`
	NumSubentries uint32             `json:"num_subentries,omitempty"`
	Flags         uint32             `json:"flags,omitempty"`
	HomeDomain    string             `json:"home_domain,omitempty"`
	Thresholds    *AccountThresholds `json:"thresholds,omitempty"`
	Signers       []AccountSignerV   `json:"signers,omitempty"`
	Trustlines    []TrustlineV       `json:"trustlines,omitempty"`
	Offers        []OfferV           `json:"offers,omitempty"`
	LastLedger    uint32             `json:"last_modified_ledger,omitempty"`
}

type AccountThresholds struct {
	Master byte `json:"master"`
	Low    byte `json:"low"`
	Med    byte `json:"med"`
	High   byte `json:"high"`
}

type AccountSignerV struct {
	Key    string `json:"key"`
	Weight uint32 `json:"weight"`
}

type TrustlineV struct {
	Asset   string `json:"asset"`
	Balance string `json:"balance"`
	Limit   string `json:"limit"`
	Flags   uint32 `json:"flags"`
}

type OfferV struct {
	OfferID int64  `json:"offer_id"`
	Selling string `json:"selling"`
	Buying  string `json:"buying"`
	Amount  string `json:"amount"`
	PriceN  int32  `json:"price_n"`
	PriceD  int32  `json:"price_d"`
}

// handleAccountState serves GET /v1/accounts/{g_strkey} — the account's current
// on-chain state reconstructed from the lake: native balance, sequence,
// thresholds, flags, signers, home domain, plus its live trustlines and offers.
// `exists:false` (200, not 404) for an account with no live AccountEntry in the
// captured window — clients distinguish "no such account / not yet captured"
// from a malformed request.
func (s *Server) handleAccountState(w http.ResponseWriter, r *http.Request) {
	if s.explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	g := r.PathValue("g_strkey")
	if !looksLikeStellarAccount(g) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-account",
			"Invalid account", http.StatusBadRequest, "g_strkey must be a 56-character G-strkey")
		return
	}

	st, err := s.explorer.AccountState(r.Context(), g)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("explorer AccountState failed", "err", err, "account", g)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := AccountStateView{AccountID: g, Exists: st.Exists}
	if st.Exists {
		out.Balance = strconv.FormatInt(st.Balance, 10)
		out.SeqNum = strconv.FormatInt(st.SeqNum, 10)
		out.NumSubentries = st.NumSubEntries
		out.Flags = st.Flags
		out.HomeDomain = st.HomeDomain
		out.Thresholds = &AccountThresholds{Master: st.MasterWeight, Low: st.ThreshLow, Med: st.ThreshMed, High: st.ThreshHigh}
		out.LastLedger = st.LastModifiedLedger
		for _, sg := range st.Signers {
			out.Signers = append(out.Signers, AccountSignerV{Key: sg.Key, Weight: sg.Weight})
		}
		for _, t := range st.Trustlines {
			out.Trustlines = append(out.Trustlines, TrustlineV{
				Asset: t.Asset, Balance: strconv.FormatInt(t.Balance, 10),
				Limit: strconv.FormatInt(t.Limit, 10), Flags: t.Flags,
			})
		}
		for _, o := range st.Offers {
			out.Offers = append(out.Offers, OfferV{
				OfferID: o.OfferID, Selling: o.Selling, Buying: o.Buying,
				Amount: strconv.FormatInt(o.Amount, 10), PriceN: o.PriceN, PriceD: o.PriceD,
			})
		}
	}
	writeJSON(w, out, Flags{})
}

// AssetHoldersView is the wire response for GET /v1/assets/{asset_id}/holders.
type AssetHoldersView struct {
	Asset       string         `json:"asset"`
	HolderCount int64          `json:"holder_count"`
	Holders     []AssetHolderV `json:"holders"`
}

type AssetHolderV struct {
	AccountID string `json:"account_id"`
	Balance   string `json:"balance"`
}

// handleAssetHolders serves GET /v1/assets/{asset_id}/holders — the top holders
// of an asset by current trustline balance, plus the total holder count.
// asset_id is the canonical form ("CODE-ISSUER" / "native"). Lake-backed
// (ledger_entry_changes trustlines).
func (s *Server) handleAssetHolders(w http.ResponseWriter, r *http.Request) {
	if s.explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	asset := r.PathValue("asset_id")
	if asset == "" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest, "asset_id path segment is required")
		return
	}
	limit, ok := parseExplorerLimit(w, r, 100, 500)
	if !ok {
		return
	}

	holders, total, err := s.explorer.AssetHolders(r.Context(), asset, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("explorer AssetHolders failed", "err", err, "asset", asset)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := AssetHoldersView{Asset: asset, HolderCount: total, Holders: make([]AssetHolderV, len(holders))}
	for i, h := range holders {
		out.Holders[i] = AssetHolderV{AccountID: h.AccountID, Balance: strconv.FormatInt(h.Balance, 10)}
	}
	writeJSON(w, out, Flags{})
}

// looksLikeStellarAccount is a cheap shape check for a G-strkey (the real
// validation is the lake lookup). 56 chars, leading 'G'.
func looksLikeStellarAccount(s string) bool {
	if len(s) != 56 || s[0] != 'G' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		base32Digit := c >= '2' && c <= '7'
		if !upper && !base32Digit {
			return false
		}
	}
	return true
}
