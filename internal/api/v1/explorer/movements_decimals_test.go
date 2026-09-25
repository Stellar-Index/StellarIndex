package explorer

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAccountMovements_ServesPerAssetDecimals: the recent tail spans every
// SEP-41 contract, so each row must carry its own asset's scale. An
// 18-decimal token's 1.0 is 10^18 base units; rendered at a feed-wide 7 it
// reads as 100,000,000,000. A failed decimals read must omit the scale
// rather than publish a guessed 7.
func TestAccountMovements_ServesPerAssetDecimals(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	const unreadableToken = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	floor := timescale.SEP41MovementsFloorLedger
	when := time.Unix(1_700_000_000, 0).UTC()
	oneUnit18, _ := new(big.Int).SetString("1000000000000000000", 10)
	tail := &scopedSEP41Tail{rows: []timescale.SEP41TransferRow{
		{ContractID: validTestContract, Ledger: floor + 3, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: unreadableToken, Amount: oneUnit18},
		{ContractID: usdcSAC, Ledger: floor + 2, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(10_000_000)},
		{ContractID: unreadableToken, Ledger: floor + 1, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(5)},
	}}
	reader := &sacNamingReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}}, sac: usdcSAC, name: "USDC:" + validTestAccount}

	var captured AccountMovementsView
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.TokenDecimals = func(_ context.Context, contractID string) (int, bool) {
		if contractID == validTestContract {
			return 18, true
		}
		return 0, false
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		captured, _ = v.(AccountMovementsView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)

	want := map[string]*int{validTestContract: intPtr(18), usdc.String(): intPtr(7), unreadableToken: nil}
	if len(captured.Movements) != len(want) {
		t.Fatalf("got %d movements, want %d: %+v", len(captured.Movements), len(want), captured.Movements)
	}
	for _, m := range captured.Movements {
		w, ok := want[m.Asset]
		if !ok {
			t.Fatalf("unexpected asset %q", m.Asset)
		}
		switch {
		case w == nil && m.Decimals != nil:
			t.Errorf("%s: decimals = %d, want omitted (read failed)", m.Asset, *m.Decimals)
		case w != nil && (m.Decimals == nil || *m.Decimals != *w):
			t.Errorf("%s: decimals = %v, want %d", m.Asset, m.Decimals, *w)
		}
	}
}

func intPtr(v int) *int { return &v }
