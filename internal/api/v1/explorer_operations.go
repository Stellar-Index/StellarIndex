package v1

import (
	"net/http"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/xdrjson"
)

// OpView is the wire shape for a decoded operation. Type is the snake_case op
// type; Fields holds the decoded, human-readable body (empty for not-yet-decoded
// types, in which case RawXDR carries the original base64 so nothing is lost).
type OpView struct {
	Ledger        uint32         `json:"ledger"`
	CloseTime     string         `json:"close_time"`
	TxHash        string         `json:"tx_hash"`
	TxIndex       uint32         `json:"tx_index"`
	OpIndex       uint32         `json:"op_index"`
	Type          string         `json:"type"`
	SourceAccount string         `json:"source_account,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
	RawXDR        string         `json:"raw_xdr,omitempty"`
}

// opView decodes an operation row's XDR body into the wire shape. On decode
// failure it degrades to the lake's (normalised) op type + the raw body, so a
// single malformed/unknown op never fails the response.
func opView(o clickhouse.OpRow) OpView {
	v := OpView{
		Ledger:        o.Seq,
		CloseTime:     o.CloseTime.UTC().Format(time.RFC3339),
		TxHash:        o.TxHash,
		TxIndex:       o.TxIndex,
		OpIndex:       o.OpIndex,
		SourceAccount: o.SourceAccount,
	}
	d, err := xdrjson.DecodeOperationBody(o.BodyXDR)
	if err != nil {
		v.Type = normalizeLakeOpType(o.OpType)
		v.RawXDR = o.BodyXDR
		return v
	}
	v.Type = d.Type
	if len(d.Fields) > 0 {
		v.Fields = d.Fields
	}
	if d.RawXDR != "" {
		v.RawXDR = d.RawXDR
	}
	return v
}

// normalizeLakeOpType turns the lake's "OperationTypeManageSellOffer" into a
// best-effort lowercase fallback ("managesselloffer") for the decode-error path
// only — the happy path uses xdrjson's controlled snake_case vocabulary.
func normalizeLakeOpType(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "OperationType"))
}

// OperationsView is the wire response for GET /v1/operations.
type OperationsView struct {
	Ledger     uint32   `json:"ledger"`
	Operations []OpView `json:"operations"`
}

// handleOperations serves GET /v1/operations?ledger=<seq> — operations in a
// ledger, decoded. When ?ledger is omitted it defaults to the latest ledger
// (recent activity out of the box). Ledger-scoped so the query is
// partition-pruned (no tx_hash index needed). Global cross-ledger browse is a
// follow-up (needs the participant index, ADR-0038 Phase B).
func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	if s.explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	seq, ok := parseUint32Query(w, r, "ledger")
	if !ok {
		return
	}
	limit, ok := parseExplorerLimit(w, r, 500, 2000)
	if !ok {
		return
	}
	if seq == 0 {
		// Default to the latest ledger.
		tip, err := s.explorer.RecentLedgers(r.Context(), 1, 0)
		if err != nil {
			if clientAborted(r, err) {
				return
			}
			s.logger.Error("explorer tip lookup failed", "err", err)
			writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
			return
		}
		if len(tip) == 0 {
			writeJSON(w, OperationsView{Operations: []OpView{}}, Flags{})
			return
		}
		seq = tip[0].Seq
	}
	rows, err := s.explorer.OperationsByLedger(r.Context(), seq, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("explorer OperationsByLedger failed", "err", err, "seq", seq)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := OperationsView{Ledger: seq, Operations: make([]OpView, len(rows))}
	for i, o := range rows {
		out.Operations[i] = opView(o)
	}
	writeJSON(w, out, Flags{})
}
