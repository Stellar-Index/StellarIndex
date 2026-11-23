package explorer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// walkCHReader is movementsArmReader with keyset paging, and a watermark read that fails the first wmFailures calls.
type walkCHReader struct {
	*movementsArmReader
	wmFailures int
}

func (r *walkCHReader) Cap67MovementsWatermark(context.Context) (uint32, error) {
	if r.wmFailures > 0 {
		r.wmFailures--
		return 0, errors.New("clickhouse: cap67 watermark: connection reset")
	}
	return r.wm, nil
}

func (r *walkCHReader) AccountMovements(_ context.Context, _ string, limit int, cur clickhouse.AccountMovementCursor, f clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	var out []clickhouse.AccountMovementRow
	for _, row := range r.chRows {
		if (f.HasMaxLedger && row.Ledger > f.MaxLedger) || (cur.Ledger != 0 && row.Ledger >= cur.Ledger) {
			continue
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// flakyTail is scopedSEP41Tail whose first failures reads error.
type flakyTail struct {
	scopedSEP41Tail
	failures int
}

func (t *flakyTail) ListSEP41TransfersByAddress(ctx context.Context, a string, limit int, cur timescale.SEP41TransferCursor, dir, contractID string, floor uint32) ([]timescale.SEP41TransferRow, error) {
	if t.failures > 0 {
		t.failures--
		return nil, errors.New("statement timeout")
	}
	return t.scopedSEP41Tail.ListSEP41TransfersByAddress(ctx, a, limit, cur, dir, contractID, floor)
}

func callMovementsPage(t *testing.T, reader ExplorerReader, tail SEP41MovementsReader, limit int, cursor string) (AccountMovementsView, int) {
	t.Helper()
	view, rec, _ := serveMovementsPage(t, reader, tail, limit, cursor)
	return view, rec.Code
}

func serveMovementsPage(t *testing.T, reader ExplorerReader, tail SEP41MovementsReader, limit int, cursor string) (view AccountMovementsView, rec *httptest.ResponseRecorder, degraded bool) {
	t.Helper()
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AccountMovementsView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, d bool, _ time.Time) {
		view, degraded = v.(AccountMovementsView), d
		w.WriteHeader(http.StatusOK)
	}
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements?"+q.Encode(), nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec = httptest.NewRecorder()
	h.AccountMovements(rec, r)
	return view, rec, degraded
}

// A 200 is a complete merge of both arms for its range, so an arm that owns part of the range and failed is a retryable 503 at any
// page length: a short 200 carries no next_cursor, which the client reads as the end of the list.
func TestAccountMovements_ArmFailureIsRetryable503(t *testing.T) {
	floor := timescale.MovementsFloor()
	wm := floor + 1000
	pgFloor := wm + 1
	ch := func(l uint32) clickhouse.AccountMovementRow {
		return clickhouse.AccountMovementRow{Address: validTestAccount, Ledger: l, TxHash: validTestTxHash, Amount: big.NewInt(1)}
	}
	pinned := func(l uint32) string { return fmt.Sprintf("%d.%s.0.0.%d", l, validTestTxHash, wm) }
	cases := []struct {
		name      string
		wmFails   bool
		tail      SEP41MovementsReader
		cursor    string
		want      int
		wantRetry string
	}{
		{name: "page 1, short, tail fails", tail: failingSEP41Tail{}, want: http.StatusServiceUnavailable, wantRetry: retryAfterDependencyDown},
		{name: "page 1, short, watermark fails", wmFails: true, tail: &stubSEP41Tail{}, want: http.StatusServiceUnavailable, wantRetry: retryAfterDependencyDown},
		{name: "page 2, cursor at pgFloor, tail fails", tail: failingSEP41Tail{}, cursor: pinned(pgFloor), want: http.StatusServiceUnavailable, wantRetry: retryAfterDependencyDown},
		{name: "page 2, cursor above pgFloor, tail fails", tail: failingSEP41Tail{}, cursor: pinned(pgFloor + 7), want: http.StatusServiceUnavailable, wantRetry: retryAfterDependencyDown},
		{name: "page 2, cursor below pgFloor, tail fails (owns nothing)", tail: failingSEP41Tail{}, cursor: pinned(wm), want: http.StatusOK},
		{name: "page 1, healthy", tail: &stubSEP41Tail{}, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &walkCHReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
			if tc.wmFails {
				reader.wmFailures = 1
			}
			for i := range uint32(10) {
				reader.chRows = append(reader.chRows, ch(wm-1-i))
			}
			view, rec, degraded := serveMovementsPage(t, reader, tc.tail, 25, tc.cursor)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (served %d rows, next_cursor %q)", rec.Code, tc.want, len(view.Movements), view.NextCursor)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			if tc.want == http.StatusOK && degraded {
				t.Error("a complete merge was flagged degraded")
			}
		})
	}
}

// A walk whose first page hits a failed merge arm must still serve every seeded movement exactly once: the failure may cost a
// retryable 503, never a next_cursor that commits the walk past the failed arm's rows.
func TestAccountMovements_WalkSurvivesArmFailure(t *testing.T) {
	floor := timescale.MovementsFloor()
	when := time.Unix(1_700_000_000, 0).UTC()
	ch := func(l uint32, asset string) clickhouse.AccountMovementRow {
		return clickhouse.AccountMovementRow{
			Address: validTestAccount, Ledger: l, LedgerCloseTime: when, TxHash: validTestTxHash,
			Direction: clickhouse.AccountMovementReceived, MovementKind: "transfer", Provenance: "cap67_derived", Asset: asset, Amount: big.NewInt(1),
		}
	}
	pg := func(l uint32) timescale.SEP41TransferRow {
		return timescale.SEP41TransferRow{
			ContractID: validTestContract, Ledger: l, TxHash: validTestTxHash, ObservedAt: when,
			FromAddr: validTestContract, ToAddr: validTestAccount, Amount: big.NewInt(1),
		}
	}

	cases := []struct {
		name  string
		build func() (*walkCHReader, *flakyTail)
	}{
		{"tail read fails on page 1", func() (*walkCHReader, *flakyTail) {
			wm := floor + 1000
			r := &walkCHReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
			for i := range uint32(40) {
				r.chRows = append(r.chRows, ch(wm-i, "native"))
			}
			tail := &flakyTail{failures: 1}
			tail.rows = []timescale.SEP41TransferRow{pg(wm + 50)}
			return r, tail
		}},
		{"tail read fails on a short page 1", func() (*walkCHReader, *flakyTail) {
			wm := floor + 1000
			r := &walkCHReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
			for i := range uint32(10) {
				r.chRows = append(r.chRows, ch(wm-i, "native"))
			}
			tail := &flakyTail{failures: 1}
			for i := range uint32(20) {
				tail.rows = append(tail.rows, pg(wm+1+i))
			}
			return r, tail
		}},
		{"healthy walk", func() (*walkCHReader, *flakyTail) {
			wm := floor + 1000
			r := &walkCHReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
			for i := range uint32(30) {
				r.chRows = append(r.chRows, ch(wm-i, "native"))
			}
			tail := &flakyTail{}
			for i := range uint32(20) {
				tail.rows = append(tail.rows, pg(wm+1+i))
			}
			return r, tail
		}},
		{"watermark read fails on page 1", func() (*walkCHReader, *flakyTail) {
			wm := floor + 5000
			r := &walkCHReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}, wmFailures: 1}
			r.chRows = append(r.chRows, ch(floor+4500, "native")) // above page 1's watched-only cursor, below the real watermark
			tail := &flakyTail{}
			for i := range uint32(30) {
				l := floor + 4029 - i
				r.chRows = append(r.chRows, ch(l, "USDC-"+validTestAccount)) // the archive holds the watched rows too
				tail.rows = append(tail.rows, pg(l))
			}
			return r, tail
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, tail := tc.build()
			want := map[uint32]bool{}
			for _, r := range reader.chRows {
				want[r.Ledger] = true
			}
			for _, r := range tail.rows {
				want[r.Ledger] = true
			}
			served := map[uint32]int{}
			cursor := ""
			for range 20 {
				view, code := callMovementsPage(t, reader, tail, 25, cursor)
				if code == http.StatusServiceUnavailable {
					continue // a client retries the same cursor
				}
				if code != http.StatusOK {
					t.Fatalf("status %d", code)
				}
				for _, m := range view.Movements {
					served[m.Ledger]++
				}
				if view.NextCursor == "" {
					break
				}
				cursor = view.NextCursor
			}
			for l := range want {
				if served[l] != 1 {
					t.Errorf("movement at ledger %d served %d times on the walk, want exactly once", l, served[l])
				}
			}
			if len(served) != len(want) {
				t.Errorf("walk served %d distinct movements, want %d", len(served), len(want))
			}
		})
	}
}

// sacLookupErrReader errors both SAC lookups, so a tail row's asset stays the raw contract id.
type sacLookupErrReader struct {
	*movementsArmReader
}

func (*sacLookupErrReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return "", false, errors.New("connection reset")
}

func (*sacLookupErrReader) SACAssetFromEvents(context.Context, string) (string, bool, error) {
	return "", false, errors.New("connection reset")
}

// A tail row whose SAC lookup errored carries the raw contract id instead of the folded asset; the page must say it is degraded.
func TestAccountMovements_TailSACLookupFailureIsDegraded(t *testing.T) {
	wm := timescale.MovementsFloor() + 1000
	reader := &sacLookupErrReader{&movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract, Ledger: wm + 5, TxHash: validTestTxHash,
		ObservedAt: time.Unix(1_700_000_000, 0).UTC(), ToAddr: validTestAccount, Amount: big.NewInt(1),
	}}}
	var degraded bool
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.WriteJSONAt = func(w http.ResponseWriter, _ any, _, d bool, _ time.Time) {
		degraded = d
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)
	if !degraded {
		t.Fatal("a tail row's SAC lookup failed but the page was served with degraded=false")
	}
}

// An uncaptured contract whose SAC events probe errored is unanswered, not "not in the lake": never the authoritative 404. A
// transport or deadline error is a retryable 503; any other error is the 500 internal problem, not a "read budget" claim.
func TestContractWasm_EventsProbeErrorStatus(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     int
		wantType string
	}{
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, http.StatusServiceUnavailable, "contract-wasm-timeout"},
		{"deadline", fmt.Errorf("sac events probe: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "contract-wasm-timeout"},
		{"schema error", errors.New("code: 47, message: Missing columns: 'topic_3'"), http.StatusInternalServerError, "errors/internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeHandler(&eventsProbeErrReader{capReader: &capReader{probe: &deadlineProbe{}}, evErr: tc.err}, nil)
			var gotType string
			h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, _ string, status int, _ string) {
				gotType = typeURL
				w.WriteHeader(status)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validTestContract+"/wasm", nil)
			r.SetPathValue("contract_id", validTestContract)
			rec := httptest.NewRecorder()
			h.ContractWasm(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d when the SAC events probe failed with %v", rec.Code, tc.want, tc.err)
			}
			if !strings.HasSuffix(gotType, tc.wantType) {
				t.Errorf("problem type %q, want suffix %q", gotType, tc.wantType)
			}
		})
	}
}

// A failed SAC lookup behind a position label is recorded, so the positions snapshot is flagged rather than served clean.
func TestPositionAssetResolver_SACLookupFailureIsRecorded(t *testing.T) {
	h := newProbeHandler(&sacLookupErrReader{&movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}}}, nil)
	var failed bool
	label := h.newPositionAssetResolver(context.Background(), &failed)(validTestContract)
	if !failed {
		t.Fatalf("label %q came from a failed SAC lookup but the failure was not recorded", label)
	}
	if want := fmt.Sprint(truncPositionsContract(validTestContract)); label != want {
		t.Errorf("label = %q, want the truncated id %q", label, want)
	}
}
