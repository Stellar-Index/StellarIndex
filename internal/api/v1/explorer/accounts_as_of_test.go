package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// orderedHistoryReader records whether the lake watermark had been read by
// the time each account-history scan ran.
type orderedHistoryReader struct {
	*capReader
	wmRead      *bool
	wmBeforeRun bool
}

func (r *orderedHistoryReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, clickhouse.ExplorerCursor, error) {
	r.wmBeforeRun = *r.wmRead
	return nil, clickhouse.ExplorerCursor{}, nil
}

func (r *orderedHistoryReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, clickhouse.ExplorerCursor, error) {
	r.wmBeforeRun = *r.wmRead
	return nil, clickhouse.ExplorerCursor{}, nil
}

// The account-history pages must say which lake ledger they were read
// against, taken before the scan so it never over-claims what the rows cover.
func TestAccountHistory_StampsAsOfLedger(t *testing.T) {
	const tip = 63_315_192
	for _, tc := range []struct {
		name   string
		path   string
		serve  func(*Handler, http.ResponseWriter, *http.Request)
		asOfOf func(any) uint32
	}{
		{
			"transactions", "/transactions", (*Handler).AccountTransactions,
			func(v any) uint32 { return v.(AccountTransactionsView).AsOfLedger },
		},
		{
			"operations", "/operations", (*Handler).AccountOperations,
			func(v any) uint32 { return v.(AccountOperationsView).AsOfLedger },
		},
	} {
		for _, wmOK := range []bool{true, false} {
			var wmRead bool
			reader := &orderedHistoryReader{capReader: &capReader{probe: &deadlineProbe{}}, wmRead: &wmRead}
			h := newProbeHandler(reader, nil)
			h.LakeWatermark = func(context.Context) (uint32, bool, bool) {
				wmRead = true
				if !wmOK {
					return 0, true, false
				}
				return tip, false, true
			}
			var got uint32
			var wrote bool
			h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
				got, wrote = tc.asOfOf(v), true
				w.WriteHeader(http.StatusOK)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+tc.path, nil)
			r.SetPathValue("g_strkey", validTestAccount)
			tc.serve(h, httptest.NewRecorder(), r)

			if !wrote {
				t.Fatalf("%s (watermark ok=%v): no envelope written", tc.name, wmOK)
			}
			want := uint32(0)
			if wmOK {
				want = tip
			}
			if got != want {
				t.Errorf("%s (watermark ok=%v): as_of_ledger = %d, want %d", tc.name, wmOK, got, want)
			}
			if !reader.wmBeforeRun {
				t.Errorf("%s: lake watermark read after the scan; as_of_ledger could name a ledger the rows never saw", tc.name)
			}
		}
	}
}

// resumeHistoryReader serves a short page that stopped at a scan frontier.
type resumeHistoryReader struct{ *capReader }

func (resumeHistoryReader) AccountTransactions(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, clickhouse.ExplorerCursor, error) {
	return []clickhouse.TxSummary{{Seq: 900, TxIndex: 4}}, clickhouse.ExplorerCursor{Ledger: 700, A: 2, B: 1}, nil
}

func (resumeHistoryReader) AccountOperations(context.Context, string, int, clickhouse.ExplorerCursor) ([]clickhouse.OpRow, clickhouse.ExplorerCursor, error) {
	return []clickhouse.OpRow{{Seq: 900, TxIndex: 4, OpIndex: 0}}, clickhouse.ExplorerCursor{Ledger: 700, A: 2, B: 1}, nil
}

// A short page that stopped at the reader's scan frontier is not the last
// page: next_cursor must carry the frontier, not the last row or nothing.
func TestAccountHistory_ShortPageCarriesResumeCursor(t *testing.T) {
	for _, tc := range []struct {
		path  string
		serve func(*Handler, http.ResponseWriter, *http.Request)
		next  func(any) string
		want  string
	}{
		{"/transactions", (*Handler).AccountTransactions, func(v any) string { return v.(AccountTransactionsView).NextCursor }, "700.2"},
		{"/operations", (*Handler).AccountOperations, func(v any) string { return v.(AccountOperationsView).NextCursor }, "700.2.1"},
	} {
		h := newProbeHandler(resumeHistoryReader{&capReader{probe: &deadlineProbe{}}}, nil)
		var got string
		h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
			got = tc.next(v)
			w.WriteHeader(http.StatusOK)
		}
		r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+tc.path+"?limit=50", nil)
		r.SetPathValue("g_strkey", validTestAccount)
		tc.serve(h, httptest.NewRecorder(), r)
		if got != tc.want {
			t.Fatalf("%s: next_cursor = %q, want the frontier %q", tc.path, got, tc.want)
		}
	}
}
