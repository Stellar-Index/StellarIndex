//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The durable operator override for a FALSE-POSITIVE scam flag, end to
// end against a real Timescale.
//
// A scam-class tag in account_directory is not a label: it withholds the
// issuer's published price and market cap on /v1/price, /v1/vwap,
// /v1/twap, /v1/chart and /v1/price/tip (pricingguard.ScamGate), demotes
// its assets below every unflagged one in the /v1/assets ranking, and
// draws the explorer's flag pill. The tags come from a third-party
// upstream, so a wrong one has to be correctable — and the correction has
// to outlive the daily `directory-sync`.
//
// It did not. The sync's conflict arm rewrote `source` itself, so an
// operator-owned row was adopted into the upstream snapshot and
// overwritten on the next run; the price went dark again within 24
// hours. These cases pin the property a mock cannot: after a full
// upstream sync carrying the original `malicious` tag, the override row
// is still the one the gate reads.
const (
	dirUpstreamSource = "stellar-expert"
	dirOtherUpstream  = "second-upstream"
)

// dirAddress renders a G-strkey-shaped address that satisfies migration
// 0136's `^[GC][A-Z2-7]{55}$` CHECK.
func dirAddress(suffix string) string {
	return "G" + suffix + strings.Repeat("A", 55-len(suffix))
}

func dirEntry(address, name string, tags ...string) timescale.DirectoryEntry {
	return timescale.DirectoryEntry{
		Address: address,
		Name:    name,
		Domain:  "example.org",
		Tags:    tags,
	}
}

func mustReplaceDirectory(t *testing.T, ctx context.Context, store *timescale.Store, source string, entries []timescale.DirectoryEntry) {
	t.Helper()
	if _, _, err := store.ReplaceDirectory(ctx, source, entries); err != nil {
		t.Fatalf("ReplaceDirectory(%s): %v", source, err)
	}
}

func mustDirectoryEntry(t *testing.T, ctx context.Context, store *timescale.Store, address string) timescale.DirectoryEntry {
	t.Helper()
	e, found, err := store.DirectoryEntryByAddress(ctx, address)
	if err != nil {
		t.Fatalf("DirectoryEntryByAddress(%s): %v", address, err)
	}
	if !found {
		t.Fatalf("DirectoryEntryByAddress(%s): row missing", address)
	}
	return e
}

// scamWithheld asks a FRESH gate (the live gate caches a verdict for 60s,
// which is not what is under test here) whether the classic asset issued
// by `issuer` has its aggregated price withheld.
func scamWithheld(ctx context.Context, store *timescale.Store, issuer string) bool {
	gate := pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{})
	return gate.Withheld(ctx, c.Asset{Type: c.AssetClassic, Code: "RIO", Issuer: issuer}, "price_read")
}

func TestDirectoryOperatorOverride_SurvivesUpstreamSync(t *testing.T) {
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

	flagged := dirAddress("RIOISSUER")
	neighbour := dirAddress("NEIGHBOUR")
	upstream := []timescale.DirectoryEntry{
		dirEntry(flagged, "Rio Issuer", "unsafe"),
		dirEntry(neighbour, "Neighbour", "exchange"),
	}

	// 1. Upstream says the issuer is unsafe; the gate withholds.
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	if !scamWithheld(ctx, store, flagged) {
		t.Fatal("gate does not withhold a directory-flagged issuer — the fixture proves nothing")
	}

	// 2. The operator judges it a false positive and records a
	//    correction: same address, the scam tag dropped.
	if err := store.UpsertDirectoryOverride(ctx, dirEntry(flagged, "Rio Issuer (reviewed)", "memo-required")); err != nil {
		t.Fatalf("UpsertDirectoryOverride: %v", err)
	}
	if scamWithheld(ctx, store, flagged) {
		t.Fatal("gate still withholds immediately after the override — the correction never took effect")
	}

	// 3. The daily sync runs again with the SAME upstream snapshot,
	//    still carrying `unsafe`. This is where the correction used to
	//    die.
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)

	got := mustDirectoryEntry(t, ctx, store, flagged)
	if got.Source != timescale.DirectoryOperatorOverrideSource {
		t.Errorf("after sync, source = %q, want %q — the sync took the row back over",
			got.Source, timescale.DirectoryOperatorOverrideSource)
	}
	if pricingguard.IsDirectoryScamFlagged(got.Tags) {
		t.Errorf("after sync, tags = %v — upstream's scam tag overwrote the operator's correction", got.Tags)
	}
	if got.Name != "Rio Issuer (reviewed)" {
		t.Errorf("after sync, name = %q, want the operator's %q", got.Name, "Rio Issuer (reviewed)")
	}
	if scamWithheld(ctx, store, flagged) {
		t.Error("gate withholds again after a sync — the override is not durable, which is the whole defect")
	}

	// 4. The override also survives the PRUNE arm: upstream drops the
	//    address entirely.
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, []timescale.DirectoryEntry{
		dirEntry(neighbour, "Neighbour", "exchange"),
	})
	got = mustDirectoryEntry(t, ctx, store, flagged)
	if got.Source != timescale.DirectoryOperatorOverrideSource {
		t.Errorf("after prune, source = %q, want the override to survive", got.Source)
	}
	if scamWithheld(ctx, store, flagged) {
		t.Error("gate withholds after the prune arm ran")
	}

	// 5. And it is undoable: removing the override hands the address
	//    back to upstream, whose next sync restores the flag.
	removed, err := store.DeleteDirectoryOverride(ctx, flagged)
	if err != nil {
		t.Fatalf("DeleteDirectoryOverride: %v", err)
	}
	if !removed {
		t.Fatal("DeleteDirectoryOverride reported no row removed")
	}
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	if !scamWithheld(ctx, store, flagged) {
		t.Error("after the override was withdrawn, upstream's flag did not come back — the undo is not reversible")
	}
}

// TestDirectoryUpsert_SourcesDoNotStealEachOthersRows pins migration
// 0136's stated contract — "scoped by `source` so a future second
// directory source can coexist without the syncs deleting each other's
// rows" — which the unconditional `source = EXCLUDED.source` arm broke
// for every address two upstreams both carry. It also pins the
// no-regression half: a source still updates the rows it DOES own.
func TestDirectoryUpsert_SourcesDoNotStealEachOthersRows(t *testing.T) {
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

	shared := dirAddress("SHAREDADDR")
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, []timescale.DirectoryEntry{
		dirEntry(shared, "As first upstream sees it", "exchange"),
	})

	// A second upstream carrying the same address must not take it over.
	mustReplaceDirectory(t, ctx, store, dirOtherUpstream, []timescale.DirectoryEntry{
		dirEntry(shared, "As second upstream sees it", "malicious"),
	})
	got := mustDirectoryEntry(t, ctx, store, shared)
	if got.Source != dirUpstreamSource {
		t.Errorf("source = %q, want %q — the second sync stole a row it does not own", got.Source, dirUpstreamSource)
	}
	if got.Name != "As first upstream sees it" {
		t.Errorf("name = %q, want the owning source's %q", got.Name, "As first upstream sees it")
	}

	// The owning source still updates its own row — the guard must not
	// freeze the sync it is there to scope.
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, []timescale.DirectoryEntry{
		dirEntry(shared, "Renamed by its owner", "exchange", "kyc"),
	})
	got = mustDirectoryEntry(t, ctx, store, shared)
	if got.Name != "Renamed by its owner" {
		t.Errorf("name = %q, want the owning source's update to land", got.Name)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v, want the owning source's 2-tag update to land", got.Tags)
	}
}
