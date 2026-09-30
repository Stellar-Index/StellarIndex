//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSep1PayloadOutlivedDomain pins the one predicate every SEP-1 identity
// read shares: a held payload stops vouching for an issuer once it is past
// Sep1AttestationMaxAge, or of unrecorded age, AND its domain is failing now.
// Neither condition alone may downgrade it.
func TestSep1PayloadOutlivedDomain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	old := time.Now().Add(-timescale.Sep1AttestationMaxAge - time.Hour)
	fresh := time.Now().Add(-time.Hour)
	cases := []struct {
		name      string
		g         string
		fetchedAt *time.Time
		failures  int
		outlived  bool
	}{
		{"old payload, domain failing", "GA2P3HKTQFBZBWOWPLESMV5AEHJLWJKNAYV5HLXOPBRWUEHFR64FQTKN", &old, 5, true},
		{"unrecorded age, domain failing", "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW", nil, 1, true},
		{"old payload, domain not failing", "GA2PZMWITS45LSQF7KWN7SH2YREIUHLC4SNNIYX5D2LTNEML74CANJMO", &old, 0, false},
		{"fresh payload, domain failing", "GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62", &fresh, 3, false},
	}

	for i, c := range cases {
		seedIssuers(t, ctx, store, []seedIssuer{{g: c.g, homeDomain: "anchor.example"}})
		seedClassicAssets(t, ctx, store, []seedAsset{{
			assetID: "USDX-" + c.g, code: "USDX", issuer: c.g, slug: "usdx-" + c.g[:8], obs: int64(100 - i),
		}})
		payload := `{"OrgName":"Anchor","OrgVerified":true,
			"Currencies":[{"Code":"USDX","Issuer":"` + c.g + `","Image":"https://anchor.example/usdx.png"}]}`
		if _, err := store.DB().ExecContext(ctx, `
            UPDATE issuers
               SET sep1_payload = $2::jsonb,
                   sep1_payload_fetched_at = $3,
                   sep1_consecutive_failures = $4
             WHERE g_strkey = $1`, c.g, payload, c.fetchedAt, c.failures); err != nil {
			t.Fatalf("%s: seed payload: %v", c.name, err)
		}
	}

	images, err := store.AllSep1Images(ctx)
	if err != nil {
		t.Fatalf("AllSep1Images: %v", err)
	}
	imaged := map[string]bool{}
	for _, img := range images {
		imaged[img.Issuer] = true
	}
	listed, err := store.ListIssuers(ctx, 10)
	if err != nil {
		t.Fatalf("ListIssuers: %v", err)
	}
	listVerified := map[string]bool{}
	for _, r := range listed {
		listVerified[r.GStrkey] = r.OrgVerified
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sep, err := store.GetIssuerSep1Cached(ctx, c.g)
			if err != nil || sep == nil {
				t.Fatalf("GetIssuerSep1Cached = (%v, %v)", sep, err)
			}
			if sep.OutlivedDomain != c.outlived {
				t.Errorf("GetIssuerSep1Cached OutlivedDomain = %v, want %v", sep.OutlivedDomain, c.outlived)
			}
			row, err := store.GetIssuer(ctx, c.g)
			if err != nil {
				t.Fatalf("GetIssuer: %v", err)
			}
			if row.OrgVerified == c.outlived {
				t.Errorf("GetIssuer org_verified = %v, want %v", row.OrgVerified, !c.outlived)
			}
			if listVerified[c.g] == c.outlived {
				t.Errorf("ListIssuers org_verified = %v, want %v", listVerified[c.g], !c.outlived)
			}
			if imaged[c.g] == c.outlived {
				t.Errorf("AllSep1Images served image = %v, want %v", imaged[c.g], !c.outlived)
			}
		})
	}
}
