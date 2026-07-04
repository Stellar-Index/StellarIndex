package v1_test

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
)

// decStub is a canned v1.TokenDecimalsReader recording which contract (if
// any) the handler consulted.
type decStub struct {
	d           uint32
	found       bool
	gotContract string
}

func (s *decStub) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	s.gotContract = contractID
	return s.d, s.found, nil
}

const decTestContract = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"

func getAssetDetail(t *testing.T, base, assetID string) (v1.AssetDetail, int) {
	t.Helper()
	resp := mustGet(t, base+"/v1/assets/"+assetID)
	if resp.StatusCode != http.StatusOK {
		return v1.AssetDetail{}, resp.StatusCode
	}
	var body struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data, resp.StatusCode
}

// TestAssetDetail_SorobanDecimalsOverlay: a Soroban token whose instance
// METADATA is captured serves its REAL on-chain decimals, not the 7 default.
func TestAssetDetail_SorobanDecimalsOverlay(t *testing.T) {
	stub := &decStub{d: 18, found: true}
	srv := v1.New(v1.Options{TokenDecimals: stub})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, decTestContract)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 18 {
		t.Errorf("decimals = %d, want 18 (overlaid from instance metadata)", detail.Decimals)
	}
	if stub.gotContract != decTestContract {
		t.Errorf("consulted contract = %q, want %q", stub.gotContract, decTestContract)
	}
}

// TestAssetDetail_SorobanDecimalsNotDerivable: no captured metadata → the
// documented default 7 stays.
func TestAssetDetail_SorobanDecimalsNotDerivable(t *testing.T) {
	srv := v1.New(v1.Options{TokenDecimals: &decStub{found: false}})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, decTestContract)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 7 {
		t.Errorf("decimals = %d, want default 7 when metadata is not derivable", detail.Decimals)
	}
}

// TestAssetDetail_ClassicNeverConsultsDecimals: classic assets ARE 7 by
// protocol — the reader must not even be consulted (a wrong overlay here
// would corrupt every unit computation downstream).
func TestAssetDetail_ClassicNeverConsultsDecimals(t *testing.T) {
	stub := &decStub{d: 18, found: true} // would lie if consulted
	srv := v1.New(v1.Options{TokenDecimals: stub})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, "USDC-"+testG)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Decimals != 7 {
		t.Errorf("classic decimals = %d, want 7 (protocol-fixed)", detail.Decimals)
	}
	if stub.gotContract != "" {
		t.Errorf("classic asset consulted the decimals reader (contract %q)", stub.gotContract)
	}
}
