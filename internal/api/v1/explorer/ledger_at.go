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

// errLedgerAtNotFound means no captured ledger provably closed at or before the instant: before the lake's first ledger, 1s+ after its
// tip's close, or with the answer inside or just before a lake gap.
var errLedgerAtNotFound = errors.New("no ledger resolvable at ts")

// LedgerAt serves GET /v1/ledgers/at?ts=, the highest-sequence ledger whose close_time is at or before ts, in the /v1/ledgers/{seq} shape.
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

// ledgerAtOrBefore finds the first captured ledger a that closed after ts and returns a-1, served only when proven (a-1 captured and
// closed at or before ts, a after it). Only a lake gap adjacent to the answer yields errLedgerAtNotFound, never a wrong ledger.
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

// FirstCapturedClosedAfter searches for the lowest captured ledger closed after ts (tipSeq must qualify); each probe reads the
// newest captured ledger at or below mid, so lake gaps cannot mislead it. Probes are aimed by close-time interpolation (a few reads
// instead of ~26 bisections); two that fail to halve the interval make the next a bisection, bounding the worst case at ~2x.
func FirstCapturedClosedAfter(ctx context.Context, r LedgerPager, tipSeq uint32, ts time.Time) (uint32, error) {
	lo, hi := uint32(0), tipSeq
	var s ledgerSearch
	aimed, prevWidth := true, tipSeq
	for lo < hi {
		width := hi - lo
		mid := lo + width/2
		if est, ok := s.aim(lo, hi, ts); aimed && ok {
			mid = est
		}
		below, err := r.RecentLedgers(ctx, 1, mid+1)
		if err != nil {
			return 0, err
		}
		if len(below) == 0 || !below[0].CloseTime.After(ts) {
			lo = mid + 1
			if len(below) > 0 {
				s.below, s.haveBelow = below[0], true
			}
			s.step(-1)
		} else {
			if below[0].Seq > mid {
				return 0, fmt.Errorf("ledger read below %d returned ledger %d", mid+1, below[0].Seq)
			}
			hi = below[0].Seq
			s.above, s.haveAbove = below[0], true
			s.step(+1)
		}
		// Judge over two probes: an aimed probe landing a ledger short of the answer leaves the far bound untouched.
		aimed = !aimed || hi-lo <= prevWidth/2 || prevWidth == tipSeq
		prevWidth = width
	}
	return lo, nil
}

// ledgerSearch holds the probed ledgers nearest ts on each side and a margin that doubles while probes keep landing on one side.
type ledgerSearch struct {
	below, above         clickhouse.LedgerHeader
	haveBelow, haveAbove bool
	side                 int
	margin               uint32
}

func (s *ledgerSearch) step(side int) {
	if side == s.side {
		s.margin = min(2*s.margin, 1<<24)
	} else {
		s.side, s.margin = side, 4
	}
}

// aim returns a probe in [lo, hi-1]: just under the tip until a close time is known, then the interpolated answer pushed back
// across the bound the last probe moved, by the margin. ok=false when the estimate falls outside the interval (e.g. below a
// lake that starts above genesis): the model is wrong there, so bisect.
func (s *ledgerSearch) aim(lo, hi uint32, ts time.Time) (uint32, bool) {
	if !s.haveAbove {
		return hi - 1, true
	}
	perLedger := 5 * time.Second
	if s.haveBelow && s.above.Seq > s.below.Seq {
		perLedger = s.above.CloseTime.Sub(s.below.CloseTime) / time.Duration(s.above.Seq-s.below.Seq)
	}
	est := float64(s.above.Seq)
	if perLedger > 0 {
		est -= float64(s.above.CloseTime.Sub(ts)) / float64(perLedger)
	}
	if est < float64(lo)-1 || est > float64(hi)+1 {
		return 0, false
	}
	est -= float64(s.side) * float64(min(s.margin, (hi-lo)/4))
	return uint32(max(float64(lo), min(est, float64(hi-1)))), true
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
