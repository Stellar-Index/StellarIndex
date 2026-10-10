package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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

// supplySnapAt is lockstepSupply's 1000-token (9 dp) circulating figure,
// observed at `at` and ledger 4242.
func supplySnapAt(at time.Time) *stubSupplyLooker {
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	return &stubSupplyLooker{hit: true, snap: supply.Supply{
		CirculatingSupply: circ, ObservedAt: at, LedgerSequence: 4242,
	}}
}

// /v1/assets/{id} carries the supply's vintage, and an observation older than
// the listing's 6 h precise-arm bound is not multiplied by today's price: the
// supply serves with its as-of, the cap is withheld, the body is stale.
func TestAssetDetail_SupplyAgeBoundsTheCap(t *testing.T) {
	cases := []struct {
		name      string
		age       time.Duration
		wantCap   bool
		wantStale bool
	}{
		{"fresh observation", time.Hour, true, false},
		{"observer stopped a day ago", 25 * time.Hour, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(-tc.age).Truncate(time.Second)
			srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: supplySnapAt(at)})
			body := lockstepGet(t, srv)

			if want := `"supply_as_of":"` + at.Format(time.RFC3339); !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
			if !strings.Contains(body, `"supply_as_of_ledger":4242`) {
				t.Errorf("body missing supply_as_of_ledger 4242: %s", body)
			}
			if got := strings.Contains(body, `"market_cap_usd":"`); got != tc.wantCap {
				t.Errorf("market_cap_usd present = %v, want %v: %s", got, tc.wantCap, body)
			}
			if got := strings.Contains(body, `"stale":true`); got != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v: %s", got, tc.wantStale, body)
			}
		})
	}
}

const (
	aliasUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aliasUSDCClassic = "USDC-" + aliasUSDCIssuer
	aliasUSDCSAC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// TestAssetDetail_SACFormResolvesCanonical is Part A's detail-page half:
// GET /v1/assets/{USDC-SAC} must serve the CANONICAL classic USDC detail
// (same asset_id as /v1/assets/{USDC-classic}), resolved from the config
// AliasRegistry with NO lake round-trip (no explorer wired here). This is
// what makes a SAC-form link land on the asset's real page instead of a
// second, thin "SAC" identity.
//
// RED without the handleAssetGet canonicalization: with no explorer to run
// resolveSACToClassic, the SAC C-address echoes back as its own asset_id.
func TestAssetDetail_SACFormResolvesCanonical(t *testing.T) {
	// Process-global registry — must not run parallel; reset on cleanup.
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{aliasUSDCSAC: "USDC:" + aliasUSDCIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	srv := v1.New(v1.Options{})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, aliasUSDCSAC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if detail.AssetID != aliasUSDCClassic {
		t.Errorf("SAC-form detail asset_id = %q, want the canonical classic %q — a SAC link must resolve to the real asset page",
			detail.AssetID, aliasUSDCClassic)
	}
}

// TestAssetDetail_WithheldPrice_ServesNoPriceHistory — same invariant on
// the asset DETAIL payload, where the leak was widest: r1 served
// price_usd null beside 24 hourly and 7 daily priced points for every
// scam-flagged asset.
func TestAssetDetail_WithheldPrice_ServesNoPriceHistory(t *testing.T) {
	const scamID = "JFKBANK2-" + scamAUDIssuer
	hist24 := []timescale.AssetPricePoint{
		{T: "2026-08-29T10:00:00Z", P: sptr("0.42")},
		{T: "2026-08-29T11:00:00Z", P: sptr("0.43")},
	}
	hist7d := []timescale.AssetPricePoint{
		{T: "2026-08-28T00:00:00Z", P: sptr("0.44")},
		{T: "2026-08-29T00:00:00Z", P: sptr("0.42")},
	}
	issuer := scamAUDIssuer
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug: "jfkbank2", AssetID: scamID, Code: "JFKBANK2",
			IssuerGStrkey: scamAUDIssuer, PriceUSD: sptr("0.42"),
		},
		hist24: hist24,
		hist7d: hist7d,
	}
	srv := v1.New(v1.Options{
		Assets: &stubAssetReader{byID: map[string]v1.AssetDetail{
			scamID: {AssetID: scamID, Type: "classic", Code: "JFKBANK2", Issuer: &issuer},
		}},
		AssetsReader: assetsReader,
		Directory:    scamAUDDirectoryStub(),
	})
	ts := httpTestServer(t, srv)

	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/assets/"+scamID), &env)

	if env.Data.PriceUSD != nil {
		t.Fatalf("fixture broken — price_usd = %q, want null (scam-flagged issuer)", *env.Data.PriceUSD)
	}
	if len(env.Data.PriceHistory24h) != 0 {
		t.Errorf("price_history_24h = %+v, want none — the last bucket IS the withheld price", env.Data.PriceHistory24h)
	}
	if len(env.Data.PriceHistory7d) != 0 {
		t.Errorf("price_history_7d = %+v, want none — the last bucket IS the withheld price", env.Data.PriceHistory7d)
	}
}
