package explorer

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// AccountsStatsView is the wire response for GET /v1/accounts/stats —
// the /accounts hub's analytics strip (operator request 2026-08-08).
// Stroops-denominated values are STRINGS (ADR-0003); counts are JSON
// numbers (all far below 2^53).
type AccountsStatsView struct {
	Totals struct {
		Accounts                 int64  `json:"accounts"`
		Trustlines               int64  `json:"trustlines"`
		TrustlineHoldingAccounts int64  `json:"trustline_holding_accounts"`
		XLMHeldStroops           string `json:"xlm_held_stroops"`
	} `json:"totals"`
	Balances struct {
		AvgStroops    string `json:"avg_stroops"`
		MedianStroops string `json:"median_stroops"`
		P90Stroops    string `json:"p90_stroops"`
		P99Stroops    string `json:"p99_stroops"`
	} `json:"balances"`
	Concentration struct {
		Top100XLMStroops string `json:"top100_xlm_stroops"`
		// Top100SharePct is a display convenience (top100 / total * 100),
		// rounded to 0.01 — derived from the two exact stroops figures
		// above, which remain the auditable values.
		Top100SharePct float64 `json:"top100_share_pct"`
	} `json:"concentration"`
	// WealthHistogram buckets accounts by floor(log10(balance in XLM)):
	// bucket -1 is "< 1 XLM", 0 is 1–10, … 10 is ">= 10B".
	WealthHistogram []WealthBucketV `json:"wealth_histogram"`
	// TrustlineHistogram bands accounts by trustline count; the "0" band
	// is derived (total accounts − trustline-holding accounts).
	TrustlineHistogram []TrustlineBucketV `json:"trustline_histogram"`
	TopHeldAssets      []HeldAssetV       `json:"top_held_assets"`
	ComputedAt         string             `json:"computed_at"`
}

type WealthBucketV struct {
	Bucket     int8   `json:"bucket"`
	Accounts   uint64 `json:"accounts"`
	XLMStroops string `json:"xlm_stroops"`
}

type TrustlineBucketV struct {
	Bucket   string `json:"bucket"`
	Accounts uint64 `json:"accounts"`
}

type HeldAssetV struct {
	Asset   string `json:"asset"`
	Holders int64  `json:"holders"`
}

// trustlineBucketOrder fixes the band display order (map iteration and
// ClickHouse's ORDER BY on the band STRING are both wrong orders).
var trustlineBucketOrder = []string{"0", "1", "2-5", "6-10", "11-50", "50+"}

// AccountsStats serves GET /v1/accounts/stats from the ch-holders-rollup
// cycle's precomputed tables — keyed reads, sub-second by construction.
func (h *Handler) AccountsStats(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	s, ok, err := h.Reader.AccountsStats(ctx)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/accounts-stats-timeout",
				"Accounts stats timed out")
			return
		}
		h.Logger.Error("explorer AccountsStats failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/accounts-stats-warming",
			"Accounts stats warming", http.StatusServiceUnavailable,
			"the accounts analytics rollup hasn't completed its first cycle on this deployment yet; retry shortly")
		return
	}

	out := AccountsStatsView{ComputedAt: s.ComputedAt.UTC().Format(time.RFC3339)}
	out.Totals.Accounts = s.TotalAccounts
	out.Totals.Trustlines = s.TotalTrustlines
	out.Totals.TrustlineHoldingAccounts = s.TrustlineHoldingAccounts
	out.Totals.XLMHeldStroops = strconv.FormatInt(s.XLMTotalStroops, 10)
	out.Balances.AvgStroops = strconv.FormatInt(s.AvgStroops, 10)
	out.Balances.MedianStroops = strconv.FormatInt(s.MedianStroops, 10)
	out.Balances.P90Stroops = strconv.FormatInt(s.P90Stroops, 10)
	out.Balances.P99Stroops = strconv.FormatInt(s.P99Stroops, 10)
	out.Concentration.Top100XLMStroops = strconv.FormatInt(s.Top100XLMStroops, 10)
	if s.XLMTotalStroops > 0 {
		out.Concentration.Top100SharePct = float64(int64(float64(s.Top100XLMStroops)/float64(s.XLMTotalStroops)*10000)) / 100
	}
	for _, b := range s.WealthHistogram {
		out.WealthHistogram = append(out.WealthHistogram, WealthBucketV{
			Bucket: b.Bucket, Accounts: b.Accounts,
			XLMStroops: strconv.FormatInt(b.XLMStroops, 10),
		})
	}
	byBand := map[string]uint64{}
	for _, b := range s.TrustlineHistogram {
		byBand[b.Bucket] = b.Accounts
	}
	if zero := s.TotalAccounts - s.TrustlineHoldingAccounts; zero > 0 {
		byBand["0"] = uint64(zero) //nolint:gosec // non-negative by the guard
	}
	for _, band := range trustlineBucketOrder {
		if n, present := byBand[band]; present {
			out.TrustlineHistogram = append(out.TrustlineHistogram, TrustlineBucketV{Bucket: band, Accounts: n})
		}
	}
	for _, a := range s.TopHeldAssets {
		out.TopHeldAssets = append(out.TopHeldAssets, HeldAssetV(a))
	}
	h.WriteJSON(w, out, false)
}
