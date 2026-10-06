package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type opTypeReader struct {
	*opsDirReader
	gotTypes []string
	gotCur   clickhouse.ExplorerCursor
	page     clickhouse.OpTypePage
}

func (r *opTypeReader) RecentOperationsOfType(_ context.Context, _ int, cur clickhouse.ExplorerCursor, opTypes []string) (clickhouse.OpTypePage, error) {
	r.gotTypes, r.gotCur = opTypes, cur
	return r.page, nil
}

func newOpTypeHandler(page clickhouse.OpTypePage) (*Handler, *opTypeReader, *writeCapture) {
	h, base, captured := newOpsDirHandler()
	reader := &opTypeReader{opsDirReader: base, page: page}
	h.Reader = reader
	return h, reader, captured
}

func TestOperations_TypeFilterMapsToLakeEnumAndBypassesDirectoryCache(t *testing.T) {
	h, reader, captured := newOpTypeHandler(clickhouse.OpTypePage{
		Rows: []clickhouse.OpRow{{
			Seq: 63_000_000, CloseTime: time.Unix(1700000000, 0).UTC(),
			TxHash: "h", OpType: "OperationTypePayment",
		}},
		ScannedFrom: 62_995_001,
	})
	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet,
		"/v1/operations?type=payment,manage_sell_offer&type=payment", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if want := []string{"OperationTypePayment", "OperationTypeManageSellOffer"}; !reflect.DeepEqual(reader.gotTypes, want) {
		t.Errorf("types = %v, want %v (deduplicated lake enum strings)", reader.gotTypes, want)
	}
	if n := reader.calls.Load(); n != 0 {
		t.Errorf("a filtered first page read the unfiltered directory %d times; it must not be served from that cache", n)
	}
	// A short page continues strictly below the scanned span, never stops.
	if got := captured.view.NextCursor; got != "62995001.0.0" {
		t.Errorf("next_cursor = %q, want the scan floor 62995001.0.0", got)
	}
	if captured.view.OpTypeStats != nil {
		t.Error("op_type_stats describes the unfiltered network; a filtered page must not carry it")
	}
}

func TestOperations_TypeFilterCursorPassesThrough(t *testing.T) {
	h, reader, captured := newOpTypeHandler(clickhouse.OpTypePage{})
	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?type=clawback&cursor=4000.0.0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if want := (clickhouse.ExplorerCursor{Ledger: 4000}); reader.gotCur != want {
		t.Errorf("cursor = %+v, want %+v", reader.gotCur, want)
	}
	if captured.view.NextCursor != "" {
		t.Errorf("next_cursor = %q, want none once the scan reached genesis", captured.view.NextCursor)
	}
}

func TestOperations_TypeFilterRejectsUnknownType(t *testing.T) {
	for _, q := range []string{"type=bogus", "type=payment,", "type=Payment", "type=OperationTypePayment"} {
		h, reader, _ := newOpTypeHandler(clickhouse.OpTypePage{})
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
		if reader.gotTypes != nil {
			t.Errorf("%s: reached the lake with %v", q, reader.gotTypes)
		}
	}
}
