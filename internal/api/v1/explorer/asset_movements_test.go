package explorer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const testUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

type assetArmReader struct {
	*movementsArmReader
	backfilledThru uint32
	markerErr      error
	sacNames       map[string]string
	rows           []clickhouse.AssetMovementRow

	gotAsset   string
	gotCeiling uint32
}

func (r *assetArmReader) AssetMovements(_ context.Context, asset string, limit int, _ clickhouse.AccountMovementCursor, maxLedger uint32) ([]clickhouse.AssetMovementRow, error) {
	r.gotAsset, r.gotCeiling = asset, maxLedger
	var out []clickhouse.AssetMovementRow
	for _, row := range r.rows {
		if row.Ledger <= maxLedger && len(out) < limit {
			out = append(out, row)
		}
	}
	return out, nil
}

func (r *assetArmReader) AssetMovementsBackfilledThru(context.Context) (uint32, error) {
	return r.backfilledThru, r.markerErr
}

func (r *assetArmReader) SACClassicAssetName(_ context.Context, contractID string) (string, bool, error) {
	name, ok := r.sacNames[contractID]
	return name, ok, nil
}

func newAssetArmReader(wm uint32) *assetArmReader {
	return &assetArmReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}}
}

func callAssetMovements(t *testing.T, reader *assetArmReader, assetID string, limit int) (int, AssetMovementsView) {
	t.Helper()
	var view AssetMovementsView
	h := newProbeHandler(reader, nil)
	h.ParseLimit = func(http.ResponseWriter, *http.Request, int, int) (int, bool) { return limit, true }
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view = v.(AssetMovementsView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/assets/x/movements", nil)
	r.SetPathValue("asset_id", assetID)
	rec := httptest.NewRecorder()
	h.AssetMovements(rec, r)
	return rec.Code, view
}

// Every spelling of one asset must reach the store as the single id the
// movement tables hold; a SAC address folds to the asset it wraps.
func TestAssetMovements_FoldsAliasesToStoredID(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	xlmSAC, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"native":                 "native",
		"XLM":                    "native",
		"crypto:XLM":             "native",
		xlmSAC:                   "native",
		"USDC:" + testUSDCIssuer: usdc.String(),
		"USDC-" + testUSDCIssuer: usdc.String(),
		usdcSAC:                  usdc.String(),
		validTestContract:        validTestContract,
	}
	for in, want := range cases {
		reader := newAssetArmReader(0)
		reader.sacNames = map[string]string{usdcSAC: "USDC:" + testUSDCIssuer, xlmSAC: "native"}
		code, view := callAssetMovements(t, reader, in, 25)
		if code != http.StatusOK || reader.gotAsset != want || view.Asset != want {
			t.Errorf("%s: code=%d store asset=%q view asset=%q, want %q", in, code, reader.gotAsset, view.Asset, want)
		}
	}
}

func TestAssetMovements_RejectsOffChainAndMalformedIDs(t *testing.T) {
	for _, in := range []string{"fiat:USD", "crypto:BTC", "not an asset", ""} {
		if code, _ := callAssetMovements(t, newAssetArmReader(0), in, 25); code != http.StatusBadRequest {
			t.Errorf("%q: code=%d, want 400", in, code)
		}
	}
}

// The feed is ceilinged at the cap67 watermark, flagged a lower bound until
// the backfill marker exists, and carries amounts above 2^64 exactly.
func TestAssetMovements_CeilingLowerBoundAndExactAmount(t *testing.T) {
	wm := timescale.MovementsFloor() + 500
	big100 := new(big.Int).Lsh(big.NewInt(1), 100)
	when := time.Unix(1_700_000_000, 0).UTC()
	reader := newAssetArmReader(wm)
	reader.rows = []clickhouse.AssetMovementRow{
		{Ledger: wm + 1, TxHash: "above", MovementKind: "transfer", Amount: big.NewInt(1), LedgerCloseTime: when},
		{
			Ledger: wm, TxHash: "aa", OpIndex: 1, LegIndex: 2, MovementKind: "transfer", Provenance: "cap67_derived",
			From: "GFROM", To: "GTO", Amount: big100, LedgerCloseTime: when,
		},
		{Ledger: wm - 1, TxHash: "bb", MovementKind: "transfer", Amount: big.NewInt(3), LedgerCloseTime: when},
	}

	code, view := callAssetMovements(t, reader, "native", 1)
	if code != http.StatusOK || reader.gotCeiling != wm || view.ThroughLedger != wm {
		t.Fatalf("code=%d ceiling=%d through=%d, want 200 and %d", code, reader.gotCeiling, view.ThroughLedger, wm)
	}
	if len(view.Movements) != 1 || view.Movements[0].TxHash != "aa" {
		t.Fatalf("movements=%+v, want only the row at the watermark", view.Movements)
	}
	m := view.Movements[0]
	if m.Amount != big100.String() || m.From != "GFROM" || m.To != "GTO" || m.Decimals == nil || *m.Decimals != 7 {
		t.Fatalf("entry=%+v, want exact amount %s, GFROM->GTO, 7 decimals", m, big100)
	}
	if want := fmt.Sprintf("%d.aa.1.2", wm); view.NextCursor != want {
		t.Fatalf("next_cursor=%q, want %q", view.NextCursor, want)
	}
	if !view.LowerBound || !strings.Contains(view.CoverageNote, "lower bound") {
		t.Fatalf("no backfill marker: lower_bound=%v note=%q, want a lower bound", view.LowerBound, view.CoverageNote)
	}

	reader.backfilledThru = wm
	if _, view = callAssetMovements(t, reader, "native", 1); view.LowerBound || strings.Contains(view.CoverageNote, "lower bound") {
		t.Fatalf("marker set: lower_bound=%v note=%q, want complete history", view.LowerBound, view.CoverageNote)
	}

	reader.markerErr = errors.New("marker read failed")
	if _, view = callAssetMovements(t, reader, "native", 1); !view.LowerBound {
		t.Fatal("unreadable marker must serve as a lower bound")
	}
}

// A watermark read error fails closed to the pre-P23 boundary, as on the
// account feed: never serve archive rows the derive may not have finished.
func TestAssetMovements_WatermarkErrorCeilingsAtP23(t *testing.T) {
	reader := newAssetArmReader(timescale.MovementsFloor() + 500)
	reader.wmErr = errors.New("connection reset")
	if code, view := callAssetMovements(t, reader, "native", 25); code != http.StatusOK ||
		reader.gotCeiling != timescale.MovementsFloor()-1 || !strings.Contains(view.CoverageNote, "no post-P23") {
		t.Fatalf("code=%d ceiling=%d note=%q, want P23-1 and the no-archive note", code, reader.gotCeiling, view.CoverageNote)
	}
}
