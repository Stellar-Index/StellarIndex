package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AnomalyReader backs /v1/anomalies, the durable freeze-event mirror (ADR-0019).
type AnomalyReader interface {
	ListFreezeEvents(ctx context.Context, firingOnly bool, limit int) ([]timescale.FreezeEventRow, error)
	FreezeReasonCounts(ctx context.Context, sinceDays int) ([]timescale.FreezeReasonCount, error)
	// FreezeDailyReasonCounts is the day×reason tally behind the
	// optional `?include=daily` calendar-heatmap block.
	FreezeDailyReasonCounts(ctx context.Context, sinceDays int) ([]timescale.FreezeDailyReasonCount, error)
	// CountFiringFreezes is an exact count, NOT a page length —
	// firing_count must stay correct past any page cap.
	CountFiringFreezes(ctx context.Context) (int64, error)
}

// DivergenceReader backs /v1/divergence and its series, the per-reference divergence history.
type DivergenceReader interface {
	ListDivergenceLatest(ctx context.Context, sinceDays int, firingOnly bool, limit int) ([]timescale.DivergenceRow, error)
	ListDivergenceSeries(ctx context.Context, assetID, quoteID string, sinceDays int) ([]timescale.DivergenceSeriesPoint, error)
}

// ── /v1/anomalies ────────────────────────────────────────────────

// AnomaliesView is the GET /v1/anomalies response; FiringCount is an exact COUNT(*) of firing freezes, not a page length.
type AnomaliesView struct {
	FiringCount int64             `json:"firing_count"`
	ReasonTally []ReasonCountV    `json:"reason_tally"`
	Events      []FreezeEventView `json:"events"`
	// Daily is the day×reason tally over ReasonTally's window, populated only for `?include=daily`.
	// Deliberately NOT omitempty: null means "not requested/served", [] means "zero freezes", so a
	// client never renders a heatmap of zeros off the null case.
	Daily []DailyReasonCountV `json:"daily"`
}

// ReasonCountV is one (reason, count) cell of AnomaliesView.ReasonTally.
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

// FreezeEventView mirrors a freeze_events row: recovered_at is null while firing, frozen_value is a decimal string, Escalated/HoldUntil are ADR-0019 lifecycle fields.
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
	HoldUntil         *string         `json:"hold_until"`
	Escalated         *bool           `json:"escalated"`
}

// handleAnomalies serves GET /v1/anomalies — the freeze timeline
// (ADR-0019). `?firing=true` restricts to firing pairs; `?limit=` (default 100, max 500);
// `?window_days=` scopes the reason tally (default 30); `?include=daily` adds the day×reason
// tally. The live firing count and reason breakdown are always reported. Returns 200 with an
// empty payload when no reader is wired.
func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.Anomalies == nil {
		writeJSON(w, AnomaliesView{ReasonTally: []ReasonCountV{}, Events: []FreezeEventView{}}, Flags{})
		return
	}
	limit, ok := parseExplorerLimit(w, r, 100, 500)
	if !ok {
		return
	}
	firingOnly := r.URL.Query().Get("firing") == "true"
	windowDays, ok := parseWindowDays(w, r, 30)
	if !ok {
		return
	}

	events, err := s.Anomalies.ListFreezeEvents(r.Context(), firingOnly, limit)
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
	firingCount, err := s.Anomalies.CountFiringFreezes(r.Context())
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("CountFiringFreezes failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	tally, err := s.Anomalies.FreezeReasonCounts(r.Context(), windowDays)
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
		Events:      make([]FreezeEventView, 0, len(events)),
	}
	for i, t := range tally {
		out.ReasonTally[i] = ReasonCountV{Reason: t.Reason, Count: t.Count}
	}
	// frozen_value is the market's aggregated price: an event for a market
	// /v1/price withholds is omitted. The counts carry no price and keep it.
	withheld := s.storedMarketGate(anomaliesGateSurface)
	for _, e := range events {
		if w, _ := withheld(r.Context(), e.AssetID, e.QuoteID); w {
			continue
		}
		out.Events = append(out.Events, freezeEventView(e))
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
	daily, err := s.Anomalies.FreezeDailyReasonCounts(r.Context(), windowDays)
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
	if e.HoldUntil != nil {
		s := e.HoldUntil.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		v.HoldUntil = &s
	}
	v.Escalated = e.Escalated
	return v
}

// ── /v1/divergence ───────────────────────────────────────────────

// DivergenceView is the wire response for GET /v1/divergence.
type DivergenceView struct {
	Pairs []DivergencePairV `json:"pairs"`
}

// DivergencePairV is one market's latest comparison against every external reference, served only grouped beside our price; prices and deltas are decimal strings.
type DivergencePairV struct {
	AssetID string `json:"asset_id"`
	QuoteID string `json:"quote_id"`
	// OurPrice, ObservedAt and ObservedAtLedger are the pair's newest
	// comparison across its references.
	OurPrice         string           `json:"our_price"`
	ObservedAt       string           `json:"observed_at"`
	ObservedAtLedger int64            `json:"observed_at_ledger"`
	References       []DivergenceRefV `json:"references"`
}

// DivergenceRefV is one reference's latest comparison for a pair.
type DivergenceRefV struct {
	Reference string `json:"reference"`
	RefPrice  string `json:"ref_price"`
	DeltaPct  string `json:"delta_pct"`
	Status    string `json:"status"`
	// ObservedAt is when this reference was last compared: the pair's
	// observed_at unless the reference missed later ticks, in which case
	// delta_pct is against our price at this earlier time.
	ObservedAt string `json:"observed_at"`
	// RefObservedAt is when the reference observed RefPrice; null on rows
	// recorded before the reference time was stored.
	RefObservedAt *string `json:"ref_observed_at"`
}

// handleDivergence serves GET /v1/divergence — the current
// cross-reference divergence board: per (asset, quote) pair, our price beside the latest
// comparison against each reference within `?window_days=` (default 7), widest |delta_pct| first.
// `?firing=true` keeps pairs with a firing reference; `?limit=` counts pairs (default 100, max
// 500). Returns 200 with an empty payload when no reader is wired.
func (s *Server) handleDivergence(w http.ResponseWriter, r *http.Request) {
	if s.Divergences == nil {
		writeJSON(w, DivergenceView{Pairs: []DivergencePairV{}}, Flags{})
		return
	}
	limit, ok := parseExplorerLimit(w, r, 100, 500)
	if !ok {
		return
	}
	firingOnly := r.URL.Query().Get("firing") == "true"
	windowDays, ok := parseWindowDays(w, r, 7)
	if !ok {
		return
	}

	rows, err := s.Divergences.ListDivergenceLatest(r.Context(), windowDays, firingOnly, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListDivergenceLatest failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	writeJSON(w, s.groupDivergenceRows(r.Context(), rows), Flags{})
}

// groupDivergenceRows folds per-reference rows into one entry per pair,
// keeping the reader's pair order. A pair /v1/price withholds is omitted:
// our_price is that market's price, and each ref_price with its delta_pct
// restates it.
func (s *Server) groupDivergenceRows(ctx context.Context, rows []timescale.DivergenceRow) DivergenceView {
	withheld := s.storedMarketGate(divergenceGateSurface)
	out := DivergenceView{Pairs: []DivergencePairV{}}
	idx := make(map[[2]string]int)
	newest := make(map[[2]string]time.Time)
	for _, d := range rows {
		if w, _ := withheld(ctx, d.AssetID, d.QuoteID); w {
			continue
		}
		key := [2]string{d.AssetID, d.QuoteID}
		i, seen := idx[key]
		if !seen {
			i = len(out.Pairs)
			idx[key] = i
			out.Pairs = append(out.Pairs, DivergencePairV{AssetID: d.AssetID, QuoteID: d.QuoteID})
		}
		p := &out.Pairs[i]
		if !seen || d.ObservedAt.After(newest[key]) {
			newest[key] = d.ObservedAt
			p.OurPrice = d.OurPrice
			p.ObservedAt = formatDivergenceTime(d.ObservedAt)
			p.ObservedAtLedger = d.ObservedAtLedger
		}
		p.References = append(p.References, DivergenceRefV{
			Reference:     d.Reference,
			RefPrice:      d.RefPrice,
			DeltaPct:      d.DeltaPct,
			Status:        d.Status,
			ObservedAt:    formatDivergenceTime(d.ObservedAt),
			RefObservedAt: divergenceRefObservedAt(d.RefObservedAt),
		})
	}
	return out
}

func formatDivergenceTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}

// divergenceRefObservedAt renders a row's reference time in observed_at's
// format, nil when the row predates it being recorded.
func divergenceRefObservedAt(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	return &s
}

// Surfaces labelling the withholding metrics on the anomaly/divergence reads.
const (
	anomaliesGateSurface  = "anomalies"
	divergenceGateSurface = "divergence"
)

// storedMarketGate returns /v1/price's withholding verdict for stored rows
// keyed on (asset_id, quote_id), memoised per market for one response. It is decided at read time
// so a newly flagged issuer or thin market also applies to rows already stored. With a gate
// wired, an id that does not parse cannot be vetted and is withheld; with no gate nothing is.
func (s *Server) storedMarketGate(surface string) func(ctx context.Context, assetID, quoteID string) (bool, PriceWithheldReason) {
	type verdict struct {
		withheld bool
		reason   PriceWithheldReason
	}
	seen := make(map[[2]string]verdict)
	return func(ctx context.Context, assetID, quoteID string) (bool, PriceWithheldReason) {
		if s.Substance == nil && s.Scam == nil {
			return false, ""
		}
		key := [2]string{assetID, quoteID}
		if v, ok := seen[key]; ok {
			return v.withheld, v.reason
		}
		v := verdict{withheld: true, reason: PriceWithheldUnattributed}
		base, berr := canonical.ParseAsset(assetID)
		quote, qerr := canonical.ParseAsset(quoteID)
		if berr == nil && qerr == nil {
			w := withheldBy(ctx, s.Substance, s.Scam, base, quote, surface)
			v = verdict{withheld: w != pricingguard.NotWithheld, reason: withheldReasonFor(w)}
		}
		seen[key] = v
		return v.withheld, v.reason
	}
}

// ── /v1/divergence/series ────────────────────────────────────────

// DivergenceSeriesView is the GET /v1/divergence/series response: one pair's delta history against every reference.
type DivergenceSeriesView struct {
	AssetID string `json:"asset_id"`
	QuoteID string `json:"quote_id"`
	Days    int    `json:"days"`
	// BucketSeconds is the downsampling width: each reference's value is
	// its LAST observation inside the bucket (firing is bucket-wide
	// any-breach). Surfaced so clients render the series at its honest
	// resolution rather than assuming raw ticks.
	BucketSeconds int `json:"bucket_seconds"`
	// ThresholdPct is the operator's divergence alert threshold
	// (percent, config `divergence.threshold_pct`) — the band a
	// chart shades. Omitted when the API isn't configured with one;
	// clients must then render no band rather than invent one.
	ThresholdPct float64                  `json:"threshold_pct,omitempty"`
	Points       []DivergenceSeriesPointV `json:"points"`
}

// DivergenceSeriesPointV is one bucket of our price beside each reference's comparison; prices and deltas are decimal strings.
type DivergenceSeriesPointV struct {
	T string `json:"t"`
	// OurPrice is from the bucket's newest observation across references.
	OurPrice   string                 `json:"our_price"`
	References []DivergenceSeriesRefV `json:"references"`
}

// DivergenceSeriesRefV is one reference's last comparison in a bucket.
type DivergenceSeriesRefV struct {
	Reference string `json:"reference"`
	RefPrice  string `json:"ref_price"`
	DeltaPct  string `json:"delta_pct"`
	Firing    bool   `json:"firing,omitempty"`
}

// divergenceSeriesDays whitelists the `?days=` windows the series
// endpoint serves. Each maps to a fixed bucket width in the reader (timescale.DivergenceSeriesBucket),
// capping a response near 360 points; an open-ended window would allow an unbounded history scan.
var divergenceSeriesDays = map[int]bool{1: true, 7: true, 30: true}

// handleDivergenceSeries serves GET /v1/divergence/series — the Δ%
// time-series for ONE pair against every reference: `?pair=<asset_id>~<quote_id>` (the markets
// slug convention), `?days=` in {1, 7, 30} (default 7). There is no per-reference selector; a
// reference's prices are served only beside the others.
//
// Cache policy is private, no-store, via an explicit policyForPath arm shared with /v1/divergence
// and /v1/anomalies. Returns 200 with empty points when no reader is wired or the window is empty.
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
	if q.Has("reference") {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Unsupported reference parameter", http.StatusBadRequest,
			"the series carries every reference beside our price; one reference cannot be selected alone. Omit reference.")
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
		AssetID: base, QuoteID: quote, Days: days,
		BucketSeconds: int(timescale.DivergenceSeriesBucket(days).Seconds()),
		ThresholdPct:  s.DivergenceThresholdPct,
		Points:        []DivergenceSeriesPointV{},
	}
	if s.divergenceSeriesWithheld(w, r, base, quote) {
		return
	}
	if s.Divergences == nil {
		writeJSON(w, out, Flags{})
		return
	}
	points, err := s.Divergences.ListDivergenceSeries(r.Context(), base, quote, days)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListDivergenceSeries failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	out.Points = groupDivergenceSeries(points)
	writeJSON(w, out, Flags{})
}

// groupDivergenceSeries folds (bucket, reference) cells into one point
// per bucket in the reader's ascending order.
func groupDivergenceSeries(points []timescale.DivergenceSeriesPoint) []DivergenceSeriesPointV {
	out := []DivergenceSeriesPointV{}
	idx := make(map[time.Time]int)
	newest := make(map[time.Time]time.Time)
	for _, p := range points {
		b := p.Bucket.UTC()
		i, seen := idx[b]
		if !seen {
			i = len(out)
			idx[b] = i
			out = append(out, DivergenceSeriesPointV{T: b.Format("2006-01-02T15:04:05Z07:00")})
		}
		if !seen || p.LastAt.After(newest[b]) {
			newest[b] = p.LastAt
			out[i].OurPrice = p.OurPrice
		}
		out[i].References = append(out[i].References, DivergenceSeriesRefV{
			Reference: p.Reference,
			RefPrice:  p.RefPrice,
			DeltaPct:  p.DeltaPct,
			Firing:    p.Firing,
		})
	}
	return out
}

// divergenceSeriesWithheld writes the response and reports true when the
// requested legs do not parse (400) or /v1/price withholds the market
// (404, with the gate's reason): every point carries our_price.
func (s *Server) divergenceSeriesWithheld(w http.ResponseWriter, r *http.Request, base, quote string) bool {
	baseAsset, berr := canonical.ParseAsset(base)
	quoteAsset, qerr := canonical.ParseAsset(quote)
	if berr != nil || qerr != nil {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid pair", http.StatusBadRequest,
			"pair must be <asset_id>~<quote_id> with both legs valid asset ids, e.g. crypto:BTC~fiat:USD")
		return true
	}
	if withheld, reason := s.storedMarketGate(divergenceGateSurface)(r.Context(), base, quote); withheld {
		writePriceWithheldProblem(w, r, baseAsset, quoteAsset, reason)
		return true
	}
	return false
}

// parseWindowDays reads an optional ?window_days= positive int in
// [1, 365]; def when absent. A present but invalid value writes a 400, like the sibling ?days=
// and ?limit= params, so a caller is never told its window was honored when it was ignored.
func parseWindowDays(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	raw := r.URL.Query().Get("window_days")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 365 {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid window_days", http.StatusBadRequest,
			"window_days must be an integer in [1, 365]")
		return 0, false
	}
	return n, true
}
