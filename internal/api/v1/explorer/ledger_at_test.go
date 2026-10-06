package explorer

import (
	"context"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

var ledgerAtGenesis = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// ledgerAtClose is a strictly increasing, irregular cadence (5-7 s), so a
// cadence-arithmetic shortcut would land on the wrong ledger.
func ledgerAtClose(seq uint32) time.Time {
	return ledgerAtGenesis.Add(time.Duration(5*seq+seq%3) * time.Second)
}

// ledgerAtReader holds ledgers [first, tip] minus holes and counts reads.
type ledgerAtReader struct {
	*capReader
	first, tip  uint32
	holes       map[uint32]bool
	pointReads  int
	recentCalls int
}

func (r *ledgerAtReader) has(seq uint32) bool {
	return seq >= r.first && seq <= r.tip && !r.holes[seq]
}

func (r *ledgerAtReader) RecentLedgers(_ context.Context, limit int, before uint32) ([]clickhouse.LedgerHeader, error) {
	r.recentCalls++
	if limit != 1 || before != 0 {
		return nil, nil // only the tip read is part of this endpoint's contract
	}
	return []clickhouse.LedgerHeader{{Seq: r.tip, CloseTime: ledgerAtClose(r.tip)}}, nil
}

func (r *ledgerAtReader) LedgerBySeq(_ context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error) {
	r.pointReads++
	if !r.has(seq) {
		return clickhouse.LedgerHeader{}, false, nil
	}
	return clickhouse.LedgerHeader{Seq: seq, CloseTime: ledgerAtClose(seq), TxCount: seq % 7}, true, nil
}

// want is the brute-force answer: the newest held ledger closed at or before
// ts whose successor is held too (or which is the tip); 0 means 404.
func (r *ledgerAtReader) want(ts time.Time) uint32 {
	if ts.After(ledgerAtClose(r.tip)) {
		return 0
	}
	for s := r.tip; s >= r.first && s > 0; s-- {
		if !ledgerAtClose(s).After(ts) {
			if r.has(s) && (s == r.tip || r.has(s+1)) {
				return s
			}
			return 0
		}
	}
	return 0
}

func ledgerAtServe(t *testing.T, reader *ledgerAtReader, rawTS string) (int, LedgerView) {
	t.Helper()
	h := newProbeHandler(reader, nil)
	var got LedgerView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		got, _ = data.(LedgerView)
		w.WriteHeader(http.StatusOK)
	}
	rec := httptest.NewRecorder()
	h.LedgerAt(rec, httptest.NewRequest(http.MethodGet, "/v1/ledgers/at?ts="+rawTS, nil))
	return rec.Code, got
}

func ledgerAtCheck(t *testing.T, reader *ledgerAtReader, ts time.Time) {
	t.Helper()
	reader.pointReads, reader.recentCalls = 0, 0
	code, got := ledgerAtServe(t, reader, ts.Format(time.RFC3339Nano))
	want := reader.want(ts)
	switch {
	case want == 0 && code != http.StatusNotFound:
		t.Fatalf("ts %s: status %d (ledger %d), want 404", ts.Format(time.RFC3339Nano), code, got.Sequence)
	case want != 0 && (code != http.StatusOK || got.Sequence != want):
		t.Fatalf("ts %s: status %d ledger %d, want 200 ledger %d", ts.Format(time.RFC3339Nano), code, got.Sequence, want)
	}
	// Bounded: one tip read plus a binary search of point reads, never a scan.
	if limit := bits.Len32(reader.tip) + 1; reader.pointReads > limit || reader.recentCalls != 1 {
		t.Fatalf("ts %s: %d point reads / %d tip reads, want <= %d / 1", ts, reader.pointReads, reader.recentCalls, limit)
	}
}

func TestLedgerAt_MatchesBruteForceOnEveryLedger(t *testing.T) {
	for _, reader := range []*ledgerAtReader{
		{capReader: &capReader{probe: &deadlineProbe{}}, first: 1, tip: 2_000},
		// A lake whose history starts above genesis (a fresh testnet).
		{capReader: &capReader{probe: &deadlineProbe{}}, first: 700, tip: 2_000},
	} {
		for s := uint32(1); s <= reader.tip; s++ {
			c := ledgerAtClose(s)
			ledgerAtCheck(t, reader, c)                           // exact tie: that ledger
			ledgerAtCheck(t, reader, c.Add(999*time.Millisecond)) // sub-second ts
			ledgerAtCheck(t, reader, c.Add(-time.Second))         // just before: the previous ledger
		}
		ledgerAtCheck(t, reader, ledgerAtClose(reader.tip).Add(time.Second)) // after the tip
		ledgerAtCheck(t, reader, ledgerAtGenesis.AddDate(-1, 0, 0))          // before genesis
	}
}

// A hole the search crosses makes the answer unprovable: 404, never a wrong
// ledger, and always 404 when the true ledger is inside the hole.
func TestLedgerAt_GapNeverYieldsAWrongLedger(t *testing.T) {
	reader := &ledgerAtReader{capReader: &capReader{probe: &deadlineProbe{}}, first: 1, tip: 2_000, holes: map[uint32]bool{}}
	for s := uint32(1000); s <= 1010; s++ {
		reader.holes[s] = true
	}
	served := 0
	for s := uint32(1); s <= reader.tip; s++ {
		for _, ts := range []time.Time{ledgerAtClose(s), ledgerAtClose(s).Add(-time.Second)} {
			code, got := ledgerAtServe(t, reader, ts.Format(time.RFC3339))
			want := reader.want(ts)
			if code == http.StatusOK {
				served++
			}
			if (code == http.StatusOK && got.Sequence != want) || (code != http.StatusOK && code != http.StatusNotFound) {
				t.Fatalf("ts %s: status %d ledger %d, want ledger %d or 404", ts.Format(time.RFC3339), code, got.Sequence, want)
			}
		}
	}
	if served == 0 || reader.want(ledgerAtClose(1005)) != 0 || reader.want(ledgerAtClose(1500)) != 1500 {
		t.Fatal("the sweep served nothing or the model is wrong about the hole; it proves nothing")
	}
}

func TestLedgerAt_AcceptsUnixSecondsAndOffsets(t *testing.T) {
	reader := &ledgerAtReader{capReader: &capReader{probe: &deadlineProbe{}}, first: 1, tip: 2_000}
	target := ledgerAtClose(1234)
	for _, raw := range []string{
		target.Format(time.RFC3339),
		target.In(time.FixedZone("", 3600)).Format("2006-01-02T15:04:05") + "%2B01:00",
		target.Add(500 * time.Millisecond).Format(time.RFC3339Nano),
		strconv.FormatInt(target.Unix(), 10),
	} {
		if code, got := ledgerAtServe(t, reader, raw); code != http.StatusOK || got.Sequence != 1234 {
			t.Errorf("ts=%s: status %d ledger %d, want 200 ledger 1234", raw, code, got.Sequence)
		}
	}
}

func TestLedgerAt_RejectsMalformedTS(t *testing.T) {
	for _, raw := range []string{"", "abc", "-5", "1.5", "2024-06-01", "2024-06-01T12:00:00", "99999999999999999999"} {
		reader := &ledgerAtReader{capReader: &capReader{probe: &deadlineProbe{}}, first: 1, tip: 2_000}
		if code, _ := ledgerAtServe(t, reader, raw); code != http.StatusBadRequest {
			t.Errorf("ts=%q: status %d, want 400", raw, code)
		}
		if reader.pointReads+reader.recentCalls != 0 {
			t.Errorf("ts=%q reached the lake", raw)
		}
	}
}
