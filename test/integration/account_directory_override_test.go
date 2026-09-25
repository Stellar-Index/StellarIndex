//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
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
	dirOverrideReason = "issuer verified via its stellar.toml; upstream tag is a false positive"
	dirOverrideActor  = "ops-oncall"
)

// dirOverrideReasonOf reads the row's override_reason ("" for NULL).
func dirOverrideReasonOf(t *testing.T, ctx context.Context, store *timescale.Store, address string) string {
	t.Helper()
	var reason sql.NullString
	if err := store.DB().QueryRowContext(ctx,
		`SELECT override_reason FROM account_directory WHERE address = $1`, address).Scan(&reason); err != nil {
		t.Fatalf("read override_reason %s: %v", address, err)
	}
	return reason.String
}

// dirOverrideByOf reads the row's override_by ("" for NULL).
func dirOverrideByOf(t *testing.T, ctx context.Context, store *timescale.Store, address string) string {
	t.Helper()
	var by sql.NullString
	if err := store.DB().QueryRowContext(ctx,
		`SELECT override_by FROM account_directory WHERE address = $1`, address).Scan(&by); err != nil {
		t.Fatalf("read override_by %s: %v", address, err)
	}
	return by.String
}

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

// rwaRecognised reports whether the RWA recognition funnel's issuer arm
// (recognition tag, no scam tag) admits `issuer`.
func rwaRecognised(t *testing.T, ctx context.Context, store *timescale.Store, issuer string) bool {
	t.Helper()
	rows, err := store.DirectoryRecognisedIssuersWithoutAsset(ctx, rwa.RecognitionTags(), 0)
	if err != nil {
		t.Fatalf("DirectoryRecognisedIssuersWithoutAsset: %v", err)
	}
	for _, r := range rows {
		if r.Address == issuer {
			return true
		}
	}
	return false
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
		dirEntry(flagged, "Rio Issuer", "issuer", "unsafe", "memo-required"),
		dirEntry(neighbour, "Neighbour", "exchange"),
	}

	// 1. Upstream says the issuer is unsafe; the gate withholds.
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	if !scamWithheld(ctx, store, flagged) {
		t.Fatal("gate does not withhold a directory-flagged issuer — the fixture proves nothing")
	}
	if rwaRecognised(t, ctx, store, flagged) {
		t.Fatal("a scam-flagged issuer is already RWA-recognised — the fixture proves nothing")
	}

	// A row with no scam tag is refused, and stays upstream-owned.
	if _, _, _, err := store.ClearDirectoryScamFlag(ctx, neighbour, dirOverrideActor, dirOverrideReason); !errors.Is(err, timescale.ErrDirectoryNotScamFlagged) {
		t.Fatalf("ClearDirectoryScamFlag(unflagged) err = %v, want ErrDirectoryNotScamFlagged", err)
	}
	if got := mustDirectoryEntry(t, ctx, store, neighbour); got.Source != dirUpstreamSource {
		t.Fatalf("refused clear still took the row over: source = %q", got.Source)
	}

	// 2. The operator judges it a false positive and records a
	//    correction: same address, only the scam tag dropped.
	if _, _, found, err := store.ClearDirectoryScamFlag(ctx, flagged, dirOverrideActor, dirOverrideReason); err != nil || !found {
		t.Fatalf("ClearDirectoryScamFlag: found=%v err=%v", found, err)
	}
	if got := dirOverrideReasonOf(t, ctx, store, flagged); got != dirOverrideReason {
		t.Errorf("override_reason after the takeover = %q, want %q", got, dirOverrideReason)
	}
	if got := dirOverrideByOf(t, ctx, store, flagged); got != dirOverrideActor {
		t.Errorf("override_by after the takeover = %q, want %q — the override must record who lifted the flag", got, dirOverrideActor)
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
	if want := []string{"issuer", "memo-required"}; !slices.Equal(got.Tags, want) {
		t.Errorf("after sync, tags = %q, want %q — the override must drop only the scam tag", got.Tags, want)
	}
	if got.Name != "Rio Issuer" || got.Domain != "example.org" {
		t.Errorf("after sync, name/domain = %q/%q, want the upstream's kept", got.Name, got.Domain)
	}
	if got := dirOverrideReasonOf(t, ctx, store, flagged); got != dirOverrideReason {
		t.Errorf("after sync, override_reason = %q, want %q kept", got, dirOverrideReason)
	}
	if got := dirOverrideByOf(t, ctx, store, flagged); got != dirOverrideActor {
		t.Errorf("after sync, override_by = %q, want %q kept", got, dirOverrideActor)
	}
	if scamWithheld(ctx, store, flagged) {
		t.Error("gate withholds again after a sync — the override is not durable, which is the whole defect")
	}
	if !rwaRecognised(t, ctx, store, flagged) {
		t.Error("cleared issuer is missing from the RWA recognition funnel — the override dropped its recognition tag")
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

	// A second upstream carrying the same address must not take it over,
	// and must SAY it skipped it rather than just upserting one fewer.
	secondOnly := dirAddress("SECONDONLY")
	res, err := store.ReplaceDirectoryWithin(ctx, dirOtherUpstream, []timescale.DirectoryEntry{
		dirEntry(shared, "As second upstream sees it", "malicious"),
		dirEntry(secondOnly, "Second upstream's own", "exchange"),
	}, timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("second upstream sync: %v", err)
	}
	if res.Upserted != 1 || res.Shadowed != 1 {
		t.Errorf("second upstream sync: upserted=%d shadowed=%d, want 1/1 — a shared address it cannot own must be reported, not silently dropped",
			res.Upserted, res.Shadowed)
	}
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

	// The second upstream drops the shared address: its prune is scoped
	// to its own rows, so the first source's row must survive it.
	res, err = store.ReplaceDirectoryWithin(ctx, dirOtherUpstream, []timescale.DirectoryEntry{
		dirEntry(secondOnly, "Second upstream's own", "exchange"),
	}, timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("second upstream re-sync: %v", err)
	}
	if res.Pruned != 0 || res.Shadowed != 0 {
		t.Errorf("second upstream re-sync: pruned=%d shadowed=%d, want 0/0", res.Pruned, res.Shadowed)
	}
	if got = mustDirectoryEntry(t, ctx, store, shared); got.Source != dirUpstreamSource || got.Name != "Renamed by its owner" {
		t.Errorf("after the second upstream's prune, shared row = %q/%q, want the first source's row intact", got.Source, got.Name)
	}
}

// TestDirectoryOverrideReason_Migration0170 executes 0170 up and down:
// an override row written before the column existed is backfilled with
// the named placeholder, the CHECK then refuses an override without a
// reason and an upstream row with one, the store refuses a blank reason
// before touching the row, and down drops the column.
func TestDirectoryOverrideReason_Migration0170(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 169)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	legacy := dirAddress("LEGACYOVR")
	upstream := dirAddress("UPSTREAMROW")
	flagged := dirAddress("FLAGGEDROW")
	for _, r := range []struct{ addr, source, tag string }{
		{legacy, timescale.DirectoryOperatorOverrideSource, "issuer"},
		{upstream, dirUpstreamSource, "exchange"},
		{flagged, dirUpstreamSource, "unsafe"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO account_directory (address, name, tags, source) VALUES ($1, 'n', ARRAY[$2], $3)`,
			r.addr, r.tag, r.source); err != nil {
			t.Fatalf("seed %s: %v", r.addr, err)
		}
	}

	applyMigrationsUpTo(t, dsn, 170)
	if got := dirOverrideReasonOf(t, ctx, store, legacy); !strings.Contains(got, "migration 0170") {
		t.Errorf("pre-0170 override row reason = %q, want the backfill placeholder", got)
	}
	if got := dirOverrideReasonOf(t, ctx, store, upstream); got != "" {
		t.Errorf("upstream row reason = %q, want NULL", got)
	}

	for name, q := range map[string]string{
		"override without a reason":    `UPDATE account_directory SET source = 'operator-override', override_reason = NULL WHERE address = $1`,
		"override with a blank reason": `UPDATE account_directory SET source = 'operator-override', override_reason = '  ' WHERE address = $1`,
		"upstream row with a reason":   `UPDATE account_directory SET override_reason = 'stale' WHERE address = $1`,
	} {
		if _, err := db.ExecContext(ctx, q, flagged); err == nil || !strings.Contains(err.Error(), "account_directory_override_reason_chk") {
			t.Errorf("%s: err = %v, want the account_directory_override_reason_chk violation", name, err)
		}
	}

	if _, _, _, err := store.ClearDirectoryScamFlag(ctx, flagged, dirOverrideActor, " \t"); !errors.Is(err, timescale.ErrDirectoryOverrideReasonRequired) {
		t.Fatalf("ClearDirectoryScamFlag(blank reason) err = %v, want ErrDirectoryOverrideReasonRequired", err)
	}
	if got := mustDirectoryEntry(t, ctx, store, flagged); got.Source != dirUpstreamSource || !slices.Equal(got.Tags, []string{"unsafe"}) {
		t.Fatalf("a refused takeover changed the row: source=%q tags=%q", got.Source, got.Tags)
	}

	applyMigrationsUpTo(t, dsn, 169)
	var cols int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'account_directory' AND column_name = 'override_reason'`).Scan(&cols); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if cols != 0 {
		t.Errorf("override_reason survives 0170 down")
	}
	if got := mustDirectoryEntry(t, ctx, store, legacy); got.Source != timescale.DirectoryOperatorOverrideSource {
		t.Errorf("0170 down changed the override row's ownership: source = %q", got.Source)
	}
}

// TestDirectoryOverrideBy_Migration0177 executes 0177 up and down: an
// override row written before the column existed is backfilled with the
// named placeholder, the CHECK then refuses an override naming no
// operator and an upstream row naming one, the store refuses a blank
// operator before touching the row and records a real one, and down
// drops the column without touching ownership or the reason.
func TestDirectoryOverrideBy_Migration0177(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 176)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	legacy := dirAddress("LEGACYBY")
	upstream := dirAddress("UPSTREAMBY")
	flagged := dirAddress("FLAGGEDBY")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_directory (address, name, tags, source, override_reason) VALUES ($1, 'n', ARRAY['issuer'], $2, 'pre-0177')`,
		legacy, timescale.DirectoryOperatorOverrideSource); err != nil {
		t.Fatalf("seed legacy override: %v", err)
	}
	for _, r := range []struct{ addr, tag string }{{upstream, "exchange"}, {flagged, "unsafe"}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO account_directory (address, name, tags, source) VALUES ($1, 'n', ARRAY[$2], $3)`,
			r.addr, r.tag, dirUpstreamSource); err != nil {
			t.Fatalf("seed %s: %v", r.addr, err)
		}
	}

	applyMigrationsUpTo(t, dsn, 177)
	if got := dirOverrideByOf(t, ctx, store, legacy); !strings.Contains(got, "migration 0177") {
		t.Errorf("pre-0177 override row operator = %q, want the backfill placeholder", got)
	}
	if got := dirOverrideByOf(t, ctx, store, upstream); got != "" {
		t.Errorf("upstream row operator = %q, want NULL", got)
	}

	for name, q := range map[string]string{
		"override without an operator":    `UPDATE account_directory SET source = 'operator-override', override_reason = 'r', override_by = NULL WHERE address = $1`,
		"override with a blank operator":  `UPDATE account_directory SET source = 'operator-override', override_reason = 'r', override_by = '  ' WHERE address = $1`,
		"upstream row naming an operator": `UPDATE account_directory SET override_by = 'stale' WHERE address = $1`,
	} {
		if _, err := db.ExecContext(ctx, q, flagged); err == nil || !strings.Contains(err.Error(), "account_directory_override_by_chk") {
			t.Errorf("%s: err = %v, want the account_directory_override_by_chk violation", name, err)
		}
	}

	if _, _, _, err := store.ClearDirectoryScamFlag(ctx, flagged, " \t", dirOverrideReason); !errors.Is(err, timescale.ErrDirectoryOverrideOperatorRequired) {
		t.Fatalf("ClearDirectoryScamFlag(blank operator) err = %v, want ErrDirectoryOverrideOperatorRequired", err)
	}
	if got := mustDirectoryEntry(t, ctx, store, flagged); got.Source != dirUpstreamSource || !slices.Equal(got.Tags, []string{"unsafe"}) {
		t.Fatalf("a refused takeover changed the row: source=%q tags=%q", got.Source, got.Tags)
	}
	if _, _, found, err := store.ClearDirectoryScamFlag(ctx, flagged, dirOverrideActor, dirOverrideReason); err != nil || !found {
		t.Fatalf("ClearDirectoryScamFlag: found=%v err=%v", found, err)
	}
	if got := dirOverrideByOf(t, ctx, store, flagged); got != dirOverrideActor {
		t.Errorf("override_by after the takeover = %q, want %q", got, dirOverrideActor)
	}

	applyMigrationsUpTo(t, dsn, 176)
	var cols int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'account_directory' AND column_name = 'override_by'`).Scan(&cols); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if cols != 0 {
		t.Errorf("override_by survives 0177 down")
	}
	if got := mustDirectoryEntry(t, ctx, store, flagged); got.Source != timescale.DirectoryOperatorOverrideSource {
		t.Errorf("0177 down changed the override row's ownership: source = %q", got.Source)
	}
	if got := dirOverrideReasonOf(t, ctx, store, flagged); got != dirOverrideReason {
		t.Errorf("0177 down changed the override reason: %q", got)
	}
}

// TestDirectoryOperatorOverride_ListingRankTierAfterSync pins the
// /v1/assets consumer of the override end to end: the default listing
// ranks a scam-flagged issuer's asset in tier 2, below every unflagged
// one; after the override AND a full upstream sync still carrying the
// flag it ranks on its own merit again; withdrawing the override lets
// the next sync demote it back.
func TestDirectoryOperatorOverride_ListingRankTierAfterSync(t *testing.T) {
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

	flagged, neighbour := charAccount(0xD1), charAccount(0xD2)
	// RIO out-observes NBR, so only the scam tier can put it second.
	seedRankAsset(t, ctx, store.DB(), mustClassicID(t, "RIO", flagged), "RIO", flagged, 5000)
	seedRankAsset(t, ctx, store.DB(), mustClassicID(t, "NBR", neighbour), "NBR", neighbour, 10)
	upstream := []timescale.DirectoryEntry{
		dirEntry(flagged, "Rio Issuer", "issuer", "unsafe"),
		dirEntry(neighbour, "Neighbour", "exchange"),
	}

	assertRank := func(step string, wantOrder []string, wantRioTier int) {
		t.Helper()
		rows := listRankOrder(t, ctx, store, timescale.AssetsOrderObservationCountDesc, 10)
		if got := codesOf(rows); !slices.Equal(got, wantOrder) {
			t.Errorf("%s: listing order = %v, want %v", step, got, wantOrder)
		}
		for _, r := range rows {
			if r.Code == "RIO" && (r.RankTier == nil || *r.RankTier != wantRioTier) {
				t.Errorf("%s: RIO rank_tier = %v, want %d", step, r.RankTier, wantRioTier)
			}
		}
	}

	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	assertRank("upstream flag", []string{"NBR", "RIO"}, 2)

	if _, _, found, err := store.ClearDirectoryScamFlag(ctx, flagged, dirOverrideActor, dirOverrideReason); err != nil || !found {
		t.Fatalf("ClearDirectoryScamFlag: found=%v err=%v", found, err)
	}
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	assertRank("override + sync", []string{"RIO", "NBR"}, 0)

	if removed, err := store.DeleteDirectoryOverride(ctx, flagged); err != nil || !removed {
		t.Fatalf("DeleteDirectoryOverride: removed=%v err=%v", removed, err)
	}
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, upstream)
	assertRank("override withdrawn + sync", []string{"NBR", "RIO"}, 2)
}
