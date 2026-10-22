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
	wm, cwm    uint32
	ledgers    []uint32
	live       []clickhouse.LiveClaimableBalance
	keys       int
	unresolved int
	gotRoles   []string
	gotLedger  []uint32
}

func (r *claimableReader) EntryIndexWatermark(context.Context) (uint32, error) { return r.wm, nil }

func (r *claimableReader) ClaimableIndexWatermark(context.Context) (uint32, error) { return r.cwm, nil }

func (r *claimableReader) IndexedClaimableBalances(context.Context, string, int) (clickhouse.ClaimableScan, error) {
	return clickhouse.ClaimableScan{Live: r.live, Keys: r.keys}, nil
}

func (r *claimableReader) AccountEntryLedgers(_ context.Context, _ string, roles []string, _ uint32, _ int) ([]uint32, error) {
	r.gotRoles = roles
	return r.ledgers, nil
}

func (r *claimableReader) LiveClaimableBalancesInLedgers(_ context.Context, _ string, ledgers []uint32, _ int) (clickhouse.ClaimableScan, error) {
	r.gotLedger = ledgers
	return clickhouse.ClaimableScan{Live: r.live, Keys: r.keys, Unresolved: r.unresolved}, nil
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
		name             string
		ledgers          []uint32
		keys, unresolved int
		note             string
	}{
		{"ledger cap", full, 1, 0, "may be missing"},
		{"key cap", []uint32{1}, claimableKeyCap, 0, "may be missing"},
		{"no current-state row", []uint32{1}, 3, 1, "1 balance(s) naming the account have no current-state row"},
	} {
		_, view := callAccountClaimable(t, &claimableReader{wm: 500, ledgers: tc.ledgers, keys: tc.keys, unresolved: tc.unresolved})
		if !view.LowerBound || !strings.Contains(view.CoverageNote, tc.note) {
			t.Fatalf("%s: lower_bound/note = %v/%q, want a flagged lower bound", tc.name, view.LowerBound, view.CoverageNote)
		}
	}
}

func TestAccountClaimableBalances_UnbuiltIndexIs503(t *testing.T) {
	if code, _ := callAccountClaimable(t, &claimableReader{}); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 before the entry index is built", code)
	}
}

func TestAccountClaimableBalances_BuiltClaimableIndexSkipsTheLedgerScan(t *testing.T) {
	reader := &claimableReader{cwm: 700, keys: 1, live: []clickhouse.LiveClaimableBalance{
		claimable(650, "0000000003", "12", validTestAccount),
	}}
	code, view := callAccountClaimable(t, reader)
	if code != http.StatusOK || len(view.ClaimableBalances) != 1 || view.ThroughLedger != 700 || view.LowerBound {
		t.Fatalf("status/balances/through/lower = %d/%d/%d/%v, want 200, one balance through the claimable watermark 700",
			code, len(view.ClaimableBalances), view.ThroughLedger, view.LowerBound)
	}
	if reader.gotRoles != nil || reader.gotLedger != nil {
		t.Fatalf("ledger scan ran (roles %v, ledgers %v) although the claimable index is built", reader.gotRoles, reader.gotLedger)
	}
	_, view = callAccountClaimable(t, &claimableReader{cwm: 700, keys: claimableKeyCap})
	if !view.LowerBound || !strings.Contains(view.CoverageNote, "older ones are left out") {
		t.Fatalf("capped indexed read: lower_bound/note = %v/%q, want a flagged lower bound", view.LowerBound, view.CoverageNote)
	}
}
