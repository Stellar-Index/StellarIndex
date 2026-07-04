package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
)

type fakeTokenSupply struct {
	gotContractID string
	supply        clickhouse.TokenSupply
	nativeCoins   int64
	nativeErr     error
	supplyErr     error
}

func (f *fakeTokenSupply) TokenSupply(_ context.Context, contractID string) (clickhouse.TokenSupply, error) {
	f.gotContractID = contractID
	if f.supplyErr != nil {
		return clickhouse.TokenSupply{}, f.supplyErr
	}
	return f.supply, nil
}

func (f *fakeTokenSupply) NativeTotalCoins(_ context.Context) (int64, uint32, error) {
	if f.nativeErr != nil {
		return 0, 0, f.nativeErr
	}
	return f.nativeCoins, 62944000, nil
}

func serveSupply(t *testing.T, reader TokenSupplyReader, sac map[string]string, assetID string) *httptest.ResponseRecorder {
	t.Helper()
	return serveSupplyWM(t, reader, sac, nil, assetID)
}

func serveSupplyWM(t *testing.T, reader TokenSupplyReader, sac map[string]string, wm LakeWatermarkReader, assetID string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{tokenSupply: reader, sacWrappers: sac, lakeWatermarkReader: wm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/supply", srv.handleAssetSupply)
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/supply", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeSupply(t *testing.T, body []byte) AssetSupply {
	t.Helper()
	data, _ := decodeSupplyEnvelope(t, body)
	return data
}

func decodeSupplyEnvelope(t *testing.T, body []byte) (AssetSupply, Flags) {
	t.Helper()
	var env struct {
		Data  AssetSupply `json:"data"`
		Flags Flags       `json:"flags"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	return env.Data, env.Flags
}

const supplyContractID = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"

func TestAssetSupply_NilReader503(t *testing.T) {
	rec := serveSupply(t, nil, nil, supplyContractID)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader: got %d, want 503", rec.Code)
	}
}

func TestAssetSupply_ContractIDDirect(t *testing.T) {
	f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
		ContractID: supplyContractID,
		Total:      big.NewInt(900), Mint: big.NewInt(1000), Burn: big.NewInt(80), Clawback: big.NewInt(20),
		FlowCount: 7,
	}}
	rec := serveSupply(t, f, nil, supplyContractID)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if f.gotContractID != supplyContractID {
		t.Errorf("resolved contract_id = %q, want %q", f.gotContractID, supplyContractID)
	}
	got := decodeSupply(t, rec.Body.Bytes())
	if got.TotalSupply != "900" || got.Source != "mint_burn_flows" || got.FlowCount != 7 {
		t.Errorf("unexpected body: %+v", got)
	}
	if got.MintTotal == nil || *got.MintTotal != "1000" {
		t.Errorf("mint_total = %v, want 1000", got.MintTotal)
	}
}

func TestAssetSupply_Native(t *testing.T) {
	f := &fakeTokenSupply{nativeCoins: 105443902087730434}
	for _, alias := range []string{"native", "XLM", "crypto:XLM"} {
		rec := serveSupply(t, f, nil, alias)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", alias, rec.Code)
		}
		got := decodeSupply(t, rec.Body.Bytes())
		if got.TotalSupply != "105443902087730434" || got.Source != "ledger_total_coins" {
			t.Errorf("%s: unexpected body %+v", alias, got)
		}
		if got.MintTotal != nil {
			t.Errorf("%s: native should not carry mint_total", alias)
		}
		// Native carries its exact source ledger as the watermark.
		if got.AsOfLedger != 62944000 {
			t.Errorf("%s: as_of_ledger = %d, want 62944000", alias, got.AsOfLedger)
		}
	}
}

// stubWatermark is a canned LakeWatermarkReader for the ADR-0041 D4 tests.
type stubWatermark struct {
	ledger   uint32
	closedAt time.Time
	err      error
	calls    int
}

func (s *stubWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	s.calls++
	return s.ledger, s.closedAt, s.err
}

// TestAssetSupply_WatermarkFreshAndStale pins ADR-0041 Decision 4 on the
// supply read: `as_of_ledger` carries the lake watermark, and `flags.stale`
// flips exactly when the watermark's close time trails now beyond
// lakeStaleThreshold (±30s margins keep the pin robust on slow CI).
func TestAssetSupply_WatermarkFreshAndStale(t *testing.T) {
	cases := []struct {
		name      string
		lag       time.Duration
		wantStale bool
	}{
		{"inside threshold → not stale", lakeStaleThreshold - 30*time.Second, false},
		{"beyond threshold → stale", lakeStaleThreshold + 30*time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wm := &stubWatermark{ledger: 63500000, closedAt: time.Now().Add(-tc.lag)}
			f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
				ContractID: supplyContractID,
				Total:      big.NewInt(9), Mint: big.NewInt(9), Burn: big.NewInt(0), Clawback: big.NewInt(0),
				FlowCount: 1,
			}}
			rec := serveSupplyWM(t, f, nil, wm, supplyContractID)
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
			if got.AsOfLedger != 63500000 {
				t.Errorf("as_of_ledger = %d, want 63500000", got.AsOfLedger)
			}
			if flags.Stale != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v (lag %s)", flags.Stale, tc.wantStale, tc.lag)
			}
		})
	}
}

// TestAssetSupply_NoWatermarkReaderOmitsField: nil watermark seam → the
// field is absent and stale never fires from the watermark (graceful,
// matching every other nil-reader degrade).
func TestAssetSupply_NoWatermarkReaderOmitsField(t *testing.T) {
	f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
		ContractID: supplyContractID,
		Total:      big.NewInt(9), Mint: big.NewInt(9), Burn: big.NewInt(0), Clawback: big.NewInt(0),
		FlowCount: 1,
	}}
	rec := serveSupply(t, f, nil, supplyContractID)
	got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
	if got.AsOfLedger != 0 || flags.Stale {
		t.Errorf("as_of_ledger = %d stale = %v, want 0/false without a watermark reader", got.AsOfLedger, flags.Stale)
	}
}

// TestAssetSupply_ClassicDerivedSAC pins the full-SAC-derivation fallback:
// with NO operator sac_wrappers entry, a classic asset resolves to its
// deterministically-derived Stellar-Asset-Contract instead of 404ing.
func TestAssetSupply_ClassicDerivedSAC(t *testing.T) {
	assetKey := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	parsed, err := canonical.ParseAsset(assetKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	derived, err := parsed.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
		ContractID: derived,
		Total:      big.NewInt(42), Mint: big.NewInt(42), Burn: big.NewInt(0), Clawback: big.NewInt(0),
		FlowCount: 1,
	}}
	rec := serveSupply(t, f, nil, assetKey) // no operator map at all
	if rec.Code != http.StatusOK {
		t.Fatalf("derived classic: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if f.gotContractID != derived {
		t.Errorf("resolved contract_id = %q, want derived SAC %q", f.gotContractID, derived)
	}
	got := decodeSupply(t, rec.Body.Bytes())
	if got.ContractID != derived || got.TotalSupply != "42" {
		t.Errorf("unexpected body: %+v", got)
	}
}

// TestAssetSupply_NoSACShape404 pins the residual 404: ids with no SAC at
// all (fiat:*) still can't resolve a contract.
func TestAssetSupply_NoSACShape404(t *testing.T) {
	f := &fakeTokenSupply{}
	rec := serveSupply(t, f, nil, "fiat:USD")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("fiat asset: got %d, want 404", rec.Code)
	}
	if f.gotContractID != "" {
		t.Errorf("reader should not be called for a contract-less asset shape")
	}
}

func TestAssetSupply_ClassicViaSACWrapper(t *testing.T) {
	assetKey := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sac := map[string]string{supplyContractID: assetKey}
	f := &fakeTokenSupply{supply: clickhouse.TokenSupply{
		ContractID: supplyContractID,
		Total:      big.NewInt(500), Mint: big.NewInt(500), Burn: big.NewInt(0), Clawback: big.NewInt(0),
		FlowCount: 3,
	}}
	rec := serveSupply(t, f, sac, assetKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if f.gotContractID != supplyContractID {
		t.Errorf("classic asset should resolve to its SAC %q, got %q", supplyContractID, f.gotContractID)
	}
	got := decodeSupply(t, rec.Body.Bytes())
	if got.ContractID != supplyContractID || got.TotalSupply != "500" {
		t.Errorf("unexpected body: %+v", got)
	}
}
