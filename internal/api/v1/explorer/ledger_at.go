package explorer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// errLedgerAtNotFound means no captured ledger provably closed at or before
// the requested instant: before the lake's first ledger, 1s or more after its
// tip's close, or with the answer inside or just before a lake gap.
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
				" with its successor also captured; ts is before the lake's first ledger, 1s or more after its tip's close, or inside or just before a gap")
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

// ledgerAtOrBefore finds the first captured ledger a that closed after ts and
// returns a-1. The answer is served only when it is proven: a-1 is captured
// and closed at or before ts, and its successor a closed after it. Only a lake
// gap adjacent to the answer yields errLedgerAtNotFound, never a wrong ledger.
func (h *Handler) ledgerAtOrBefore(ctx context.Context, ts time.Time) (clickhouse.LedgerHeader, error) {
	tip, err := h.Reader.RecentLedgers(ctx, 1, 0)
	if err != nil {
		return clickhouse.LedgerHeader{}, err
	}
	if len(tip) == 0 {
		return clickhouse.LedgerHeader{}, errLedgerAtNotFound
	}
	if !ts.Before(tip[0].CloseTime) {
		// Close times are whole seconds and strictly increase, so the next
		// ledger closes at least 1s after the tip. Past that, a ledger not
		// yet captured may still have closed at or before ts.
		if ts.Before(tip[0].CloseTime.Add(time.Second)) {
			return tip[0], nil
		}
		return clickhouse.LedgerHeader{}, errLedgerAtNotFound
	}
	after, err := FirstCapturedClosedAfter(ctx, h.Reader, tip[0].Seq, ts)
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

// LedgerPager is the read FirstCapturedClosedAfter probes with.
type LedgerPager interface {
	RecentLedgers(ctx context.Context, limit int, beforeSeq uint32) ([]clickhouse.LedgerHeader, error)
}

// FirstCapturedClosedAfter binary-searches for the lowest captured ledger
// whose close_time is after ts; tipSeq must be captured and close after ts.
// Each probe reads the newest captured ledger at or below mid (a sort-key
// range read), so a lake gap far from the answer cannot mislead the search.
func FirstCapturedClosedAfter(ctx context.Context, r LedgerPager, tipSeq uint32, ts time.Time) (uint32, error) {
	lo, hi := uint32(0), tipSeq
	for lo < hi {
		mid := lo + (hi-lo)/2
		below, err := r.RecentLedgers(ctx, 1, mid+1)
		if err != nil {
			return 0, err
		}
		if len(below) == 0 || !below[0].CloseTime.After(ts) {
			lo = mid + 1
			continue
		}
		if below[0].Seq > mid {
			return 0, fmt.Errorf("ledger read below %d returned ledger %d", mid+1, below[0].Seq)
		}
		hi = below[0].Seq
	}
	return lo, nil
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
