package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/holds"
)

// SetHolds swaps in the operator hold list; safe to call while serving.
func (s *Server) SetHolds(list []holds.Hold) {
	s.holds.Store(&list)
}

// underReview wraps a supply, balance or holder handler: when an operator
// hold covers the response it adds `flags.under_review` and a top-level
// `under_review_reason`. The numbers are served unchanged. With no holds
// loaded the handler runs unbuffered.
func (s *Server) underReview(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := s.holds.Load()
		if list == nil || len(*list) == 0 {
			h(w, r)
			return
		}
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		h(rec, r)
		body := rec.body.Bytes()
		if rec.status == http.StatusOK {
			if marked, ok := markUnderReview(*list, r.PathValue("asset_id"), body); ok {
				body = marked
			} else if strings.HasPrefix(rec.header.Get("Content-Type"), "text/csv") {
				// A CSV export has no envelope; its flags travel in a header.
				if _, held := holds.Match(*list, subjectOf(r.PathValue("asset_id"), nil)); held {
					rec.header.Add("X-StellarIndex-Flags", "under_review")
				}
			}
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	}
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

// markUnderReview returns the envelope with the hold marker added, or false
// when the body is not an envelope or no hold covers it.
func markUnderReview(list []holds.Hold, pathAsset string, body []byte) ([]byte, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil, false
	}
	hold, ok := holds.Match(list, subjectOf(pathAsset, env["data"]))
	if !ok {
		return nil, false
	}
	flags := map[string]json.RawMessage{}
	if raw, ok := env["flags"]; ok {
		if json.Unmarshal(raw, &flags) != nil {
			return nil, false
		}
	}
	flags["under_review"] = json.RawMessage("true")
	rawFlags, err := json.Marshal(flags)
	if err != nil {
		return nil, false
	}
	env["flags"] = rawFlags
	env["under_review_reason"] = json.RawMessage(strconv.Quote(hold.Reason))
	out, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}

// subjectOf collects every asset or contract id the response names: the
// path asset, data.asset_id/contract_id, and for an account the native
// balance and each trustline asset.
func subjectOf(pathAsset string, data json.RawMessage) holds.Subject {
	sub := holds.Subject{}
	if pathAsset != "" {
		sub.Assets = append(sub.Assets, pathAsset)
	}
	var d struct {
		AssetID    string `json:"asset_id"`
		ContractID string `json:"contract_id"`
		Balance    string `json:"balance"`
		AsOfLedger uint32 `json:"as_of_ledger"`
		// AssetDetail names its ledger supply_as_of_ledger.
		SupplyAsOfLedger *uint32 `json:"supply_as_of_ledger"`
		Trustlines       []struct {
			Asset string `json:"asset"`
		} `json:"trustlines"`
	}
	if json.Unmarshal(data, &d) != nil {
		return sub
	}
	sub.Ledger = d.AsOfLedger
	if sub.Ledger == 0 && d.SupplyAsOfLedger != nil {
		sub.Ledger = *d.SupplyAsOfLedger
	}
	for _, a := range []string{d.AssetID, d.ContractID} {
		if a != "" {
			sub.Assets = append(sub.Assets, a)
		}
	}
	if d.Balance != "" {
		sub.Assets = append(sub.Assets, "native")
	}
	for _, t := range d.Trustlines {
		sub.Assets = append(sub.Assets, t.Asset)
	}
	return sub
}
