package explorer

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// scopedSEP41Tail models ListSEP41TransfersByAddress's SQL: floor,
// contract and keyset predicates apply BEFORE the LIMIT. rows must be
// newest-first with distinct ledgers (the cursor compares on ledger).
type scopedSEP41Tail struct {
	rows []timescale.SEP41TransferRow
}

func (s *scopedSEP41Tail) ListSEP41TransfersByAddress(_ context.Context, _ string, limit int, cur timescale.SEP41TransferCursor, _, contractID string, floor uint32) ([]timescale.SEP41TransferRow, error) {
	var out []timescale.SEP41TransferRow
	for _, r := range s.rows {
		if r.Ledger < floor || (contractID != "" && r.ContractID != contractID) {
			continue
		}
		if cur.IsSet() && r.Ledger >= cur.Ledger {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// sacNamingReader resolves one SAC contract to its classic name, the way
// the lake's SACClassicAssetName does for a captured SAC instance.
type sacNamingReader struct {
	*movementsArmReader
	sac, name string
}

func (r *sacNamingReader) SACClassicAssetName(_ context.Context, contractID string) (string, bool, error) {
	if contractID == r.sac {
		return r.name, true, nil
	}
	return "", false, nil
}

func callMovementsQuery(t *testing.T, reader ExplorerReader, tail SEP41MovementsReader, limit int, q url.Values) AccountMovementsView {
	t.Helper()
	var captured AccountMovementsView
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view, ok := v.(AccountMovementsView)
		if !ok {
			t.Fatalf("WriteJSON received %T, want AccountMovementsView", v)
		}
		captured = view
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements?"+q.Encode(), nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)
	return captured
}

// TestAccountMovements_AssetFilterScopesPostgresTailBeforeLimit: the
// account's newest Postgres transfers are all a non-matching token and its
// USDC transfers sit below them. ?asset=USDC must return a full page of USDC
// with a cursor that reaches the rest; a filter applied after the LIMIT
// returned an empty page, no cursor, and stranded every USDC row.
func TestAccountMovements_AssetFilterScopesPostgresTailBeforeLimit(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	floor := timescale.SEP41MovementsFloorLedger
	when := time.Unix(1_700_000_000, 0).UTC()
	row := func(ledger uint32, contract string) timescale.SEP41TransferRow {
		return timescale.SEP41TransferRow{
			ContractID: contract, Ledger: ledger, TxHash: validTestTxHash, ObservedAt: when,
			FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(int64(ledger)),
		}
	}
	var rows []timescale.SEP41TransferRow
	for l := floor + 104; l >= floor+100; l-- { // newest: another token
		rows = append(rows, row(l, validTestContract))
	}
	for l := floor + 13; l >= floor+10; l-- { // older: the filtered asset
		rows = append(rows, row(l, sac))
	}
	reader := &sacNamingReader{
		movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}},
		sac:                sac,
		name:               "USDC:" + validTestAccount,
	}
	tail := &scopedSEP41Tail{rows: rows}
	q := url.Values{"asset": {usdc.String()}}

	page1 := callMovementsQuery(t, reader, tail, 3, q)
	if got := ledgersOf(page1); !slices.Equal(got, []uint32{floor + 13, floor + 12, floor + 11}) {
		t.Fatalf("page 1 ledgers = %v, want the three newest USDC transfers %v", got, []uint32{floor + 13, floor + 12, floor + 11})
	}
	for _, m := range page1.Movements {
		if m.Asset != usdc.String() {
			t.Errorf("page 1 row at ledger %d has asset %q, want %q", m.Ledger, m.Asset, usdc.String())
		}
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1 is full but next_cursor is empty — the USDC transfer at the bottom is unreachable")
	}

	q.Set("cursor", page1.NextCursor)
	page2 := callMovementsQuery(t, reader, tail, 3, q)
	if got := ledgersOf(page2); !slices.Equal(got, []uint32{floor + 10}) {
		t.Fatalf("page 2 ledgers = %v, want the remaining USDC transfer [%d]", got, floor+10)
	}
	if page2.NextCursor != "" {
		t.Errorf("page 2 next_cursor = %q, want empty (history exhausted)", page2.NextCursor)
	}
}

// TestSEP41AssetScope pins the ?asset= -> contract_id mapping: native and
// classic assets scope to their derived SAC and render as the asset; any
// other value can only equal a raw contract_id.
func TestSEP41AssetScope(t *testing.T) {
	xlmSAC, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in, contract, label string
	}{
		{"", "", ""},
		{"native", xlmSAC, "native"},
		{usdc.String(), usdcSAC, usdc.String()},
		{validTestContract, validTestContract, ""},
		{"pool:00ff", "pool:00ff", ""},
		{"fiat:USD", "fiat:USD", ""},
	} {
		contract, label := sep41AssetScope(tc.in)
		if contract != tc.contract || label != tc.label {
			t.Errorf("sep41AssetScope(%q) = (%q, %q), want (%q, %q)", tc.in, contract, label, tc.contract, tc.label)
		}
	}
}

func ledgersOf(v AccountMovementsView) []uint32 {
	out := make([]uint32, len(v.Movements))
	for i, m := range v.Movements {
		out[i] = m.Ledger
	}
	return out
}
