package explorer

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// errLedgerAtNotFound means no captured ledger provably closed at or before
// the requested instant: before the lake's first ledger, after its tip, or
// in a gap the lake cannot answer across.
var errLedgerAtNotFound = errors.New("no ledger resolvable at ts")

// LedgerAt serves GET /v1/ledgers/at?ts= — the highest-sequence ledger whose
// close_time is at or before ts, in the /v1/ledgers/{seq} shape.
func (h *Handler) LedgerAt(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	ts, ok := h.parseLedgerAtTS(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	l, err := h.ledgerAtOrBefore(ctx, ts)
	switch {
	case errors.Is(err, errLedgerAtNotFound):
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/ledger-not-found",
			"Ledger not found", http.StatusNotFound,
			"no captured ledger closed at or before "+ts.UTC().Format(time.RFC3339Nano)+
				" with its successor also captured; ts is before the lake's first ledger, after its tip, or inside a gap")
		return
	case err != nil:
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer ledger-at deadline exceeded", "ts", ts)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/ledger-detail-timeout",
				"Ledger lookup timed out")
			return
		}
		h.Logger.Error("explorer ledger-at failed", "err", err, "ts", ts)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	_, stale, _ := h.LakeWatermark(ctx)
	h.WriteJSON(w, ledgerView(l), stale)
}

// ledgerAtOrBefore resolves ts with ledgerSeqAtCloseTime's binary search over
// LedgerBySeq point reads (at most ~32 sort-key lookups, no range scan). The
// answer is returned only when it is proven: ledger c closed at or before ts
// AND ledger c+1 closed after it (or c is the tip), so a lake hole that
// misled the search yields errLedgerAtNotFound rather than a wrong ledger.
func (h *Handler) ledgerAtOrBefore(ctx context.Context, ts time.Time) (clickhouse.LedgerHeader, error) {
	tip, err := h.Reader.RecentLedgers(ctx, 1, 0)
	if err != nil {
		return clickhouse.LedgerHeader{}, err
	}
	if len(tip) == 0 || ts.After(tip[0].CloseTime) {
		// A ledger not yet captured may still close at or before ts.
		return clickhouse.LedgerHeader{}, errLedgerAtNotFound
	}
	if ts.Equal(tip[0].CloseTime) {
		return tip[0], nil
	}
	// close_time is whole seconds, so "closed after ts" is "closed at or
	// after the next whole second".
	after, err := h.ledgerSeqAtCloseTime(ctx, tip[0].Seq, ts.Truncate(time.Second).Add(time.Second))
	if err != nil {
		return clickhouse.LedgerHeader{}, err
	}
	if after == 0 {
		return clickhouse.LedgerHeader{}, errLedgerAtNotFound
	}
	l, found, err := h.Reader.LedgerBySeq(ctx, after-1)
	if err != nil {
		return clickhouse.LedgerHeader{}, err
	}
	if !found || l.CloseTime.After(ts) {
		return clickhouse.LedgerHeader{}, errLedgerAtNotFound
	}
	return l, nil
}

// parseLedgerAtTS reads the required `ts`: RFC 3339 or non-negative unix
// seconds. ok=false means a 400 problem+json was written.
func (h *Handler) parseLedgerAtTS(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := r.URL.Query().Get("ts")
	if raw == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/missing-ts",
			"Missing ts parameter", http.StatusBadRequest,
			"ts is required: RFC 3339 (e.g. 2024-06-01T12:00:00Z) or unix seconds")
		return time.Time{}, false
	}
	if strings.Trim(raw, "0123456789") == "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return time.Unix(n, 0).UTC(), true
		}
	} else if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, true
	}
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-ts",
		"Invalid ts parameter", http.StatusBadRequest,
		"ts must be RFC 3339 (e.g. 2024-06-01T12:00:00Z; encode a '+' offset as %2B) or non-negative unix seconds")
	return time.Time{}, false
}
