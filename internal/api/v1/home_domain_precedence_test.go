package v1_test

// Home-domain identity precedence (2026-08-06). Founding case: the
// ex-apay ETH issuer GBFXOHVAS… — its on-chain home_domain has said
// ultracapital.xyz since Ultra Stellar acquired apay.io's wrapped
// assets, but /v1/assets/ETH-GBFXOHVAS… rendered "apay.io" (and
// SEP-1-"verified" against it) because the hand-curated knownIssuers
// map outranked the account's own signed field. Precedence is now:
// storage row → live on-chain account state → curated map.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetGet_OnChainHomeDomainBeatsCuratedMap — when the storage
// row has no home_domain, the backfill must consult the live
// on-chain account state BEFORE the curated knownIssuers map.
func TestAssetGet_OnChainHomeDomainBeatsCuratedMap(t *testing.T) {
	issuer := testUSDCIssuer // present in knownIssuers as circle.com
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:  "USDC-" + testUSDCIssuer,
				Type:     "classic",
				Code:     "USDC",
				Issuer:   &issuer,
				Decimals: 7,
			},
		},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			HomeDomain: "live-onchain.example",
		},
	}
	srv := v1.New(v1.Options{Assets: reader, Explorer: explorer})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "live-onchain.example" {
		t.Errorf("HomeDomain = %v, want live-onchain.example (on-chain must beat the curated map)",
			env.Data.HomeDomain)
	}
}

// TestAssetGet_CuratedMapStillFillsWhenChainSilent — no explorer
// reader wired (or the account is unobserved) keeps the curated map
// as the working fallback; this is the pre-existing R-016 behavior
// the precedence change must not regress.
func TestAssetGet_CuratedMapStillFillsWhenChainSilent(t *testing.T) {
	issuer := testUSDCIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:  "USDC-" + testUSDCIssuer,
				Type:     "classic",
				Code:     "USDC",
				Issuer:   &issuer,
				Decimals: 7,
			},
		},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: false},
	}
	srv := v1.New(v1.Options{Assets: reader, Explorer: explorer})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "circle.com" {
		t.Errorf("HomeDomain = %v, want circle.com (curated fallback when chain has no observation)",
			env.Data.HomeDomain)
	}
}

// TestIssuerGet_OnChainBeatsCuratedMap — same precedence on the
// issuer card: an empty DB row must fill from on-chain account
// state, not from a (possibly stale) curated entry.
func TestIssuerGet_OnChainBeatsCuratedMap(t *testing.T) {
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{GStrkey: testUSDCIssuer},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			HomeDomain: "live-onchain.example",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+testUSDCIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.HomeDomain != "live-onchain.example" {
		t.Errorf("HomeDomain = %q, want live-onchain.example (on-chain must beat the curated map)",
			env.Data.HomeDomain)
	}
	// The curated map still supplies what chain can't: the org name.
	if env.Data.OrgName != "Circle" {
		t.Errorf("OrgName = %q, want Circle (curated fills fields chain doesn't carry)",
			env.Data.OrgName)
	}
}

// TestIssuerGet_ScamSuppressionSurvivesAccountState — S-010: for a
// flagged, unverified issuer, the self-declared identity IS the
// impersonation. Before the 2026-08-06 reorder the suppression ran
// ahead of the account-state enrich, which then refilled the cleared
// home_domain straight from the scammer's own on-chain field.
func TestIssuerGet_ScamSuppressionSurvivesAccountState(t *testing.T) {
	const scam = "GA2XZLXNLAL26VBCA2OESAIMXTRH5GXKLHYZMDGNCR2SYS5QZWWNBLCK"
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{GStrkey: scam},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			Flags:      0x2, // auth_revocable — objective state, must survive
			HomeDomain: "lobstr.co",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+scam)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.ScamReason == "" {
		t.Fatalf("ScamReason empty — test issuer %s no longer in the scam list; pick another", scam)
	}
	if env.Data.HomeDomain != "" {
		t.Errorf("HomeDomain = %q, want \"\" (suppressed identity must not be refilled from on-chain state)",
			env.Data.HomeDomain)
	}
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("AuthRevocable = %v, want true (auth flags are objective state, not identity)",
			env.Data.AuthRevocable)
	}
}
