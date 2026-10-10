package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
