package explorer

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	aecIssuer  = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	aecSponsor = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
	aecOther   = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
	aecUSD     = "USD-" + aecIssuer
)

// accountEntryChangesReader indexes every ledger that has a source row and streams the rows in lake order.
type accountEntryChangesReader struct {
	*capReader
	wm       uint32
	ledgers  []uint32
	rows     []clickhouse.EntryHistorySourceRow
	gotRoles []string
}

func newAccountEntryChangesReader(wm uint32, rows []clickhouse.EntryHistorySourceRow) *accountEntryChangesReader {
	r := &accountEntryChangesReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm, rows: rows}
	for _, s := range rows {
		if !slices.Contains(r.ledgers, s.Ledger) {
			r.ledgers = append(r.ledgers, s.Ledger)
		}
	}
	slices.SortFunc(r.ledgers, func(a, b uint32) int { return cmp.Compare(b, a) })
	return r
}

func (r *accountEntryChangesReader) EntryIndexWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return r.wm, nil
}

func (r *accountEntryChangesReader) AccountEntryLedgers(_ context.Context, _ string, roles []string, maxLedger uint32, limit int) ([]uint32, error) {
	r.gotRoles = roles
	var out []uint32
	for _, l := range r.ledgers {
		if l <= maxLedger && len(out) < limit {
			out = append(out, l)
		}
	}
	return out, nil
}

func (r *accountEntryChangesReader) LiveClaimableBalancesInLedgers(context.Context, string, []uint32, int) (clickhouse.ClaimableScan, error) {
	return clickhouse.ClaimableScan{}, nil
}

func (r *accountEntryChangesReader) StreamEntryChangesForLedgers(_ context.Context, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
	for _, s := range r.rows {
		if slices.Contains(ledgers, s.Ledger) {
			if err := fn(s); err != nil {
				return err
			}
		}
	}
	return nil
}

func aecTrustline(holder string, balance int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTrustline, TrustLine: &xdr.TrustLineEntry{
		AccountId: xdr.MustAddress(holder), Asset: xdr.MustNewCreditAsset("USD", aecIssuer).ToTrustLineAsset(),
		Balance: xdr.Int64(balance), Limit: xdr.Int64(1 << 62),
	}}}
}

func aecSponsored(e xdr.LedgerEntry, sponsor string) xdr.LedgerEntry {
	e.Ext = xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(sponsor)}}
	return e
}

// aecRow is a lake source row for the trustline e at op 0 of tx.
func aecRow(t *testing.T, ledger, seq uint32, tx string, change uint32, changeType string, e xdr.LedgerEntry) clickhouse.EntryHistorySourceRow {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	keyXDR, err := xdr.MarshalBase64(k)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	entryXDR, err := xdr.MarshalBase64(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return clickhouse.EntryHistorySourceRow{
		Ledger: ledger, CloseTime: time.Unix(1_700_000_000, 0).UTC(), TxHash: tx, OpIndex: 0, ChangeIndex: change,
		IntraLedgerSeq: seq, ChangeType: changeType, EntryType: "trustline", KeyXDR: keyXDR, EntryXDR: entryXDR,
	}
}

// aecFixture: the account's USD trustline is created sponsored in ledger 90, then updated twice in ledger 100 by
// two transactions whose apply order (intra_ledger_seq) is the reverse of their hash order; one update row is an
// unmerged duplicate and another account's trustline sits in the same ledger.
func aecFixture(t *testing.T) []clickhouse.EntryHistorySourceRow {
	mine := func(bal int64) xdr.LedgerEntry { return aecTrustline(validTestAccount, bal) }
	return []clickhouse.EntryHistorySourceRow{
		aecRow(t, 90, 1, "c1", 0, "created", aecSponsored(mine(10), aecSponsor)),
		aecRow(t, 100, 2, "99", 0, "created", aecTrustline(aecOther, 5)),
		aecRow(t, 100, 4, "a1", 0, "state", mine(25)),
		aecRow(t, 100, 4, "a1", 1, "updated", mine(40)),
		aecRow(t, 100, 4, "a1", 1, "updated", mine(40)),
		aecRow(t, 100, 3, "b1", 0, "state", mine(10)),
		aecRow(t, 100, 3, "b1", 1, "updated", mine(25)),
	}
}

func callAccountEntryChanges(t *testing.T, reader *accountEntryChangesReader, account string, q url.Values, limit int) (int, AccountEntryChangesView) {
	t.Helper()
	var view AccountEntryChangesView
	h := newProbeHandler(reader, nil)
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSONAt = func(w http.ResponseWriter, v any, _, _ bool, _ time.Time) {
		view = v.(AccountEntryChangesView)
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AccountEntryChangesView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/x/entry-changes?"+q.Encode(), nil)
	r.SetPathValue("g_strkey", account)
	rec := httptest.NewRecorder()
	h.AccountEntryChanges(rec, r)
	return rec.Code, view
}

func TestAccountEntryChanges_NewestFirstForTheAccountOnly(t *testing.T) {
	reader := newAccountEntryChangesReader(120, aecFixture(t))
	code, view := callAccountEntryChanges(t, reader, validTestAccount, nil, 25)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	want := []struct {
		ledger     uint32
		tx, amount string
	}{{100, "a1", "40"}, {100, "b1", "25"}, {90, "c1", "10"}}
	if len(view.Changes) != len(want) {
		t.Fatalf("changes = %+v, want %d rows", view.Changes, len(want))
	}
	for i, w := range want {
		e := view.Changes[i]
		if e.Ledger != w.ledger || e.TxHash != w.tx || e.Amount != w.amount || e.Role != "owner" || e.Asset != aecUSD || !json.Valid(e.Entry) {
			t.Fatalf("row %d = %+v, want ledger %d tx %s amount %s owner of %s with a JSON entry", i, e, w.ledger, w.tx, w.amount, aecUSD)
		}
	}
	if !slices.Equal(view.Changes[0].Changed, []string{"balance"}) || view.Changes[0].IntraLedgerSeq != 4 {
		t.Fatalf("newest row changed/seq = %v/%d, want [balance]/4", view.Changes[0].Changed, view.Changes[0].IntraLedgerSeq)
	}
	if view.NextCursor != "" || view.LowerBound || view.ThroughLedger != 120 {
		t.Fatalf("cursor/lower_bound/through = %q/%v/%d, want none/false/120 on an exhausted index", view.NextCursor, view.LowerBound, view.ThroughLedger)
	}
	if !slices.Equal(reader.gotRoles, []string{"owner", "claimant"}) || !slices.Equal(view.Roles, reader.gotRoles) {
		t.Fatalf("roles asked/served = %v/%v, want the owner+claimant default on both", reader.gotRoles, view.Roles)
	}
}

func TestAccountEntryChanges_RoleFilter(t *testing.T) {
	reader := newAccountEntryChangesReader(120, aecFixture(t))
	q := url.Values{"role": {"sponsor"}}
	if _, view := callAccountEntryChanges(t, reader, validTestAccount, q, 25); len(view.Changes) != 0 {
		t.Fatalf("holder as sponsor = %+v, want nothing", view.Changes)
	}
	_, view := callAccountEntryChanges(t, reader, aecSponsor, q, 25)
	if len(view.Changes) != 1 || view.Changes[0].Role != "sponsor" || view.Changes[0].Ledger != 90 || view.Changes[0].Amount != "" {
		t.Fatalf("sponsor rows = %+v, want one sponsor row at ledger 90 without an amount", view.Changes)
	}
	if !slices.Equal(reader.gotRoles, []string{"sponsor"}) {
		t.Fatalf("index asked for roles %v, want [sponsor]", reader.gotRoles)
	}
	q.Set("role", "owner,holder")
	if code, _ := callAccountEntryChanges(t, reader, validTestAccount, q, 25); code != http.StatusBadRequest {
		t.Fatalf("role=owner,holder status = %d, want 400", code)
	}
}

func TestAccountEntryChanges_CursorWalksInsideALedger(t *testing.T) {
	reader := newAccountEntryChangesReader(120, aecFixture(t))
	var got []string
	q := url.Values{}
	for page := 0; page < 6; page++ {
		code, view := callAccountEntryChanges(t, reader, validTestAccount, q, 1)
		if code != http.StatusOK {
			t.Fatalf("page %d status = %d (cursor %q)", page, code, q.Get("cursor"))
		}
		for _, e := range view.Changes {
			got = append(got, fmt.Sprintf("%d/%s/%s", e.Ledger, e.TxHash, e.Amount))
		}
		if view.NextCursor == "" {
			break
		}
		q.Set("cursor", view.NextCursor)
	}
	if want := []string{"100/a1/40", "100/b1/25", "90/c1/10"}; !slices.Equal(got, want) {
		t.Fatalf("paged rows = %v, want %v", got, want)
	}
}

// A role the account rarely holds can walk past the pass cap without filling the page: the page is partial and
// its cursor resumes strictly below the last ledger walked.
func TestAccountEntryChanges_SparseRoleResumesBelowTheLastLedger(t *testing.T) {
	rows := []clickhouse.EntryHistorySourceRow{aecRow(t, 5, 1, "c1", 0, "created", aecTrustline(validTestAccount, 1))}
	for l := uint32(6); l <= 305; l++ {
		rows = append(rows, aecRow(t, l, 1, "d1", 0, "created", aecTrustline(aecOther, 1)))
	}
	reader := newAccountEntryChangesReader(400, rows)
	_, view := callAccountEntryChanges(t, reader, validTestAccount, nil, 25)
	if len(view.Changes) != 0 || !view.LowerBound || view.NextCursor != "50.-.-1.0.-" {
		t.Fatalf("partial page = %d rows, lower_bound %v, cursor %q; want 0/true/50.-.-1.0.-", len(view.Changes), view.LowerBound, view.NextCursor)
	}
	_, view = callAccountEntryChanges(t, reader, validTestAccount, url.Values{"cursor": {view.NextCursor}}, 25)
	if len(view.Changes) != 1 || view.Changes[0].Ledger != 5 || view.NextCursor != "" || view.LowerBound {
		t.Fatalf("resumed page = %+v cursor %q lower_bound %v, want the ledger-5 row and the end", view.Changes, view.NextCursor, view.LowerBound)
	}
}

func TestAccountEntryChanges_NotBuiltIs503(t *testing.T) {
	code, _ := callAccountEntryChanges(t, newAccountEntryChangesReader(0, nil), validTestAccount, nil, 25)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while the entry index watermark is 0", code)
	}
}
