package explorer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type claimableReader struct {
	downReader
	wm        uint32
	ledgers   []uint32
	live      []clickhouse.LiveClaimableBalance
	keys      int
	gotRoles  []string
	gotLedger []uint32
}

func (r *claimableReader) EntryIndexWatermark(context.Context) (uint32, error) { return r.wm, nil }

func (r *claimableReader) AccountEntryLedgers(_ context.Context, _ string, roles []string, _ uint32, _ int) ([]uint32, error) {
	r.gotRoles = roles
	return r.ledgers, nil
}

func (r *claimableReader) LiveClaimableBalancesInLedgers(_ context.Context, _ string, ledgers []uint32, _ int) ([]clickhouse.LiveClaimableBalance, int, error) {
	r.gotLedger = ledgers
	return r.live, r.keys, nil
}

func claimable(ledger uint32, id string, amount string, claimants ...string) clickhouse.LiveClaimableBalance {
	cs := make([]any, 0, len(claimants))
	for _, c := range claimants {
		cs = append(cs, map[string]any{"destination": c, "predicate": map[string]any{"type": "unconditional"}})
	}
	return clickhouse.LiveClaimableBalance{
		Ledger: ledger, CloseTime: time.Unix(1_700_000_000, 0).UTC(), Claimants: claimants,
		Fields: map[string]any{"balance_id": id, "asset": "native", "amount": amount, "claimants": cs},
	}
}

func callAccountClaimable(t *testing.T, reader *claimableReader) (int, AccountClaimableBalancesView) {
	t.Helper()
	var view AccountClaimableBalancesView
	h := newProbeHandler(reader, nil)
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) {
		view = v.(AccountClaimableBalancesView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AccountClaimableBalancesView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/x/claimable-balances", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec := httptest.NewRecorder()
	h.AccountClaimableBalances(rec, r)
	return rec.Code, view
}

func TestAccountClaimableBalances_KeepsOnlyBalancesTheAccountCanClaim(t *testing.T) {
	reader := &claimableReader{wm: 500, ledgers: []uint32{300, 200}, keys: 2, live: []clickhouse.LiveClaimableBalance{
		claimable(300, "0000000001", "9000000000000000", aecOther, validTestAccount),
		claimable(200, "0000000002", "70", aecOther), // the prefilter matched the account elsewhere in the entry
	}}
	code, view := callAccountClaimable(t, reader)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(view.ClaimableBalances) != 1 {
		t.Fatalf("balances = %+v, want only the one naming the account", view.ClaimableBalances)
	}
	b := view.ClaimableBalances[0]
	if b.Amount != "9000000000000000" || b.Asset != "native" || b.LastModifiedLedger != 300 ||
		!strings.HasSuffix(b.BalanceID, "01") || !json.Valid(b.Entry) {
		t.Fatalf("balance = %+v, want 9000000000000000 native stroops at ledger 300 with a JSON entry", b)
	}
	if view.LowerBound || view.ThroughLedger != 500 {
		t.Fatalf("lower_bound/through = %v/%d, want false/500 under both caps", view.LowerBound, view.ThroughLedger)
	}
	if !slices.Equal(reader.gotRoles, []string{"claimant"}) || !slices.Equal(reader.gotLedger, reader.ledgers) {
		t.Fatalf("roles/ledgers = %v/%v, want [claimant] and the indexed ledgers", reader.gotRoles, reader.gotLedger)
	}
}

func TestAccountClaimableBalances_CapsServeALowerBound(t *testing.T) {
	full := make([]uint32, claimableLedgerCap)
	for _, tc := range []struct {
		name    string
		ledgers []uint32
		keys    int
	}{{"ledger cap", full, 1}, {"key cap", []uint32{1}, claimableKeyCap}} {
		_, view := callAccountClaimable(t, &claimableReader{wm: 500, ledgers: tc.ledgers, keys: tc.keys})
		if !view.LowerBound || !strings.Contains(view.CoverageNote, "may be missing") {
			t.Fatalf("%s: lower_bound/note = %v/%q, want a flagged lower bound", tc.name, view.LowerBound, view.CoverageNote)
		}
	}
}

func TestAccountClaimableBalances_UnbuiltIndexIs503(t *testing.T) {
	if code, _ := callAccountClaimable(t, &claimableReader{}); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before the entry index is built", code)
	}
}
