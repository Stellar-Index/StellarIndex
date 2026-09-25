//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A flagged issuer's asset named by an unregistered SAC contract id has
// no issuer to look up; the scam gate recognises it through
// DirectoryScamFlaggedClassicAssets. This pins that query against real
// Timescale: only classic assets of scam-tagged G-accounts are listed,
// and the gate built over the real store withholds their SAC spelling.
func TestScamGate_SACIndexOverRealDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		flaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		cleanIssuer   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	flaggedContract := "C" + dirAddress("FLAGGED")[1:] // only G-accounts issue classic assets
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, []timescale.DirectoryEntry{
		dirEntry(flaggedIssuer, "Rio Issuer", "issuer", "Malicious"),
		dirEntry(cleanIssuer, "Clean Issuer", "issuer"),
		dirEntry(flaggedContract, "Flagged Contract", "unsafe"),
	})

	now := time.Now().UTC()
	for _, a := range []struct{ code, issuer string }{
		{"RIO", flaggedIssuer},
		{"RIOX", flaggedIssuer},
		{"USDC", cleanIssuer},
	} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO classic_assets
			    (asset_id, code, issuer_g_strkey, slug,
			     first_seen_at, first_seen_ledger, last_seen_at, last_seen_ledger,
			     observation_count)
			VALUES ($1, $2, $3, $4, $5, 1, $5, 100, 1)`,
			a.code+"-"+a.issuer, a.code, a.issuer, "sacidx-"+a.code, now); err != nil {
			t.Fatalf("insert classic asset %s: %v", a.code, err)
		}
	}

	listed, err := store.DirectoryScamFlaggedClassicAssets(ctx)
	if err != nil {
		t.Fatalf("DirectoryScamFlaggedClassicAssets: %v", err)
	}
	got := make([]string, 0, len(listed))
	for _, a := range listed {
		if a.Type != c.AssetClassic {
			t.Errorf("listed %v with type %q, want classic", a, a.Type)
		}
		got = append(got, a.Code+"-"+a.Issuer)
	}
	slices.Sort(got)
	want := []string{"RIO-" + flaggedIssuer, "RIOX-" + flaggedIssuer}
	if !slices.Equal(got, want) {
		t.Fatalf("flagged classic assets = %v, want %v (mixed-case tag must match; clean issuer excluded)", got, want)
	}

	gate := pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{})
	sacOf := func(code, issuer string) c.Asset {
		classic, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatalf("classic %s: %v", code, err)
		}
		cid, err := classic.SacContractID()
		if err != nil {
			t.Fatalf("derive SAC %s: %v", code, err)
		}
		sac, err := c.NewSorobanAsset(cid)
		if err != nil {
			t.Fatalf("soroban asset %s: %v", cid, err)
		}
		return sac
	}
	if !gate.WithheldPair(ctx, sacOf("RIOX", flaggedIssuer), c.NativeAsset(), "price_read") {
		t.Error("flagged issuer's unregistered SAC was served over the real store")
	}
	if gate.WithheldPair(ctx, sacOf("USDC", cleanIssuer), c.NativeAsset(), "price_read") {
		t.Error("clean issuer's SAC was withheld over the real store")
	}
}
