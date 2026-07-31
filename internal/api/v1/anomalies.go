package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AnomalyReader backs /v1/anomalies — the durable freeze-event mirror
// (ADR-0019). timescale.Store implements it.
type AnomalyReader interface {
	ListFreezeEvents(ctx context.Context, firingOnly bool, limit int) ([]timescale.FreezeEventRow, error)
	FreezeReasonCounts(ctx context.Context, sinceDays int) ([]timescale.FreezeReasonCount, error)
	// FreezeDailyReasonCounts is the day×reason tally behind the
	// optional `?include=daily` calendar-heatmap block.
	FreezeDailyReasonCounts(ctx context.Context, sinceDays int) ([]timescale.FreezeDailyReasonCount, error)
	// CountFiringFreezes is an exact count, NOT a page length —
	// firing_count must stay correct past any page cap (C1-051).
	CountFiringFreezes(ctx context.Context) (int64, error)
}

// DivergenceReader backs /v1/divergence + /v1/divergence/series —
// the per-reference divergence-observation history. timescale.Store
// implements it.
type DivergenceReader interface {
	ListDivergenceLatest(ctx context.Context, sinceDays int, firingOnly bool, limit int) ([]timescale.DivergenceRow, error)
	ListDivergenceSeries(ctx context.Context, assetID, quoteID, reference string, sinceDays int) ([]timescale.DivergenceSeriesPoint, error)
}

// ── /v1/anomalies ────────────────────────────────────────────────

// AnomaliesView is the wire response for GET /v1/anomalies.
//
// FiringCount is an exact COUNT(*) over currently-firing freezes, not the
// length of a page: it used to be `len(ListFreezeEvents(…, 500))`, so a
// storm of more than 500 assets reported exactly 500 and the number
// silently saturated at the moment it mattered most (C1-051,
// audit-2026-07-23).
type AnomaliesView struct {
	FiringCount int64             `json:"firing_count"`
	ReasonTally []ReasonCountV    `json:"reason_tally"`
	Events      []FreezeEventView `json:"events"`
	// Daily is the day×reason tally over the same window as
	// ReasonTally — populated only when `?include=daily` was
	// requested. Deliberately NOT omitempty: `null` means "not
	// requested / not served" while `[]` means "requested, zero
	// freezes in the window" — a client must never render a
	// heatmap of zeros off the null case (degraded ≠ zero).
	Daily []DailyReasonCountV `json:"daily"`
}

type ReasonCountV struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// DailyReasonCountV is one (UTC day, reason) heatmap cell.
type DailyReasonCountV struct {
	Day    string `json:"day"` // YYYY-MM-DD (UTC)
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// FreezeEventView mirrors a freeze_events row. recovered_at is null
// while the freeze is currently firing. frozen_value is a decimal
// string (ADR-0003).
type FreezeEventView struct {
	AssetID           string          `json:"asset_id"`
	QuoteID           string          `json:"quote_id"`
	FrozenAt          string          `json:"frozen_at"`
	FrozenAtLedger    int64           `json:"frozen_at_ledger"`
	Reason            string          `json:"reason"`
	FrozenValue       string          `json:"frozen_value"`
	RecoveredAt       *string         `json:"recovered_at"`
	RecoveredAtLedger *int64          `json:"recovered_at_ledger"`
	Firing            bool            `json:"firing"`
	Detail            json.RawMessage `json:"detail,omitempty"`
}

// handleAnomalies serves GET /v1/anomalies — the freeze timeline
// (ADR-0019). `?firing=true` restricts to currently-firing pairs;
// `?limit=` (default 100, max 500); `?window_days=` scopes the reason
// tally (default 30); `?include=daily` adds the day×reason tally over
// the same window (the /anomalies calendar heatmap). Always reports
// the live firing count + per-reason breakdown alongside the event
// list.
//
// 200 + empty payload when no reader is wired — feature-gated like
// /v1/lending/pools.
func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.anomalies == nil {
		writeJSON(w, AnomaliesView{ReasonTally: []ReasonCountV{}, Events: []FreezeEventView{}}, Flags{})
		return
	}
	limit, ok := parseExplorerLimit(w, r, 100, 500)
	if !ok {
		return
	}
	firingOnly := r.URL.Query().Get("firing") == "true"
	windowDays := parseWindowDays(r, 30)

	events, err := s.anomalies.ListFreezeEvents(r.Context(), firingOnly, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListFreezeEvents failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	// Always compute the live firing count (independent of the firing
	// filter) so the UI can show "N firing now" on the full timeline.
	// An exact count, not a page length — see AnomaliesView.
	firingCount, err := s.anomalies.CountFiringFreezes(r.Context())
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("CountFiringFreezes failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	tally, err := s.anomalies.FreezeReasonCounts(r.Context(), windowDays)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("FreezeReasonCounts failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}

	out := AnomaliesView{
		FiringCount: firingCount,
		ReasonTally: make([]ReasonCountV, len(tally)),
		Events:      make([]FreezeEventView, len(events)),
	}
	for i, t := range tally {
		out.ReasonTally[i] = ReasonCountV{Reason: t.Reason, Count: t.Count}
	}
	for i, e := range events {
		out.Events[i] = freezeEventView(e)
	}
	// Opt-in day×reason tally (`?include=daily`) — same window as the
	// reason tally. Kept opt-in so the default response stays one
	// indexed scan lighter for consumers that only want the timeline.
	if r.URL.Query().Get("include") == "daily" {
		daily, ok := s.anomaliesDaily(w, r, windowDays)
		if !ok {
			return
		}
		out.Daily = daily
	}
	writeJSON(w, out, Flags{})
}

// anomaliesDaily fetches + shapes the `?include=daily` day×reason
// block. ok=false means the response has already been written (error
// or aborted client) and the caller must return.
func (s *Server) anomaliesDaily(w http.ResponseWriter, r *http.Request, windowDays int) ([]DailyReasonCountV, bool) {
	daily, err := s.anomalies.FreezeDailyReasonCounts(r.Context(), windowDays)
	if err != nil {
		if clientAborted(r, err) {
			return nil, false
		}
		s.logger.Error("FreezeDailyReasonCounts failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return nil, false
	}
	out := make([]DailyReasonCountV, len(daily))
	for i, d := range daily {
		out[i] = DailyReasonCountV{
			Day:    d.Day.UTC().Format("2006-01-02"),
			Reason: d.Reason,
			Count:  d.Count,
		}
	}
	return out, true
}

func freezeEventView(e timescale.FreezeEventRow) FreezeEventView {
	v := FreezeEventView{
		AssetID:           e.AssetID,
		QuoteID:           e.QuoteID,
		FrozenAt:          e.FrozenAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		FrozenAtLedger:    e.FrozenAtLedger,
		Reason:            e.Reason,
		FrozenValue:       e.FrozenValue,
		RecoveredAtLedger: e.RecoveredAtLedger,
		Firing:            e.RecoveredAt == nil,
		Detail:            rawOrNull(e.Detail),
	}
	if e.RecoveredAt != nil {
		s := e.RecoveredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		v.RecoveredAt = &s
	}
	if e.Detail == "" {
		v.Detail = nil // omit rather than emit "null"
	}
	return v
}

// ── /v1/divergence ───────────────────────────────────────────────

// DivergenceView is the wire response for GET /v1/divergence.
type DivergenceView struct {
	Observations []DivergenceObsV `json:"observations"`
}

// DivergenceObsV mirrors the latest divergence_observations row per
// (asset, quote, reference). Prices + delta are decimal strings.
type DivergenceObsV struct {
	AssetID          string `json:"asset_id"`
	QuoteID          string `json:"quote_id"`
	Reference        string `json:"reference"`
	ObservedAt       string `json:"observed_at"`
	ObservedAtLedger int64  `json:"observed_at_ledger"`
	OurPrice         string `json:"our_price"`
	RefPrice         string `json:"ref_price"`
	DeltaPct         string `json:"delta_pct"`
	Status           string `json:"status"`
}

// handleDivergence serves GET /v1/divergence — the current
// cross-reference divergence board: the latest observation per
// (asset, quote, reference) within `?window_days=` (default 7),
// widest |delta_pct| first. `?firing=true` restricts to references
// whose latest comparison breached threshold; `?limit=` (default 100,
// max 500).
//
// 200 + empty payload when no reader is wired.
func (s *Server) handleDivergence(w http.ResponseWriter, r *http.Request) {
	if s.divergences == nil {
		writeJSON(w, DivergenceView{Observations: []DivergenceObsV{}}, Flags{})
		return
	}
	limit, ok := parseExplorerLimit(w, r, 100, 500)
	if !ok {
		return
	}
	firingOnly := r.URL.Query().Get("firing") == "true"
	windowDays := parseWindowDays(r, 7)

	rows, err := s.divergences.ListDivergenceLatest(r.Context(), windowDays, firingOnly, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListDivergenceLatest failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	out := DivergenceView{Observations: make([]DivergenceObsV, len(rows))}
	for i, d := range rows {
		out.Observations[i] = DivergenceObsV{
			AssetID:          d.AssetID,
			QuoteID:          d.QuoteID,
			Reference:        d.Reference,
			ObservedAt:       d.ObservedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			ObservedAtLedger: d.ObservedAtLedger,
			OurPrice:         d.OurPrice,
			RefPrice:         d.RefPrice,
			DeltaPct:         d.DeltaPct,
			Status:           d.Status,
		}
	}
	writeJSON(w, out, Flags{})
}

// ── /v1/divergence/series ────────────────────────────────────────

// DivergenceSeriesView is the wire response for GET
// /v1/divergence/series — one (pair, reference) Δ% history.
type DivergenceSeriesView struct {
	AssetID   string `json:"asset_id"`
	QuoteID   string `json:"quote_id"`
	Reference string `json:"reference"`
	Days      int    `json:"days"`
	// BucketSeconds is the downsampling width: each point is the
	// LAST observation inside its bucket (firing is bucket-wide
	// any-breach). Surfaced so clients render the series at its
	// honest resolution rather than assuming raw ticks.
	BucketSeconds int `json:"bucket_seconds"`
	// ThresholdPct is the operator's divergence alert threshold
	// (percent, config `divergence.threshold_pct`) — the band a
	// chart shades. Omitted when the API isn't configured with one;
	// clients must then render no band rather than invent one.
	ThresholdPct float64                  `json:"threshold_pct,omitempty"`
	Points       []DivergenceSeriesPointV `json:"points"`
}

// DivergenceSeriesPointV is one bucket. Prices + delta are decimal
// strings (ADR-0003).
type DivergenceSeriesPointV struct {
	T        string `json:"t"`
	DeltaPct string `json:"delta_pct"`
	OurPrice string `json:"our_price"`
	RefPrice string `json:"ref_price"`
	Firing   bool   `json:"firing,omitempty"`
}

// divergenceSeriesDays whitelists the `?days=` windows the series
// endpoint serves — each maps to a fixed bucket width in the reader
// (timescale.DivergenceSeriesBucket), keeping every response ≤ ~360
// points. An open-ended window param would let one request scan an
// unbounded history slice.
var divergenceSeriesDays = map[int]bool{1: true, 7: true, 30: true}

// divergenceReferences mirrors the divergence_observations.reference
// CHECK constraint (migration 0019) — the only values that can exist.
// Rejecting everything else up front keeps garbage params from ever
// reaching the index scan.
var divergenceReferences = map[string]bool{
	"chainlink": true, "coingecko": true,
	"reflector-cex": true, "reflector-fx": true, "reflector-dex": true,
	"redstone": true, "band": true,
}

// handleDivergenceSeries serves GET /v1/divergence/series — the Δ%
// time-series for ONE (pair, reference): `?pair=<asset_id>~<quote_id>`
// (the markets slug convention), `?reference=` (one of the board's
// reference names), `?days=` ∈ {1, 7, 30} (default 7). The history
// companion to the /v1/divergence board.
//
// Cache policy: falls through to policyForPath's conservative default
// (private, no-store) — deliberately matching its sibling
// /v1/divergence rather than introducing a divergent policy here.
//
// 200 + empty points when no reader is wired or the triple has no
// observations in the window.
func (s *Server) handleDivergenceSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pair := q.Get("pair")
	base, quote, ok := strings.Cut(pair, "~")
	if !ok || base == "" || quote == "" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid pair", http.StatusBadRequest,
			"pair must be <asset_id>~<quote_id>, e.g. crypto:BTC~fiat:USD")
		return
	}
	reference := q.Get("reference")
	if !divergenceReferences[reference] {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid reference", http.StatusBadRequest,
			"reference must be one of: chainlink, coingecko, reflector-cex, reflector-fx, reflector-dex, redstone, band")
		return
	}
	days := 7
	if raw := q.Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !divergenceSeriesDays[n] {
			writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
				"Invalid days", http.StatusBadRequest, "days must be one of: 1, 7, 30")
			return
		}
		days = n
	}

	out := DivergenceSeriesView{
		AssetID: base, QuoteID: quote, Reference: reference, Days: days,
		BucketSeconds: int(timescale.DivergenceSeriesBucket(days).Seconds()),
		ThresholdPct:  s.divergenceThresholdPct,
		Points:        []DivergenceSeriesPointV{},
	}
	if s.divergences == nil {
		writeJSON(w, out, Flags{})
		return
	}
	points, err := s.divergences.ListDivergenceSeries(r.Context(), base, quote, reference, days)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListDivergenceSeries failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	out.Points = make([]DivergenceSeriesPointV, len(points))
	for i, p := range points {
		out.Points[i] = DivergenceSeriesPointV{
			T:        p.Bucket.UTC().Format("2006-01-02T15:04:05Z07:00"),
			DeltaPct: p.DeltaPct,
			OurPrice: p.OurPrice,
			RefPrice: p.RefPrice,
			Firing:   p.Firing,
		}
	}
	writeJSON(w, out, Flags{})
}

// parseWindowDays reads an optional ?window_days= positive int,
// clamped to [1, 365]; def when absent or malformed.
func parseWindowDays(r *http.Request, def int) int {
	raw := r.URL.Query().Get("window_days")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 365 {
		return def
	}
	return n
}
