package v1_test

import (
	"context"
	"database/sql"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubSep1Cache implements v1.Sep1CachedReader in memory, mirroring Store.GetIssuerSep1Cached: payload for known issuers, (nil,
// nil) for a row with no payload yet, (nil, sql.ErrNoRows) for unknown issuers, or a configured error.
type stubSep1Cache struct {
	byIssuer map[string]*timescale.IssuerSep1Cached
	err      error
}

func (s *stubSep1Cache) GetIssuerSep1Cached(_ context.Context, gStrkey string) (*timescale.IssuerSep1Cached, error) {
	if s.err != nil {
		return nil, s.err
	}
	if payload, ok := s.byIssuer[gStrkey]; ok {
		return payload, nil
	}
	return nil, sql.ErrNoRows
}

func currencyAuthorityFixture(t *testing.T, cur timescale.IssuerSep1Currency) string {
	t.Helper()
	issuer := testUSDCIssuer
	domain := "issuer.example.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic",
				Code: "USDC", Issuer: &issuer, HomeDomain: &domain, Decimals: 7,
			},
		},
	}
	cur.Code, cur.Issuer = "USDC", testUSDCIssuer
	sep1 := &stubSep1Cache{byIssuer: map[string]*timescale.IssuerSep1Cached{
		testUSDCIssuer: {Currencies: []timescale.IssuerSep1Currency{cur}},
	}}
	return httpTestServer(t, v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})).URL
}
