// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/holds"
)

// underReviewRows wraps a listing handler: each row an operator hold covers gains `under_review` and `under_review_reason`, and
// the envelope `flags.under_review`; numbers are unchanged. It marks after the handler, so a cached body never outlives a hold change.
func (s *Server) underReviewRows(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := s.holds.Load()
		if list == nil || len(*list) == 0 {
			h(w, r)
			return
		}
		rec := &bufferedResponse{outer: w, header: w.Header(), status: http.StatusOK}
		h(rec, r)
		body := rec.body.Bytes()
		if rec.status == http.StatusOK {
			if marked, ok := markRowsUnderReview(*list, s.listingLedger(r, *list), body); ok {
				body = marked
			}
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	}
}

// listingLedger is "now" for listing rows, which carry no ledger: the cached lake watermark, read only when a hold is ledger-ranged;
// 0 (no watermark) leaves only holds with no ledger_to covering the rows.
func (s *Server) listingLedger(r *http.Request, list []holds.Hold) uint32 {
	for _, h := range list {
		if h.LedgerFrom != 0 || h.LedgerTo != 0 {
			ledger, _, _ := s.lakeWatermark(r.Context())
			return ledger
		}
	}
	return 0
}

// markRowsUnderReview returns the envelope with every held row of its data array marked, or false when nothing is held.
func markRowsUnderReview(list []holds.Hold, ledger uint32, body []byte) ([]byte, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil, false
	}
	var rows []json.RawMessage
	if json.Unmarshal(env["data"], &rows) != nil {
		return nil, false
	}
	hit := false
	for i, raw := range rows {
		sub := subjectOf("", raw)
		sub.Ledger, sub.Current = ledger, true
		hold, held := holds.Match(list, sub)
		if !held {
			continue
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil {
			return nil, false
		}
		reason, err := json.Marshal(hold.Reason)
		if err != nil {
			return nil, false
		}
		row["under_review"], row["under_review_reason"] = json.RawMessage("true"), reason
		if rows[i], err = json.Marshal(row); err != nil {
			return nil, false
		}
		hit = true
	}
	if !hit || !setUnderReviewFlag(env) {
		return nil, false
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, false
	}
	env["data"] = data
	out, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}
