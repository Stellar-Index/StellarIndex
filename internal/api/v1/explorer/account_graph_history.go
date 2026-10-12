// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package explorer

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AccountGraphHistoryView is GET /v1/accounts/{g_strkey}/graph/history; the creation and sponsorship series are kept apart, not summed, as they cover different spans.
type AccountGraphHistoryView struct {
	Account string `json:"account"`
	// Granularity is the bucket width, always "1M". The served tier keeps only two timestamps per
	// edge, so a finer bucket would place no additional event.
	Granularity string `json:"granularity"`
	Series      struct {
		Created   AccountGraphHistorySeriesV `json:"created"`
		Sponsored AccountGraphHistorySeriesV `json:"sponsored"`
	} `json:"series"`
	Note string `json:"note"`
}

// AccountGraphHistorySeriesV is one relation's series and the totals that qualify it.
type AccountGraphHistorySeriesV struct {
	// Points holds only the months that carry something; a quiet month emits no point and
	// nothing is carried forward. Read an absent month against `coverage`: inside the covered span
	// nothing happened, outside it nothing was observed.
	Points []AccountGraphHistoryPointV `json:"points"`
	Totals AccountGraphHistoryTotalsV  `json:"totals"`
	// LowerBound is true when `totals.events_unplaced` is non-zero: each point's `events` is then
	// a lower bound on that month, while `new_accounts` is exact either way.
	LowerBound bool `json:"lower_bound"`
	// Unplaced names what the series could not place, tallied by reason (like `excluded` on
	// /v1/rwa/assets), so a figure that dropped rows is not passed off as the full one. Absent
	// when nothing was refused.
	Unplaced []AccountGraphHistoryUnplacedV `json:"unplaced,omitempty"`
	// Coverage is the span the rollup cycle actually aggregated (ADR-0031): data-derived, never a
	// constant or the tip by assumption. It is what makes an absent month readable.
	Coverage AccountGraphCoverageV `json:"coverage"`
}

// AccountGraphHistoryTotalsV is the whole-history denominator; every figure spans the account's entire history, not the returned points.
type AccountGraphHistoryTotalsV struct {
	// Accounts is distinct counterparties ever created or sponsored, and
	// equals the sum of the points' new_accounts exactly.
	Accounts uint64 `json:"accounts"`
	// Events is the operations behind them — creations, or sponsorship
	// arrangements started.
	Events uint64 `json:"events"`
	// EventsPlaced is how many of Events the points account for;
	// EventsUnplaced the rest. The two always sum to Events.
	EventsPlaced   uint64 `json:"events_placed"`
	EventsUnplaced uint64 `json:"events_unplaced"`
}

// AccountGraphHistoryPointV is one calendar month.
type AccountGraphHistoryPointV struct {
	// Period is the month as YYYY-MM; PeriodStart is its first instant
	// in UTC, for a consumer that would otherwise parse the label.
	Period      string `json:"period"`
	PeriodStart string `json:"period_start"`
	// NewAccounts is counterparties FIRST created or sponsored in this
	// month. Exact and complete.
	NewAccounts uint64 `json:"new_accounts"`
	// Events is the individual operations known to fall in this month. A
	// lower bound when the series sets `lower_bound`.
	Events uint64 `json:"events"`
}

// AccountGraphHistoryUnplacedV is one reason events could not be given a month, with the exact number it accounts for.
type AccountGraphHistoryUnplacedV struct {
	Reason string `json:"reason"`
	Events uint64 `json:"events"`
	Detail string `json:"detail"`
}

// unplacedRepeatEvents is the only reason this surface refuses a month, and it comes from the
// stored shape: the graph holds one row per distinct pair with first_at and last_at, so an edge
// with N events places two and the middle N-2 have no recorded time. Per-event timestamps live
// only in the rollups' working tables, which are truncated and refilled every cycle.
const unplacedRepeatEvents = "repeat-events-not-timestamped"

const unplacedRepeatEventsDetail = "an edge stores first_at and last_at only, so events between " +
	"the first and the last of a repeated relationship have no recorded month; they are counted " +
	"here rather than spread across the interval or dropped"

// accountGraphHistoryNote is served on every response. Beyond what the figures mean, it states
// how to read an absent month, which a series forces and a snapshot does not.
const accountGraphHistoryNote = "History, not live state: creations are immutable and sponsorship " +
	"figures count arrangements STARTED, never arrangements in force. Buckets are calendar months " +
	"in UTC. A month with no activity emits NO POINT — never a zero and never a carried-forward " +
	"value; read an absent month against the series' coverage span, inside which it means nothing " +
	"happened and outside which it means nothing was observed. new_accounts is exact and sums to " +
	"totals.accounts; events is a lower bound whenever lower_bound is true, with the shortfall " +
	"counted exactly in totals.events_unplaced and named in unplaced[]."

// AccountGraphHistory serves GET /v1/accounts/{g_strkey}/graph/history; bounded by construction (primary-key range reads, one point per active month) so it takes no granularity or window.
func (h *Handler) AccountGraphHistory(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	hist, ok, err := h.Reader.AccountGraphHistory(ctx, g)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-graph-history-timeout",
				"Account graph history timed out")
			return
		}
		h.Logger.Error("explorer AccountGraphHistory failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/account-graph-history-warming",
			"Account graph history warming", http.StatusServiceUnavailable,
			"the sponsorship/creation graph hasn't completed its first cycle on this "+
				"deployment yet; retry shortly")
		return
	}

	_, stale, _ := h.lakeTip(ctx)
	h.WriteJSON(w, accountGraphHistoryView(g, hist), stale)
}

// accountGraphHistoryView renders the reader snapshot onto the wire contract. It is split out so
// the honesty rules in the shaping can be tested without an HTTP round-trip.
func accountGraphHistoryView(account string, h clickhouse.AccountGraphHistory) AccountGraphHistoryView {
	out := AccountGraphHistoryView{
		Account:     account,
		Granularity: "1M",
		Note:        accountGraphHistoryNote,
	}
	out.Series.Created = historySeriesView(h.Created)
	out.Series.Sponsored = historySeriesView(h.Sponsored)
	return out
}

func historySeriesView(s clickhouse.AccountGraphHistorySide) AccountGraphHistorySeriesV {
	v := AccountGraphHistorySeriesV{
		// Never `null` on the wire: no relationship in this direction is an empty series.
		Points:   make([]AccountGraphHistoryPointV, 0, len(s.Points)),
		Coverage: coverageView(s.Coverage),
	}
	v.Totals = AccountGraphHistoryTotalsV{
		Accounts:       s.Accounts,
		Events:         s.Events,
		EventsPlaced:   s.EventsPlaced,
		EventsUnplaced: s.EventsUnplaced,
	}
	for _, p := range s.Points {
		// An empty bucket would be a zero posing as a measurement; drop it, since the coverage
		// span makes its absence readable.
		if p.NewAccounts == 0 && p.Events == 0 {
			continue
		}
		start := p.Start.UTC()
		v.Points = append(v.Points, AccountGraphHistoryPointV{
			Period:      start.Format("2006-01"),
			PeriodStart: start.Format(time.RFC3339),
			NewAccounts: p.NewAccounts,
			Events:      p.Events,
		})
	}
	if s.EventsUnplaced > 0 {
		v.LowerBound = true
		v.Unplaced = []AccountGraphHistoryUnplacedV{{
			Reason: unplacedRepeatEvents,
			Events: s.EventsUnplaced,
			Detail: unplacedRepeatEventsDetail,
		}}
	}
	return v
}
