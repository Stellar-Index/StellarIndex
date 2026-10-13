package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// validMEVKinds is the ?kind= enum: detector Kind* constants plus oracle_deviation (no detector yet; filtering yields no rows, not a
// 400).
var validMEVKinds = map[string]bool{
	mev.KindArbitrage:          true,
	mev.KindSandwich:           true,
	mev.KindOracleSandwich:     true,
	mev.KindLiquidationCascade: true,
	mev.KindWashTrade:          true,
	"oracle_deviation":         true,
}

// MEVReader is the storage seam for /v1/mev; timescale.Store implements it via ListMEVEvents.
type MEVReader interface {
	ListMEVEvents(ctx context.Context, kind string, limit int) ([]timescale.MEVEventRow, error)
}

// MEVPulseLooker reads the MEV worker's run heartbeat; mev.RedisHeartbeat implements it.
type MEVPulseLooker interface {
	Pulse(ctx context.Context) (full, present bool, err error)
}

// mevDetectorsPartial is the flags.detectors_partial value: false only on a fresh full beat. An
// unwired looker or a failed read counts as partial, since "no detector ran" and "cannot tell"
// must not read as a quiet network.
func (s *Server) mevDetectorsPartial(r *http.Request) bool {
	if s.MEV == nil || s.MEVPulse == nil {
		return true
	}
	full, present, err := s.MEVPulse.Pulse(r.Context())
	if err != nil {
		if !clientAborted(r, err) {
			s.logger.Warn("mev: heartbeat read failed", "err", err)
		}
		return true
	}
	return !(full && present)
}

// MEVEventView is a /v1/mev entry; detail holds per-kind evidence plus a note on what is claimed, and profit_usd is null because no detector estimates profit.
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

// handleMEVEvents serves GET /v1/mev, newest first; ?kind= filters, ?limit= defaults to 50 (max 500).
// Returns 200 with an empty array when no MEVReader is wired or nothing is detected.
func (s *Server) handleMEVEvents(w http.ResponseWriter, r *http.Request) {
	flags := Flags{DetectorsPartial: s.mevDetectorsPartial(r)}
	if s.MEV == nil {
		writeJSON(w, []MEVEventView{}, flags)
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
	rows, err := s.MEV.ListMEVEvents(mCtx, kind, limit)
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
	writeJSON(w, out, flags)
}

// rawOrNull passes a stored jsonb string through, or JSON null when empty or invalid (defensive; the column is NOT NULL).
func rawOrNull(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}
