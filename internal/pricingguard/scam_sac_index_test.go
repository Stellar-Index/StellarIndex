package pricingguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A flagged issuer's asset named by its SAC contract id carries no issuer
// of its own, and with no [supply].sac_wrappers entry canonicalisation
// cannot invert it — so the gate served the flagged market's price under
// that C-address. These tests pin the flagged-issuer→SAC index that
// closes it.

// sacIndexDirectory flags the listed issuers and lists the given classic
// assets as their scam-flagged issuances, the way *timescale.Store does.
type sacIndexDirectory struct {
	flagged  map[string]bool
	assets   []canonical.Asset
	listErr  error
	listings int
}

func (d *sacIndexDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

func (d *sacIndexDirectory) DirectoryScamFlaggedClassicAssets(context.Context) ([]canonical.Asset, error) {
	d.listings++
	if d.listErr != nil {
		return nil, d.listErr
	}
	return d.assets, nil
}

// unregisteredSAC returns the SAC contract spelling of a classic asset,
// asserting canonicalisation leaves it as a bare contract (no alias).
func unregisteredSAC(t *testing.T, classic canonical.Asset) canonical.Asset {
	t.Helper()
	cid, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(cid)
	if err != nil {
		t.Fatalf("build SAC asset: %v", err)
	}
	if got := canonical.CanonicalAsset(sac); got.Type != canonical.AssetSoroban {
		t.Fatalf("SAC canonicalised to %v — an alias is registered, so this would not exercise the unregistered path", got)
	}
	return sac
}

func TestScamGate_UnregisteredSACOfFlaggedIssuerIsWithheld(t *testing.T) {
	flagged, clean, native := pairLegAssets(t)
	ctx := context.Background()
	dir := &sacIndexDirectory{
		flagged: map[string]bool{pairLegFlaggedIssuer: true},
		assets:  []canonical.Asset{flagged},
	}
	g := NewScamGate(dir, ScamGateOptions{})
	flaggedSAC := unregisteredSAC(t, flagged)

	if !g.WithheldPair(ctx, flaggedSAC, native, "price_read") {
		t.Fatal("<flagged SAC>/native was served — a flagged issuer's price is withheld " +
			"under CODE-ISSUER but published under its SAC contract id")
	}
	if !g.WithheldPair(ctx, native, flaggedSAC, "vwap") {
		t.Fatal("native/<flagged SAC> was served — the quote leg must be resolved the same way")
	}
	if g.WithheldPair(ctx, unregisteredSAC(t, clean), native, "price_read") {
		t.Error("an unflagged issuer's SAC was withheld — the index must match only flagged issuances")
	}
	if dir.listings != 1 {
		t.Errorf("flagged-asset listing ran %d times, want 1 — the index is cached for scamCacheTTL", dir.listings)
	}
}

func TestScamGate_SACIndexRefreshesAfterTTL(t *testing.T) {
	flagged, _, native := pairLegAssets(t)
	ctx := context.Background()
	dir := &sacIndexDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}, assets: []canonical.Asset{flagged}}
	g := NewScamGate(dir, ScamGateOptions{})
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }
	flaggedSAC := unregisteredSAC(t, flagged)

	if !g.WithheldPair(ctx, flaggedSAC, native, "price_read") {
		t.Fatal("fixture: flagged SAC not withheld")
	}
	dir.assets = nil // operator cleared the flag
	now = now.Add(scamCacheTTL + time.Second)
	if g.WithheldPair(ctx, flaggedSAC, native, "price_read") {
		t.Error("SAC still withheld past scamCacheTTL after the flag was cleared — the override must take effect within the TTL")
	}
}

func TestScamGate_SACIndexFailsOpenOnListingError(t *testing.T) {
	flagged, _, native := pairLegAssets(t)
	ctx := context.Background()
	dir := &sacIndexDirectory{listErr: errors.New("db down")}
	g := NewScamGate(dir, ScamGateOptions{})
	flaggedSAC := unregisteredSAC(t, flagged)

	if g.WithheldPair(ctx, flaggedSAC, native, "price_read") {
		t.Fatal("a listing error withheld a contract leg — the gate is fail-open, like a directory error")
	}
	dir.listErr = nil
	dir.assets = []canonical.Asset{flagged}
	if !g.WithheldPair(ctx, flaggedSAC, native, "price_read") {
		t.Error("a failed listing was cached — the next request must re-ask")
	}
}
