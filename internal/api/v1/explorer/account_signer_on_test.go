package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type signerOnReader struct {
	downReader
	wm       uint32
	ledgers  []uint32
	scan     clickhouse.SignerScan
	gotRoles []string
}

func (r *signerOnReader) SignerIndexWatermark(context.Context) (uint32, error) { return r.wm, nil }

func (r *signerOnReader) AccountEntryLedgers(_ context.Context, _ string, roles []string, _ uint32, _ int) ([]uint32, error) {
	r.gotRoles = roles
	return r.ledgers, nil
}

func (r *signerOnReader) AccountsSignedBy(context.Context, string, []uint32, int) (clickhouse.SignerScan, error) {
	return r.scan, nil
}

func callAccountSignerOn(t *testing.T, reader *signerOnReader) (int, AccountSignerOnView) {
	t.Helper()
	var view AccountSignerOnView
	h := newProbeHandler(reader, nil)
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) {
		view = v.(AccountSignerOnView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AccountSignerOnView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/x/signer-on", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec := httptest.NewRecorder()
	h.AccountSignerOn(rec, r)
	return rec.Code, view
}

func TestAccountSignerOn_ListsSignedAccountsThroughTheSignerRole(t *testing.T) {
	reader := &signerOnReader{wm: 900, ledgers: []uint32{850, 800}, scan: clickhouse.SignerScan{
		Signed: []clickhouse.SignedAccount{{Account: "GOTHER", Weight: 2, Ledger: 850}}, Candidates: 3,
	}}
	code, view := callAccountSignerOn(t, reader)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !slices.Equal(reader.gotRoles, []string{clickhouse.EntryIndexRoleSigner}) {
		t.Fatalf("roles = %v, want the signer role only", reader.gotRoles)
	}
	want := []AccountSignerOn{{Account: "GOTHER", Weight: 2, LastModifiedLedger: 850}}
	if !slices.Equal(view.SignerOn, want) || view.ThroughLedger != 900 || view.LowerBound {
		t.Fatalf("view = %+v", view)
	}
}

func TestAccountSignerOn_EmptyListIsAnArrayNotNull(t *testing.T) {
	_, view := callAccountSignerOn(t, &signerOnReader{wm: 900})
	if view.SignerOn == nil {
		t.Fatal("signer_on must encode as [] when the key signs for nothing")
	}
}

func TestAccountSignerOn_CapsServeALowerBound(t *testing.T) {
	ledgers := make([]uint32, signerOnLedgerCap)
	_, view := callAccountSignerOn(t, &signerOnReader{wm: 900, ledgers: ledgers})
	if !view.LowerBound || !strings.Contains(view.CoverageNote, "most recent signer ledgers") {
		t.Fatalf("ledger cap: view = %+v", view)
	}
	_, view = callAccountSignerOn(t, &signerOnReader{wm: 900, ledgers: []uint32{1}, scan: clickhouse.SignerScan{Candidates: signerOnAccountCap}})
	if !view.LowerBound {
		t.Fatalf("account cap: view = %+v", view)
	}
}

func TestAccountSignerOn_UnbuiltIndexIs503(t *testing.T) {
	code, _ := callAccountSignerOn(t, &signerOnReader{})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}
