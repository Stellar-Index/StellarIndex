package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// validMEVKinds is the openapi ?kind= enum: the detectors' own
// exported Kind* constants (so live kinds can't drift from what the
// workers write to mev_events.kind) plus oracle_deviation, reserved
// in the spec for a detector that doesn't exist yet — a filter on it
// is valid and just yields no rows, not a 400.
var validMEVKinds = map[string]bool{
	mev.KindArbitrage:          true,
	mev.KindSandwich:           true,
	mev.KindOracleSandwich:     true,
	mev.KindLiquidationCascade: true,
	mev.KindWashTrade:          true,
	"oracle_deviation":         true,
}

// MEVReader is the storage seam for /v1/mev. timescale.Store
// implements via ListMEVEvents.
type MEVReader interface {
	ListMEVEvents(ctx context.Context, kind string, limit int) ([]timescale.MEVEventRow, error)
}

// MEVEventView is the wire shape for /v1/mev entries. detail is the
// pattern's per-kind evidence object (legs / roles / oracle refs /
// fills, plus a note stating what is and is not claimed). profit_usd
// is null when not meaningful for the kind — no current detector
// estimates profit (trade direction is ambiguous in the served rows).
type MEVEventView struct {
	EventID          string          `json:"event_id"`
	DetectedAt       string          `json:"detected_at"`
	DetectedAtLedger int64           `json:"detected_at_ledger"`
	Kind             string          `json:"kind"`
	AssetID          string          `json:"asset_id,omitempty"`
	QuoteID          string          `json:"quote_id,omitempty"`
	TxHashes         []string        `json:"tx_hashes"`
	Accounts         []string        `json:"accounts"`
	Detail           json.RawMessage `json:"detail"`
	ProfitUSD        *string         `json:"profit_usd"`
}

// handleMEVEvents serves GET /v1/mev — the auto-flagged MEV-event
// feed, newest first. ?kind= filters to one pattern (arbitrage /
// sandwich / oracle_sandwich / liquidation_cascade / wash_trade);
// ?limit= (default 50, max 500).
//
// 200 + empty array when no MEVReader is wired or nothing's been
// detected — the same feature-gated-reader degradation as /v1/markets
// and /v1/lending/pools.
func (s *Server) handleMEVEvents(w http.ResponseWriter, r *http.Request) {
	if s.mev == nil {
		writeJSON(w, []MEVEventView{}, Flags{})
		return
	}
	limit, ok := parseExplorerLimit(w, r, 50, 500)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && !validMEVKinds[kind] {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-kind",
			"Invalid kind", http.StatusBadRequest,
			"kind must be one of arbitrage, sandwich, oracle_sandwich, oracle_deviation, liquidation_cascade, wash_trade; got "+kind)
		return
	}

	// Hard 8s ceiling — companion to the same fix on /v1/pools and
	// /v1/markets: without it a cold-cache scan hangs until
	// the ingress times out instead of returning a fast, retryable 503.
	mCtx, mCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer mCancel()
	rows, err := s.mev.ListMEVEvents(mCtx, kind, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(mCtx, err) {
			s.logger.Warn("ListMEVEvents deadline exceeded", "limit", limit, "kind", kind)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/mev-timeout",
				"MEV query timed out", http.StatusServiceUnavailable,
				"the underlying mev_events scan didn't return in 8s. Retry in a few seconds.")
			return
		}
		s.logger.Error("ListMEVEvents failed", "err", err, "kind", kind)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := make([]MEVEventView, len(rows))
	for i, e := range rows {
		v := MEVEventView{
			EventID:          e.EventID,
			DetectedAt:       e.DetectedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			DetectedAtLedger: e.DetectedAtLedger,
			Kind:             e.Kind,
			AssetID:          e.AssetID,
			QuoteID:          e.QuoteID,
			TxHashes:         e.TxHashes,
			Accounts:         e.Accounts,
			Detail:           rawOrNull(e.Detail),
		}
		if e.ProfitUSD != "" {
			p := e.ProfitUSD
			v.ProfitUSD = &p
		}
		if v.TxHashes == nil {
			v.TxHashes = []string{}
		}
		if v.Accounts == nil {
			v.Accounts = []string{}
		}
		out[i] = v
	}
	writeJSON(w, out, Flags{})
}

// rawOrNull passes a stored jsonb string through as-is, falling back
// to a JSON null when it's empty or not valid JSON (defensive — the
// column is NOT NULL so this should never fire).
func rawOrNull(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}
