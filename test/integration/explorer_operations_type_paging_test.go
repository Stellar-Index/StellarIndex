//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// opKey orders like the stellar.operations sort key.
type opKey struct{ L, T, O uint32 }

func (a opKey) less(b opKey) bool {
	if a.L != b.L {
		return a.L < b.L
	}
	if a.T != b.T {
		return a.T < b.T
	}
	return a.O < b.O
}

// TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup walks
// /v1/operations?type= through the real handler and reader over a sparse
// type: matches sit on and beside the 5,000-ledger scan floors, several share
// a ledger and a transaction, one is an un-merged duplicate part, and payments
// are interleaved as non-matches. Every limit must return each match exactly
// once, newest first, with every cursor strictly below the last.
func TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.operations"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.operations") })

	// A range no other test seeds; the walk starts above it and stops below it.
	const top = uint32(3_500_000_000)
	const window = 5000
	start := top + 1
	floor := func(k uint32) uint32 { return start - k*window } // short-page scan floors
	bottom := floor(4) - 1

	matches := []opKey{
		{top, 2, 0},
		{top, 2, 1},
		{top, 2, 2},
		{top, 5, 0},
		{floor(1), 0, 0},
		{floor(1) - 1, 0, 0},
		{floor(1) - 1, 1, 0},
		{floor(2), 3, 1},
		{floor(2) - 1, 0, 0},
		{floor(3), 0, 0},
		{floor(4) + 9, 0, 0},
	}
	nonMatches := []opKey{{top, 2, 3}, {floor(1), 0, 1}, {floor(2), 3, 0}, {floor(3) + 1, 0, 0}, {bottom, 0, 0}}
	insert := func(k opKey, opType, ingestedAt string) { insertLakeOp(ctx, t, raw, k, opType, ingestedAt) }
	for _, k := range matches {
		insert(k, "OperationTypeInflation", "2026-09-01 00:00:00")
	}
	for _, k := range nonMatches {
		insert(k, "OperationTypePayment", "2026-09-01 00:00:00")
	}
	dup := matches[4]
	insert(dup, "OperationTypeInflation", "2026-09-02 00:00:00") // a second, un-merged part
	// Physical rows on purpose: the walk must collapse exactly these two.
	var copies []struct {
		IngestedAt time.Time `ch:"ingested_at"`
	}
	if err := raw.Select(ctx, &copies, `SELECT ingested_at FROM stellar.operations WHERE ledger_seq = ? AND tx_index = 0 AND op_index = 0`, dup.L); err != nil || len(copies) != 2 {
		t.Fatalf("duplicate rows = %d (err %v), want 2 un-merged copies", len(copies), err)
	}

	er, err := clickhouse.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	for _, limit := range []int{1, 2, 3, 50} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			assertServedExactly(t, walkTypedOperations(t, er, limit, opKey{start, 0, 0}, bottom), matches)
		})
	}
}

func insertLakeOp(ctx context.Context, t *testing.T, raw driver.Conn, k opKey, opType, ingestedAt string) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr, ingested_at)
		VALUES (%d, toDateTime('2030-01-01 00:00:00', 'UTC'), '%064x', %d, %d, '%s', '', '', toDateTime('%s', 'UTC'))`,
		k.L, uint64(k.L)<<8|uint64(k.T), k.T, k.O, opType, ingestedAt)
	if err := raw.Exec(ctx, q); err != nil {
		t.Fatalf("insert %v: %v", k, err)
	}
}

// assertServedExactly: walkTypedOperations already proved strict descent, so
// equal length plus full membership means each match exactly once.
func assertServedExactly(t *testing.T, got, matches []opKey) {
	t.Helper()
	if len(got) != len(matches) {
		t.Fatalf("walk returned %d ops, want %d: %v", len(got), len(matches), got)
	}
	seen := map[opKey]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range matches {
		if !seen[k] {
			t.Errorf("match %v never served (gap)", k)
		}
	}
}

func typedOpsHandler(t *testing.T, er *clickhouse.ExplorerReader, page *explorer.OperationsView) *explorer.Handler {
	capture := func(w http.ResponseWriter, data any) {
		*page, _ = data.(explorer.OperationsView)
		w.WriteHeader(http.StatusOK)
	}
	return &explorer.Handler{
		Reader: er,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, r *http.Request, def, _ int) (int, bool) {
			n, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil {
				return def, true
			}
			return n, true
		},
		LakeWatermark: func(context.Context) (uint32, bool, bool) { return 0, false, false },
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, title string, status int, detail string) {
			t.Errorf("problem %d %s: %s", status, title, detail)
			w.WriteHeader(status)
		},
		WriteJSON:   func(w http.ResponseWriter, data any, _ bool) { capture(w, data) },
		WriteJSONAt: func(w http.ResponseWriter, data any, _, _ bool, _ time.Time) { capture(w, data) },
	}
}

// appendTypedPage appends a page's ops at or above bottom, failing unless
// each is strictly below the request cursor and the previous op.
func appendTypedPage(t *testing.T, out []opKey, ops []explorer.OpView, cur opKey, bottom uint32) []opKey {
	t.Helper()
	for _, o := range ops {
		k := opKey{o.Ledger, o.TxIndex, o.OpIndex}
		if !k.less(cur) {
			t.Fatalf("cursor %v served %v, not strictly below it", cur, k)
		}
		if n := len(out); n > 0 && !k.less(out[n-1]) {
			t.Fatalf("op %v after %v: duplicate or out of order", k, out[n-1])
		}
		if k.L >= bottom {
			out = append(out, k)
		}
	}
	return out
}

// walkTypedOperations pages ?type=inflation from cursor `from` until the
// cursor drops below `bottom`, failing on any ordering or cursor regression.
func walkTypedOperations(t *testing.T, er *clickhouse.ExplorerReader, limit int, from opKey, bottom uint32) []opKey {
	t.Helper()
	var page explorer.OperationsView
	h := typedOpsHandler(t, er, &page)
	var out []opKey
	cur := from
	for pages := 0; cur.L >= bottom; pages++ {
		if pages > 64 {
			t.Fatalf("no progress after %d pages at cursor %v", pages, cur)
		}
		page = explorer.OperationsView{}
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v1/operations?type=inflation&limit=%d&cursor=%d.%d.%d", limit, cur.L, cur.T, cur.O), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("cursor %v: status %d", cur, rec.Code)
		}
		if len(page.Operations) > limit {
			t.Fatalf("cursor %v: %d ops, limit %d", cur, len(page.Operations), limit)
		}
		out = appendTypedPage(t, out, page.Operations, cur, bottom)
		if page.NextCursor == "" {
			break
		}
		next := parseOpCursor(t, page.NextCursor)
		if !next.less(cur) {
			t.Fatalf("next_cursor %v does not move below %v", next, cur)
		}
		cur = next
	}
	return out
}

func parseOpCursor(t *testing.T, s string) opKey {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		t.Fatalf("cursor %q is not ledger.tx.op", s)
	}
	var n [3]uint32
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatalf("cursor %q: %v", s, err)
		}
		n[i] = uint32(v)
	}
	return opKey{n[0], n[1], n[2]}
}
