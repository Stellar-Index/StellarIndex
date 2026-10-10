//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The churn ceiling on the daily directory sync, end to end against a
// real Timescale.
//
// The upstream is an unpinned branch of a third-party repo and a scam
// tag withholds the issuer's price. Before the ceiling, ReplaceDirectory
// refused only an EMPTY snapshot: a hijacked or partial one that kept
// a single row would prune every other label (un-withholding every
// scam issuer) or newly flag thousands of issuers (withholding their
// prices) in one committed transaction with nothing failing. These
// cases pin that such a snapshot is refused whole, that the refusal is
// a rollback, that ordinary churn still lands, and that the operator's
// explicit opt-in still accepts it.

// dirChurnEntries renders n distinct upstream rows, the first `flagged`
// of them carrying a scam-class tag.
func dirChurnEntries(n, flagged int) []timescale.DirectoryEntry {
	out := make([]timescale.DirectoryEntry, 0, n)
	for i := range n {
		tag := "exchange"
		if i < flagged {
			tag = "malicious"
		}
		out = append(out, dirEntry(dirAddress("CHURN"+dirLetters(i)), fmt.Sprintf("Row %d", i), tag))
	}
	return out
}

// dirLetters spells i in four base-26 letters: the strkey CHECK's
// alphabet is A-Z2-7, so decimal digits 0/1/8/9 would not pass it.
func dirLetters(i int) string {
	var b [4]byte
	for k := 3; k >= 0; k-- {
		b[k] = byte('A' + i%26)
		i /= 26
	}
	return string(b[:])
}

func dirSourceCounts(t *testing.T, ctx context.Context, store *timescale.Store, source string) (rows, flagged int64) {
	t.Helper()
	db := store.DB()
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM account_directory WHERE source = $1`, source).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM account_directory
		 WHERE source = $1 AND EXISTS (SELECT 1 FROM unnest(tags) t WHERE lower(t) = 'malicious')`, source).Scan(&flagged); err != nil {
		t.Fatalf("count flagged: %v", err)
	}
	return rows, flagged
}

func TestDirectorySync_ChurnCeiling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	canonical.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Bootstrap: 4000 rows, none flagged. The first sync of a source is
	// unbounded (nothing held yet), so this lands whatever its size.
	full := dirChurnEntries(4000, 0)
	res, err := store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, full, timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("bootstrap sync: %v", err)
	}
	if res.Existing != 0 || res.Upserted != 4000 {
		t.Fatalf("bootstrap: existing=%d upserted=%d, want 0/4000", res.Existing, res.Upserted)
	}
	// Default ceiling over 4000 held rows: max(100, ceil(5 %)) = 200.

	// 1. A snapshot that keeps only 3000 rows would prune 1000 > 200:
	//    refused whole, and the table is exactly as it was.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, full[:3000])
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("mass-prune snapshot: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if rows, _ := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 4000 {
		t.Fatalf("after refused prune: %d rows, want 4000 (the refusal must be a rollback)", rows)
	}

	// 2. The same 4000 rows with 1000 newly carrying `malicious` would
	//    withhold 1000 issuers' prices at once: refused, nothing flagged.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, dirChurnEntries(4000, 1000))
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("mass-flag snapshot: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 4000 || flagged != 0 {
		t.Fatalf("after refused flagging: rows=%d flagged=%d, want 4000/0", rows, flagged)
	}

	// 3. Ordinary churn — 150 newly flagged, 150 pruned, both under 200
	//    — lands, and the run reports what it did.
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirChurnEntries(3850, 150), timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("ordinary churn: %v", err)
	}
	if res.Existing != 4000 || res.Pruned != 150 || res.NewlyFlagged != 150 {
		t.Fatalf("ordinary churn: %+v, want existing=4000 pruned=150 newlyFlagged=150", res)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 3850 || flagged != 150 {
		t.Fatalf("after ordinary churn: rows=%d flagged=%d, want 3850/150", rows, flagged)
	}

	// 4. Re-syncing the same flagged rows counts nothing as NEWLY
	//    flagged: the base is "flagged before", not "flagged".
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirChurnEntries(3850, 150), timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("idempotent resync: %v", err)
	}
	if res.NewlyFlagged != 0 || res.Pruned != 0 {
		t.Fatalf("idempotent resync: %+v, want newlyFlagged=0 pruned=0", res)
	}

	// 5. The operator's explicit opt-in accepts the mass prune.
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, full[:1000], timescale.DirectoryChurnUnbounded)
	if err != nil {
		t.Fatalf("-accept-churn sync: %v", err)
	}
	if res.Pruned != 2850 {
		t.Fatalf("-accept-churn sync: pruned=%d, want 2850", res.Pruned)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 1000 || flagged != 0 {
		t.Fatalf("after -accept-churn: rows=%d flagged=%d, want 1000/0", rows, flagged)
	}
}

// dirPaddedEntries renders n rows under `prefix`, the first `flagged`
// tagged with a whitespace-padded, upper-cased scam tag.
func dirPaddedEntries(prefix string, n, flagged int) []timescale.DirectoryEntry {
	out := make([]timescale.DirectoryEntry, 0, n)
	for i := range n {
		tags := []string{"exchange"}
		if i < flagged {
			tags = []string{" MALICIOUS ", "exchange"}
		}
		out = append(out, dirEntry(dirAddress(prefix+dirLetters(i)), fmt.Sprintf("Row %d", i), tags...))
	}
	return out
}

// A padded scam tag must count as newly flagged (the Go price gate trims
// it and withholds), and a snapshot that strips the flags in place must
// be refused like a mass prune: both un-withhold or withhold prices en
// masse without moving the row count.
func TestDirectorySync_ChurnCountsPaddedAndClearedFlags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	canonical.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 0), timescale.DefaultDirectoryChurnLimit); err != nil {
		t.Fatalf("bootstrap sync: %v", err)
	}
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 1000))
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("1000 padded scam tags: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	res, err := store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 1000), timescale.DirectoryChurnUnbounded)
	if err != nil {
		t.Fatalf("-accept-churn padded sync: %v", err)
	}
	if res.NewlyFlagged != 1000 {
		t.Fatalf("padded sync: newlyFlagged=%d, want 1000", res.NewlyFlagged)
	}
	if _, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); flagged != 1000 {
		t.Fatalf("padded tags: %d rows match the SQL predicate, want 1000 (stored canonical)", flagged)
	}
	e, ok, err := store.DirectoryEntryByAddress(ctx, dirAddress("PADS"+dirLetters(0)))
	if err != nil || !ok || !slices.Equal(e.Tags, []string{"malicious", "exchange"}) {
		t.Fatalf("stored tags = %q (ok=%v err=%v), want [malicious exchange]", e.Tags, ok, err)
	}

	// Same 4000 rows, every tag stripped: 1000 un-flagged > 200.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 0))
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("mass un-flag snapshot: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 4000 || flagged != 1000 {
		t.Fatalf("after refused un-flag: rows=%d flagged=%d, want 4000/1000 (the refusal must be a rollback)", rows, flagged)
	}
	// The un-flag cap is 5 % of the 1000 FLAGGED addresses (50), not of
	// the 4000 rows (200): 51 is refused even though it is far under the
	// row cap, and 50 lands.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 949))
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("51 of 1000 flagged un-flagged: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if _, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); flagged != 1000 {
		t.Fatalf("after refused 51 un-flags: flagged=%d, want 1000", flagged)
	}
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirPaddedEntries("PADS", 4000, 950), timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("ordinary un-flag churn: %v", err)
	}
	if res.Unflagged != 50 || res.NewlyFlagged != 0 {
		t.Fatalf("ordinary un-flag churn: %+v, want unflagged=50 newlyFlagged=0", res)
	}
}

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
	return gate.Withheld(ctx, canonical.Asset{Type: canonical.AssetClassic, Code: "RIO", Issuer: issuer}, "price_read")
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

	canonical.InstallAliasRegistry(nil)
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
	//    still carrying `unsafe`. This is where a correction would be
	//    lost.
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

	canonical.InstallAliasRegistry(nil)
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

	canonical.InstallAliasRegistry(nil)
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

// TestAccountErasureTrigger pins 0188's audit_log exception: only the
// exact erasure scrub of a closed account's own rows passes.
func TestAccountErasureTrigger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var acct, other uuid.UUID
	mustScan(t, ctx, db, &acct, `INSERT INTO accounts (name, slug, billing_email) VALUES ('a', 'trig', 'a@x.example') RETURNING id`)
	mustScan(t, ctx, db, &other, `INSERT INTO accounts (name, slug, billing_email) VALUES ('b', 'other', 'b@x.example') RETURNING id`)
	var userRow, staffRow, otherRow uuid.UUID
	mustScan(t, ctx, db, &userRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip, user_agent)
		VALUES ($1, 'user', 'key.mint', '{"name":"k","scopes":["a"]}', '198.51.100.1', 'UA') RETURNING id`, acct)
	mustScan(t, ctx, db, &staffRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip, user_agent)
		VALUES ($1, 'staff', 'admin.account.read', '{"account_slug":"trig","actor_key_id":"kid"}', '203.0.113.1', 'S') RETURNING id`, acct)
	mustScan(t, ctx, db, &otherRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata, ip)
		VALUES ($1, 'user', 'key.mint', '{"name":"k"}', '198.51.100.2') RETURNING id`, other)

	scrub := `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata), ip = NULL, user_agent = NULL WHERE id = $1`
	inTx := func(guc string, stmt string, args ...any) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if guc != "" {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('stellarindex.erasing_account', $1, true)`, guc); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
		return tx.Commit()
	}

	requireAppendOnlyRefusal(t, "scrub without the GUC", inTx("", scrub, userRow))
	requireAppendOnlyRefusal(t, "scrub of an active account", inTx(acct.String(), scrub, userRow))
	mustExec(t, ctx, db, `UPDATE accounts SET status = 'closed' WHERE id = $1`, acct)
	requireAppendOnlyRefusal(t, "scrub of another account's row", inTx(acct.String(), scrub, otherRow))
	requireAppendOnlyRefusal(t, "scrub that rewrites action",
		inTx(acct.String(), `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata), action = 'x' WHERE id = $1`, userRow))
	requireAppendOnlyRefusal(t, "metadata other than the erase function's",
		inTx(acct.String(), `UPDATE audit_log SET metadata = '{}' WHERE id = $1`, userRow))
	requireAppendOnlyRefusal(t, "nulling a staff address", inTx(acct.String(), scrub, staffRow))
	requireAppendOnlyRefusal(t, "delete under the GUC", inTx(acct.String(), `DELETE FROM audit_log WHERE id = $1`, userRow))

	if err := inTx(acct.String(), scrub, userRow); err != nil {
		t.Fatalf("permitted scrub refused: %v", err)
	}
	if err := inTx(acct.String(), `UPDATE audit_log SET metadata = audit_log_erase_metadata(metadata) WHERE id = $1`, staffRow); err != nil {
		t.Fatalf("permitted staff metadata scrub refused: %v", err)
	}
	var meta, staffMeta string
	var ip sql.NullString
	mustScan(t, ctx, db, &meta, `SELECT metadata::text FROM audit_log WHERE id = $1`, userRow)
	mustScan(t, ctx, db, &ip, `SELECT host(ip) FROM audit_log WHERE id = $1`, userRow)
	mustScan(t, ctx, db, &staffMeta, `SELECT metadata::text || host(ip) FROM audit_log WHERE id = $1`, staffRow)
	if meta != `{"scopes": ["a"]}` || ip.Valid {
		t.Errorf("user row after scrub = %s ip=%v, want name removed and ip NULL", meta, ip)
	}
	if staffMeta != `{"actor_key_id": "kid"}203.0.113.1` {
		t.Errorf("staff row after scrub = %s, want slug removed and address kept", staffMeta)
	}
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

func mustScan(t *testing.T, ctx context.Context, db *sql.DB, dst any, q string, args ...any) {
	t.Helper()
	if err := db.QueryRowContext(ctx, q, args...).Scan(dst); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

// TestAccountObservationSeedProvenanceRoundTrip executes migration 0189's
// table through the store: unstamped reads ok=false, a complete
// pass' upsert round-trips every column — including watched_accounts/
// missing_accounts, stored sorted regardless of input order so a `missing`
// count is traceable to a specific G-strkey — and a second complete pass
// overwrites the singleton row rather than adding a second one.
func TestAccountObservationSeedProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, ok, err := store.AccountObservationSeedProvenanceRow(ctx); err != nil || ok {
		t.Fatalf("never stamped: ok=%v err=%v, want ok=false", ok, err)
	}

	// Given deliberately unsorted, so the round trip also proves the store
	// sorts before writing.
	watchedAccounts := []string{"GE", "GC", "GA", "GD", "GB"}
	wantWatchedSorted := []string{"GA", "GB", "GC", "GD", "GE"}
	missingAccounts := []string{"GB"}

	minL, maxL := uint32(30_000_000), uint32(63_400_000)
	first := timescale.AccountObservationSeedProvenance{
		AccountsWatched: 5, WatchedAccounts: watchedAccounts,
		AccountsSeeded: 3, AccountsMissing: 1, MissingAccounts: missingAccounts, AccountsRemoved: 1,
		MinLedgerSeen: &minL, MaxLedgerSeen: &maxL,
	}
	if err := store.UpsertAccountObservationSeedProvenance(ctx, first); err != nil {
		t.Fatalf("UpsertAccountObservationSeedProvenance: %v", err)
	}
	got, ok, err := store.AccountObservationSeedProvenanceRow(ctx)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.AccountsWatched != 5 || got.AccountsSeeded != 3 || got.AccountsMissing != 1 || got.AccountsRemoved != 1 ||
		got.MinLedgerSeen == nil || *got.MinLedgerSeen != minL || got.MaxLedgerSeen == nil || *got.MaxLedgerSeen != maxL || got.SeededAt.IsZero() {
		t.Errorf("read back %+v, want %+v", got, first)
	}
	if !reflect.DeepEqual(got.WatchedAccounts, wantWatchedSorted) {
		t.Errorf("WatchedAccounts = %v, want %v (sorted)", got.WatchedAccounts, wantWatchedSorted)
	}
	if !reflect.DeepEqual(got.MissingAccounts, missingAccounts) {
		t.Errorf("MissingAccounts = %v, want %v", got.MissingAccounts, missingAccounts)
	}

	// A second complete pass — a re-seed weeks later that this time finds
	// every account already live — overwrites the one row; it must not
	// duplicate it (the primary key is the fixed scope, not a per-run id).
	second := timescale.AccountObservationSeedProvenance{
		AccountsWatched: 5, WatchedAccounts: watchedAccounts, AccountsSeeded: 5,
	}
	if err := store.UpsertAccountObservationSeedProvenance(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _, err = store.AccountObservationSeedProvenanceRow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountsWatched != 5 || got.AccountsSeeded != 5 || got.AccountsMissing != 0 || got.AccountsRemoved != 0 ||
		got.MinLedgerSeen != nil || got.MaxLedgerSeen != nil {
		t.Errorf("after overwrite %+v, want %+v with nil ledger bounds and no second row", got, second)
	}
	if !reflect.DeepEqual(got.WatchedAccounts, wantWatchedSorted) {
		t.Errorf("WatchedAccounts after overwrite = %v, want %v (sorted)", got.WatchedAccounts, wantWatchedSorted)
	}
	if len(got.MissingAccounts) != 0 {
		t.Errorf("MissingAccounts after overwrite = %v, want empty — the second pass found nothing missing", got.MissingAccounts)
	}

	var rowCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM account_observation_seed_provenance").Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("account_observation_seed_provenance has %d row(s), want exactly 1 (upsert, not insert)", rowCount)
	}
}

// TestLatestAccountObservationsAtOrBefore_MatchesSingleRead executes the
// batch read against real Postgres: per account it must return exactly the
// row the single-account read returns, and omit accounts with no
// observation at or before the ledger bound.
func TestLatestAccountObservationsAtOrBefore_MatchesSingleRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		multi   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		single  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		future  = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
		unknown = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
		asOf    = uint32(25)
	)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, o := range []domain.AccountObservation{
		{AccountID: multi, Ledger: 10, HomeDomain: "old.example"},
		{AccountID: multi, Ledger: 20, HomeDomain: "new.example"},
		{AccountID: multi, Ledger: 30, IsRemoval: true},
		{AccountID: single, Ledger: 5},
		{AccountID: future, Ledger: 40, HomeDomain: "future.example"},
	} {
		o.ObservedAt = t0.Add(time.Duration(o.Ledger) * 5 * time.Second)
		o.Balance = big.NewInt(int64(o.Ledger))
		if err := store.InsertAccountObservation(ctx, o); err != nil {
			t.Fatalf("InsertAccountObservation %s@%d: %v", o.AccountID, o.Ledger, err)
		}
	}

	got, err := store.LatestAccountObservationsAtOrBefore(ctx, []string{multi, single, future, unknown}, asOf)
	if err != nil {
		t.Fatalf("LatestAccountObservationsAtOrBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2 (multi, single): %+v", len(got), got)
	}
	if row := got[multi]; row.Ledger != 20 || row.HomeDomain == nil || *row.HomeDomain != "new.example" {
		t.Errorf("multi: ledger=%d home_domain=%v, want 20 new.example", row.Ledger, row.HomeDomain)
	}
	if row := got[single]; row.HomeDomain != nil {
		t.Errorf("single: home_domain=%q, want NULL", *row.HomeDomain)
	}
	for _, acc := range []string{multi, single} {
		one, err := store.LatestAccountObservationAtOrBefore(ctx, acc, asOf)
		if err != nil {
			t.Fatalf("LatestAccountObservationAtOrBefore %s: %v", acc, err)
		}
		b := got[acc]
		if b.Ledger != one.Ledger || b.IsRemoval != one.IsRemoval || b.Balance.Cmp(one.Balance) != 0 ||
			(b.HomeDomain == nil) != (one.HomeDomain == nil) {
			t.Errorf("%s: batch %+v != single %+v", acc, b, one)
		}
	}

	latest, err := store.LatestAccountObservationsAtOrBefore(ctx, []string{multi}, ^uint32(0))
	if err != nil {
		t.Fatalf("unbounded asOf must be capped to int4, got: %v", err)
	}
	if !latest[multi].IsRemoval {
		t.Errorf("unbounded read: multi=%+v, want the ledger-30 removal", latest[multi])
	}

	empty, err := store.LatestAccountObservationsAtOrBefore(ctx, nil, asOf)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input: got %v, %v; want empty map, nil", empty, err)
	}
}

// Two SDF-reserve-style watched accounts. The storage layer treats the
// AccountID as opaque, but these are well-formed G-strkeys for realism.
var watermarkReserveAccounts = []string{
	"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
	"GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUV5H",
}

// watermarkStoreAdapter maps *timescale.Store onto
// supply.AccountObservationLookup — the same 20-line adapter the aggregator
// and ops binaries inline (they each own their binary and don't share it).
type watermarkStoreAdapter struct{ s *timescale.Store }

func (a watermarkStoreAdapter) LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (supply.AccountObservationRow, error) {
	row, err := a.s.LatestAccountObservationAtOrBefore(ctx, accountID, asOfLedger)
	if err != nil {
		return supply.AccountObservationRow{}, err
	}
	return supply.AccountObservationRow{
		Balance:   row.Balance,
		IsRemoval: row.IsRemoval,
		Ledger:    row.Ledger,
	}, nil
}

func (a watermarkStoreAdapter) MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error) {
	return a.s.MaxAccountObservationLedger(ctx, asOfLedger)
}

// fixedLedgers is a supply.LedgerLookup that reports one pinned chain tip —
// the snapshot ledger the refresher evaluates against.
type fixedLedgers struct {
	ledger uint32
	at     time.Time
}

func (f fixedLedgers) LatestKnownLedger(context.Context) (uint32, time.Time, error) {
	return f.ledger, f.at, nil
}

// capturingInserter records how many snapshots the refresher accepted (and the
// last one), so a test can distinguish an ACCEPT (gate passed) from a REJECT.
type capturingInserter struct {
	calls int
	last  supply.Supply
}

func (c *capturingInserter) InsertSupply(_ context.Context, s supply.Supply) error {
	c.calls++
	c.last = s
	return nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// insertQuietReserveObservations seeds one observation per reserve account at
// lastChangeLedger and returns the observation timestamp. This models the live
// r1 state: the reserve accounts' NEWEST rows sit at their last balance change,
// deep in the past relative to the current tip.
func insertQuietReserveObservations(t *testing.T, ctx context.Context, store *timescale.Store, lastChangeLedger uint32) {
	t.Helper()
	obsAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, acc := range watermarkReserveAccounts {
		if err := store.InsertAccountObservation(ctx, domain.AccountObservation{
			AccountID:  acc,
			Ledger:     lastChangeLedger,
			ObservedAt: obsAt,
			Balance:    big.NewInt(int64(100 * (i + 1))), // small non-zero reserve
		}); err != nil {
			t.Fatalf("InsertAccountObservation %s@%d: %v", acc, lastChangeLedger, err)
		}
	}
}

func newWatermarkRefresher(store *timescale.Store, snapshotLedger uint32, inserter *capturingInserter) *supply.Refresher {
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	reader := supply.NewLCMReserveBalanceReader(watermarkStoreAdapter{s: store})
	computer, err := supply.NewXLMComputer(watermarkReserveAccounts, reader)
	if err != nil {
		// NewXLMComputer only errors on nil-reader-with-accounts; we always
		// pass a reader, so this is a programming error if hit.
		panic(err)
	}
	// Default thresholds: 1000-ledger stale gate, 17280-ledger (~1 day)
	// dormancy horizon — the exact production defaults the r1 gate ran under.
	return supply.NewRefresher(
		fixedLedgers{ledger: snapshotLedger, at: t0},
		computer,
		inserter,
		discardLogger(),
	)
}

// TestAccountObserverWatermark_QuietObserverStaysFresh is the money-adjacent
// regression proof. A HEALTHY account observer
// that has PROCESSED up to a fresh ledger but whose watched reserve accounts
// have not CHANGED for far longer than the dormancy horizon must NOT trip the
// XLM supply freshness gate.
//
// A reader that took Store.MaxAccountObservationLedger
// returned MAX(ledger) FROM account_observations = the last balance-change
// ledger (50_000_000). The anchor assertion below fails (got 50_000_000, want
// the 50_030_000 watermark), and — end to end — the refresher rejects the
// snapshot as stale_component (gap 30_000 > horizon 17_280) instead of
// accepting it. With the fix the anchor is the fresh watermark and the gate
// accepts.
func TestAccountObserverWatermark_QuietObserverStaysFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lastChange = uint32(50_000_000)
	// The observer has processed 30_000 ledgers past the last reserve change —
	// well past DefaultMaxDormantComponentLedgers (17_280). A quiet asset, a
	// healthy observer.
	const processed = lastChange + 30_000

	insertQuietReserveObservations(t, ctx, store, lastChange)
	if err := store.UpsertAccountObserverWatermark(ctx, processed); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark: %v", err)
	}

	// ── Storage-level: the anchor is the observer WATERMARK, not the last
	// balance change. This is the exact assertion that is red on unfixed code.
	anchor, err := store.MaxAccountObservationLedger(ctx, processed)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor == lastChange {
		t.Fatalf("anchor regressed to the last balance-change ledger %d — a quiet "+
			"reserve account is NOT a stalled observer; this is exactly what froze "+
			"XLM supply and cried wolf on r1", lastChange)
	}
	if anchor != processed {
		t.Fatalf("anchor = %d, want %d (the observer watermark)", anchor, processed)
	}

	// ── End-to-end: the refresher ACCEPTS the snapshot (gate passes).
	inserter := &capturingInserter{}
	out := newWatermarkRefresher(store, processed, inserter).Tick(ctx)
	if out.Kind != supply.OutcomeKindOK {
		t.Fatalf("gate rejected a healthy quiet observer: kind=%s err=%v — the "+
			"freshness anchor must track the observer watermark, not the last "+
			"balance change", out.Kind, out.Err)
	}
	if inserter.calls != 1 {
		t.Fatalf("InsertSupply calls = %d, want 1 (snapshot accepted)", inserter.calls)
	}
}

// TestAccountObserverWatermark_StalledObserverStillTrips proves dead-observer
// detection is PRESERVED. When the observer's watermark is FROZEN (it stopped
// advancing) while the chain tip keeps climbing, the anchor freezes at the
// watermark and the gate correctly fails closed past the dormancy horizon —
// it does not republish a frozen supply stamped at the current tip.
func TestAccountObserverWatermark_StalledObserverStillTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lastChange = uint32(50_000_000)
	// The observer advanced a little, then STALLED (died).
	const frozenWatermark = lastChange + 100
	// The chain tip, meanwhile, has run far ahead of the frozen watermark.
	const snapshotLedger = frozenWatermark + 30_000

	insertQuietReserveObservations(t, ctx, store, lastChange)
	if err := store.UpsertAccountObserverWatermark(ctx, frozenWatermark); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark: %v", err)
	}

	// The anchor is the FROZEN watermark (bounded by, but far below, the tip).
	anchor, err := store.MaxAccountObservationLedger(ctx, snapshotLedger)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor != frozenWatermark {
		t.Fatalf("anchor = %d, want %d (the frozen observer watermark)", anchor, frozenWatermark)
	}

	// The gate must fail closed: a dead observer is not a dormant asset.
	inserter := &capturingInserter{}
	out := newWatermarkRefresher(store, snapshotLedger, inserter).Tick(ctx)
	if out.Kind != supply.OutcomeKindStaleComponent {
		t.Fatalf("gate did not fail closed on a stalled observer: kind=%s err=%v — "+
			"a frozen watermark past the dormancy horizon must be rejected as "+
			"stale_component", out.Kind, out.Err)
	}
	if inserter.calls != 0 {
		t.Fatalf("InsertSupply calls = %d, want 0 (snapshot rejected)", inserter.calls)
	}
}

// TestAccountObserverWatermark_Monotonic pins the never-regress guard: a lower
// or equal processed_ledger is a silent no-op, so an out-of-order or replayed
// writer can never drag the freshness anchor backwards.
func TestAccountObserverWatermark_Monotonic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const high = uint32(50_050_000)
	if err := store.UpsertAccountObserverWatermark(ctx, high); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark(high): %v", err)
	}
	// A regressing write must not move the anchor.
	if err := store.UpsertAccountObserverWatermark(ctx, high-10_000); err != nil {
		t.Fatalf("UpsertAccountObserverWatermark(regress): %v", err)
	}
	// asOf is set above `high` so the LEAST() bound doesn't clamp the result.
	anchor, err := store.MaxAccountObservationLedger(ctx, high+1)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger: %v", err)
	}
	if anchor != high {
		t.Fatalf("anchor = %d, want %d — a regressing upsert must be a no-op", anchor, high)
	}

	// Empty-table posture is exercised implicitly elsewhere, but assert the
	// bound too: asOf below the watermark clamps the anchor to asOf.
	bounded, err := store.MaxAccountObservationLedger(ctx, high-5_000)
	if err != nil {
		t.Fatalf("MaxAccountObservationLedger(bounded): %v", err)
	}
	if bounded != high-5_000 {
		t.Fatalf("bounded anchor = %d, want %d (LEAST clamps to asOf)", bounded, high-5_000)
	}
}

// TestFreezeEvents_ReleasedBy executes migration 0208 and every write that
// touches the column: MarkRecovered on a row in a COMPRESSED chunk, on a live
// row, and the previous binary's UPDATE, which must keep working and leave it
// NULL (migrations/README.md rule 9).
func TestFreezeEvents_ReleasedBy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	quote, _ := canonical.NewFiatAsset("USD")
	releasedBy := func(asset canonical.Asset) (sql.NullString, bool) {
		t.Helper()
		var by sql.NullString
		var closed bool
		if err := db.QueryRowContext(ctx, `
			SELECT released_by, recovered_at IS NOT NULL FROM freeze_events
			 WHERE asset_id = $1 AND quote_id = $2`, asset.String(), quote.String()).Scan(&by, &closed); err != nil {
			t.Fatalf("read released_by for %s: %v", asset.String(), err)
		}
		return by, closed
	}

	// 1. An open row in an old chunk, compressed before MarkRecovered runs.
	// Ledger 0: the sink has no ledger provider and stamps recovered_at_ledger 0.
	old := canonical.NativeAsset()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO freeze_events (asset_id, quote_id, frozen_at, frozen_at_ledger, reason, frozen_value)
		VALUES ($1, $2, TIMESTAMPTZ '2025-01-15 00:00:00Z', 0, 'outlier_storm', 0.5)`,
		old.String(), quote.String()); err != nil {
		t.Fatalf("seed old open row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(ch) FROM show_chunks('freeze_events', older_than => now() - INTERVAL '90 days') ch
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no freeze_events chunk was compressed — the compressed-UPDATE claim would be vacuous")
	}
	if err := sink.MarkRecovered(ctx, old, quote, "operator:ash"); err != nil {
		t.Fatalf("MarkRecovered on a compressed chunk: %v", err)
	}
	if by, closed := releasedBy(old); !closed || by.String != "operator:ash" {
		t.Errorf("compressed row: closed=%v released_by=%v, want closed with operator:ash", closed, by)
	}

	// 2. A live row closed by the recovery worker's value.
	auto, _ := canonical.NewClassicAsset("USDT", "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin}
	if err := sink.RecordFreeze(ctx, auto, quote, "1.000000000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	if by, closed := releasedBy(auto); closed || by.Valid {
		t.Errorf("open row: closed=%v released_by=%v, want open with NULL", closed, by)
	}
	if err := sink.MarkRecovered(ctx, auto, quote, freeze.ReleasedBySystemRecovery); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	if by, closed := releasedBy(auto); !closed || by.String != freeze.ReleasedBySystemRecovery {
		t.Errorf("auto row: closed=%v released_by=%v, want closed with %s", closed, by, freeze.ReleasedBySystemRecovery)
	}

	// 3. The pre-0208 binary's literal UPDATE against the 0208 schema.
	prev, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err := sink.RecordFreeze(ctx, prev, quote, "1.000000000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE freeze_events
		   SET recovered_at        = $3,
		       recovered_at_ledger = $4
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		prev.String(), quote.String(), time.Now().UTC(), int64(0))
	if err != nil {
		t.Fatalf("previous-binary MarkRecovered UPDATE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("previous-binary UPDATE closed %d rows, want 1", n)
	}
	if by, closed := releasedBy(prev); !closed || by.Valid {
		t.Errorf("previous-binary row: closed=%v released_by=%v, want closed with NULL", closed, by)
	}
}

// TestFreezeEventSink_LKGVWAPLandsOnRow exercises the path:
// `RecordFreeze` writes the frozen_value column with the
// orchestrator-supplied LKG VWAP rather than the hardcoded 0 the
// a bare insert would write.
func TestFreezeEventSink_LKGVWAPLandsOnRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := timescale.NewFreezeEventSink(store)

	asset, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := canonical.NewFiatAsset("USD")

	const lkg = "0.999450000000"
	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 7.5,
		Reason:       "deviation 7.5% exceeds 5% threshold for stablecoin",
	}

	if err := sink.RecordFreeze(ctx, asset, quote, lkg, decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}

	// Read it back via raw SQL — easier than threading a Lookup API
	// just for this assertion.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var (
		frozenValueRaw string
		reason         string
	)
	err = db.QueryRowContext(ctx, `
		SELECT frozen_value::text, reason
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2
		 ORDER BY frozen_at DESC
		 LIMIT 1
	`, asset.String(), quote.String()).Scan(&frozenValueRaw, &reason)
	if err != nil {
		t.Fatalf("read freeze_events row: %v", err)
	}
	// Postgres NUMERIC formats trailing zeros: 0.999450000000.
	if frozenValueRaw != lkg {
		t.Errorf("frozen_value = %q, want %q", frozenValueRaw, lkg)
	}
	// Reason classification from mapFreezeReason: deviation-driven
	// Phase 1 freezes map to outlier_storm.
	if reason != "outlier_storm" {
		t.Errorf("reason = %q, want outlier_storm", reason)
	}
}

// TestFreezeEventSink_FirstTickFreezeHookGetsNoSentinel: a first-tick
// freeze has no prior bucket, so the NUMERIC NOT NULL column is filled with
// 0 — but the fan-out hook (the customer webhook's frozen_value) must get
// the empty value, not the filler it would read as a zero price.
func TestFreezeEventSink_FirstTickFreezeHookGetsNoSentinel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var (
		calls    int
		hookSeen string
	)
	sink := timescale.NewFreezeEventSink(store, timescale.WithFreezeHook(
		func(_ context.Context, _, _ canonical.Asset, frozenValue string, _ anomaly.Decision) {
			calls++
			hookSeen = frozenValue
		}))

	asset, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := canonical.NewFiatAsset("USD")
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, DeviationPct: 7.5, Reason: "first tick"}
	if err := sink.RecordFreeze(ctx, asset, quote, "", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	if calls != 1 || hookSeen != "" {
		t.Errorf("hook calls = %d, frozenValue = %q; want one call with the empty value", calls, hookSeen)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT frozen_value::text FROM freeze_events WHERE asset_id = $1 AND quote_id = $2`,
		asset.String(), quote.String()).Scan(&stored); err != nil {
		t.Fatalf("read freeze_events row: %v", err)
	}
	if stored != "0" {
		t.Errorf("stored frozen_value = %q, want the column filler 0", stored)
	}
}

// TestFreezeEventSink_RecoveryRoundTrip exercises the path:
// ListOpen → MarkRecovered → ListOpen returns one fewer row. Pins
// the worker's contract against the postgres schema.
func TestFreezeEventSink_RecoveryRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := timescale.NewFreezeEventSink(store)
	asset, _ := canonical.NewClassicAsset("USDT", "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	quote, _ := canonical.NewFiatAsset("USD")

	if err := sink.RecordFreeze(ctx, asset, quote, "1.000000000000", anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassStablecoin,
	}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}

	open, err := sink.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 open freeze, got %d: %+v", len(open), open)
	}
	if open[0].Asset.String() != asset.String() {
		t.Errorf("open[0].Asset = %s, want %s", open[0].Asset.String(), asset.String())
	}
	// Conform to the freeze.OpenFreezePair shape that the recovery
	// worker consumes (compile-time check via type assertion).
	var _ freeze.OpenFreezePair = open[0]

	if err := sink.MarkRecovered(ctx, asset, quote, "operator:test"); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}

	open, err = sink.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen after recovery: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("expected 0 open freezes after MarkRecovered, got %d: %+v", len(open), open)
	}

	// Re-running MarkRecovered on an already-closed pair returns
	// ErrNotFound — operators relying on the idempotent-by-skip
	// semantics catch this loudly.
	err = sink.MarkRecovered(ctx, asset, quote, "operator:test")
	if err == nil {
		t.Error("expected ErrNotFound on second MarkRecovered, got nil")
	}
}

// TestFreezeEventSink_IdempotentOpenRow — two RecordFreeze calls
// for the same (asset, quote) while the first is still firing
// (recovered_at NULL) MUST NOT insert a second row. The guard
// does not rely on the (asset, quote, frozen_at) PK with microsecond
// resolution; the explicit `WHERE NOT EXISTS` clause is what
// guarantees idempotency.
func TestFreezeEventSink_IdempotentOpenRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := timescale.NewFreezeEventSink(store)
	asset, _ := canonical.NewClassicAsset("PYUSD", "GBGQNZAZ54NZWZA7KGOTOZYCXEYIQGOUJK7L6EM7EJD7AQRBKO7VSXJP")
	quote, _ := canonical.NewFiatAsset("USD")

	dec := anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin}
	for i := 0; i < 3; i++ {
		if err := sink.RecordFreeze(ctx, asset, quote, "1.000000000000", dec); err != nil {
			t.Fatalf("RecordFreeze #%d: %v", i, err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var count int
	err = db.QueryRowContext(ctx, `
		SELECT count(*) FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2
	`, asset.String(), quote.String()).Scan(&count)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 freeze_events row across 3 RecordFreeze calls, got %d", count)
	}
}

const (
	auctionPool       = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	auctionCollateral = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	auctionDebt       = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	auctionBackstopLP = "CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S"
	liquidatedAcct    = "GLIQUIDATEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	fillerAcct        = "GFILLERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	badDebtAcct       = "GBADDEBTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	bystanderAcct     = "GBYSTANDERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	backstopAcct      = "CBACKSTOPAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	badDebtFillerAcct = "GBADDEBTFILLERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// auctionLeg is one expected Blend leg: underlying net, superseded flag,
// b/d-token balance, last ledger.
type auctionLeg struct {
	net        string
	superseded bool
	tokens     string
	ledger     uint32
}

// TestPositionsFold_BlendAuctionSupersedesLegs pins, through real SQL,
// that BlendPositionsByUser flags every Blend leg a liquidation fill, a
// bad-debt auction fill or a bad_debt write-off moved — for the auctioned
// account AND the filler, after a PARTIAL fill, and still after later
// activity on the leg — with the exact b/d-token balance counting those
// moves, while an Interest auction touches nothing; and that
// DeFiPositionHolders leaves every moved leg out.
func TestPositionsFold_BlendAuctionSupersedesLegs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	seedBlendAuctionFixture(ctx, t, store, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	type fold struct{ supply, borrow *auctionLeg }
	cases := map[string]map[string]fold{
		// b 9000 + 600 in, 4500 + 5100 seized -> 0 bTokens; d 4800 in,
		// 2400 + 2000 seized -> 400 dTokens still owed.
		liquidatedAcct: {
			auctionCollateral: {supply: &auctionLeg{"10700", true, "0", 106}},
			auctionDebt:       {borrow: &auctionLeg{"5000", true, "400", 106}},
		},
		// Own supply (b 45) plus both lots; took on both bids with no
		// blend_positions borrow row of its own.
		fillerAcct: {
			auctionCollateral: {supply: &auctionLeg{"50", true, "9645", 106}},
			auctionDebt:       {borrow: &auctionLeg{"0", true, "4400", 106}},
		},
		badDebtAcct:       {auctionDebt: {borrow: &auctionLeg{"30", true, "0", 104}}},
		backstopAcct:      {auctionDebt: {borrow: &auctionLeg{"0", true, "-28", 108}}},
		badDebtFillerAcct: {auctionDebt: {borrow: &auctionLeg{"0", true, "28", 108}}},
		bystanderAcct:     {auctionCollateral: {supply: &auctionLeg{"10", false, "10", 100}}},
	}
	for acct, want := range cases {
		folds, err := store.BlendPositionsByUser(ctx, acct)
		if err != nil {
			t.Fatalf("BlendPositionsByUser(%s): %v", acct, err)
		}
		if len(folds) != len(want) {
			t.Fatalf("%s folds = %+v, want %d (pool, asset) rows", acct, folds, len(want))
		}
		for _, f := range folds {
			w := want[f.Asset]
			var got fold
			if f.HasSupplyLeg {
				got.supply = &auctionLeg{f.SupplyNet, f.SupplySuperseded, f.SupplyTokens, f.SupplyLastLedger}
			}
			if f.HasBorrowLeg {
				got.borrow = &auctionLeg{f.BorrowNet, f.BorrowSuperseded, f.BorrowTokens, f.BorrowLastLedger}
			}
			if f.Pool != auctionPool || !sameAuctionLeg(got.supply, w.supply) || !sameAuctionLeg(got.borrow, w.borrow) {
				t.Errorf("%s %s fold = %+v, want supply %+v borrow %+v", acct, f.Asset, f, w.supply, w.borrow)
			}
		}
	}

	holders, err := store.DeFiPositionHolders(ctx)
	if err != nil {
		t.Fatalf("DeFiPositionHolders: %v", err)
	}
	var blendLegs []timescale.DeFiPositionHolder
	for _, h := range holders {
		if h.Protocol == "blend" && (h.PositionKind == "lending_supply" || h.PositionKind == "lending_borrow") {
			blendLegs = append(blendLegs, h)
		}
	}
	if len(blendLegs) != 1 || blendLegs[0].User != bystanderAcct || blendLegs[0].Amount != "10" {
		t.Errorf("blend holder legs = %+v, want only the untouched bystander supply of 10", blendLegs)
	}
}

func sameAuctionLeg(a, b *auctionLeg) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// seedBlendAuctionFixture writes the money-market events, two partial
// UserLiquidation fills with user activity between them, a BadDebt fill,
// an Interest fill (which must move no pool position) and a bad_debt
// write-off.
func seedBlendAuctionFixture(ctx context.Context, t *testing.T, store *timescale.Store, t0 time.Time) {
	t.Helper()
	at := func(ledger uint32) time.Time { return t0.Add(time.Duration(ledger) * time.Second) }
	positions := []struct {
		kind, asset, acct string
		amount, tokens    int64
		ledger            uint32
	}{
		{blend.EventSupplyCollateral, auctionCollateral, liquidatedAcct, 10000, 9000, 100},
		{blend.EventBorrow, auctionDebt, liquidatedAcct, 5000, 4800, 101},
		{blend.EventSupplyCollateral, auctionCollateral, liquidatedAcct, 700, 600, 105},
		{blend.EventSupply, auctionCollateral, fillerAcct, 50, 45, 99},
		{blend.EventSupply, auctionCollateral, bystanderAcct, 10, 10, 100},
		{blend.EventBorrow, auctionDebt, badDebtAcct, 30, 28, 101},
	}
	for i, r := range positions {
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: auctionPool, Kind: r.kind, Asset: r.asset, User: r.acct,
			TokenAmount: big.NewInt(r.amount), BOrDAmount: big.NewInt(r.tokens),
			Ledger: r.ledger, TxHash: pad64("a", i), Timestamp: at(r.ledger),
		}); err != nil {
			t.Fatalf("InsertBlendPositionEvent %d: %v", i, err)
		}
	}
	amounts := func(asset string, n int64) []blend.AssetAmount {
		return []blend.AssetAmount{{Asset: canonical.Asset{Type: canonical.AssetSoroban, ContractID: asset}, Amount: big.NewInt(n)}}
	}
	fills := []blend.FillAuctionEvent{
		{
			AuctionType: 0, User: liquidatedAcct, Filler: fillerAcct, FillPercent: big.NewInt(50), Ledger: 103,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 2400), Lot: amounts(auctionCollateral, 4500)},
		},
		{
			AuctionType: 0, User: liquidatedAcct, Filler: fillerAcct, FillPercent: big.NewInt(100), Ledger: 106,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 2000), Lot: amounts(auctionCollateral, 5100)},
		},
		{
			AuctionType: 2, User: backstopAcct, Filler: bystanderAcct, FillPercent: big.NewInt(100), Ledger: 107,
			Data: &blend.AuctionData{Bid: amounts(auctionBackstopLP, 70), Lot: amounts(auctionCollateral, 90)},
		},
		{
			AuctionType: 1, User: backstopAcct, Filler: badDebtFillerAcct, FillPercent: big.NewInt(100), Ledger: 108,
			Data: &blend.AuctionData{Bid: amounts(auctionDebt, 28), Lot: amounts(auctionBackstopLP, 60)},
		},
	}
	for i, f := range fills {
		f.Pool, f.TxHash, f.Timestamp = auctionPool, pad64("b", i), at(f.Ledger)
		if err := store.InsertBlendFillAuction(ctx, f); err != nil {
			t.Fatalf("InsertBlendFillAuction %d: %v", i, err)
		}
	}
	if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
		Pool: auctionPool, Kind: blend.EventBadDebt, Asset: auctionDebt, User: badDebtAcct,
		Amount: big.NewInt(28), Ledger: 104, TxHash: pad64("c", 0), Timestamp: at(104),
	}); err != nil {
		t.Fatalf("InsertBlendEmissionEvent bad_debt: %v", err)
	}
}

// TestPositionsFold_AllSixProtocols exercises every SQL fold query
// GET /v1/accounts/{g}/positions reads (internal/storage/timescale/
// positions.go) through real TimescaleDB — the layer go build/go vet
// cannot validate (raw SQL strings). One test user, one row of
// contributing activity per protocol (plus a second, offsetting row
// where the fold's sign convention needs to be proven, not just its
// existence), asserting the fold nets match hand-computed expectations.
func TestPositionsFold_AllSixProtocols(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const user = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// ─── blend money-market: supply 1000, withdraw 400 -> net 600;
	// borrow 300, flash_loan 999, repay 100 -> net 1199 (a flash loan
	// mints debt tokens exactly as borrow does).
	const (
		blendPool  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		blendAsset = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	)
	blendRows := []struct {
		kind   string
		amount int64
		ledger uint32
	}{
		{blend.EventSupply, 1000, 70_000_000},
		{blend.EventWithdraw, 400, 70_000_001},
		{blend.EventBorrow, 300, 70_000_002},
		{blend.EventRepay, 100, 70_000_003},
		{blend.EventFlashLoan, 999, 70_000_004},
	}
	for i, r := range blendRows {
		ev := domain.BlendPositionEvent{
			Pool: blendPool, Kind: r.kind, Asset: blendAsset, User: user,
			TokenAmount: big.NewInt(r.amount), BOrDAmount: big.NewInt(r.amount),
			Ledger: r.ledger, TxHash: pad64("p", i), OpIndex: 0, EventIndex: 0,
			Timestamp: t0.Add(time.Duration(i) * time.Minute),
		}
		if r.kind == blend.EventFlashLoan {
			ev.Counterparty = "CAXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXY32S"
		}
		if err := store.InsertBlendPositionEvent(ctx, ev); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s): %v", r.kind, err)
		}
	}

	blendFolds, err := store.BlendPositionsByUser(ctx, user)
	if err != nil {
		t.Fatalf("BlendPositionsByUser: %v", err)
	}
	if len(blendFolds) != 1 {
		t.Fatalf("BlendPositionsByUser rows = %d, want 1 (single pool/asset group): %+v", len(blendFolds), blendFolds)
	}
	bf := blendFolds[0]
	if bf.SupplyNet != "600" {
		t.Errorf("blend SupplyNet = %q, want 600 (1000 supply - 400 withdraw)", bf.SupplyNet)
	}
	if bf.BorrowNet != "1199" {
		t.Errorf("blend BorrowNet = %q, want 1199 (300 borrow + 999 flash_loan - 100 repay)", bf.BorrowNet)
	}
	if !bf.HasSupplyLeg || !bf.HasBorrowLeg {
		t.Errorf("blend fold legs = supply:%v borrow:%v, want both true", bf.HasSupplyLeg, bf.HasBorrowLeg)
	}
	assertBlendFlashLoanFolds(ctx, t, store, blendPool, t0)

	// ─── blend backstop: deposit(amount=1000,shares=900), then
	// withdraw(amount=tokens_out,shares_burned=300) -> shares net 600.
	// queue_withdrawal must NOT move the net.
	const backstopPool = blendPool
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_000, TxHash: pad64("q", 0), OpIndex: 0, EventIndex: 0, ObservedAt: t0,
		EventType: timescale.BackstopDeposit, Pool: backstopPool, UserAddress: user,
		Amount: "1000", Amount2: "900",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (deposit): %v", err)
	}
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_001, TxHash: pad64("q", 1), OpIndex: 0, EventIndex: 0, ObservedAt: t0.Add(time.Minute),
		EventType: timescale.BackstopQueueWithdrawal, Pool: backstopPool, UserAddress: user,
		Amount: "300",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (queue_withdrawal): %v", err)
	}
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     71_000_002, TxHash: pad64("q", 2), OpIndex: 0, EventIndex: 0, ObservedAt: t0.Add(2 * time.Minute),
		EventType: timescale.BackstopWithdraw, Pool: backstopPool, UserAddress: user,
		Amount: "290", Amount2: "300",
	}); err != nil {
		t.Fatalf("InsertBlendBackstopEvent (withdraw): %v", err)
	}

	backstopFolds, err := store.BlendBackstopSharesByUser(ctx, user)
	if err != nil {
		t.Fatalf("BlendBackstopSharesByUser: %v", err)
	}
	if len(backstopFolds) != 1 || backstopFolds[0].SharesNet != "600" {
		t.Errorf("BlendBackstopSharesByUser = %+v, want SharesNet=600 (900 deposit shares - 300 withdraw shares; queue_withdrawal excluded)", backstopFolds)
	}

	// ─── phoenix stake: bond 500, unbond 150 -> net 350.
	const stakeContract = "CBRGNWGAC25AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const lpToken = "CLPTOKEN0000000000000000000000000000000000000000000000000"
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 72_000_000, ObservedAt: t0, TxHash: pad64("r", 0), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixBond, User: user, LPToken: lpToken, Amount: "500",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (bond): %v", err)
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 72_000_001, ObservedAt: t0.Add(time.Minute), TxHash: pad64("r", 1), OpIndex: 0, EventIndex: 0,
		Action: timescale.PhoenixUnbond, User: user, LPToken: lpToken, Amount: "150",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (unbond): %v", err)
	}

	phoenixFolds, err := store.PhoenixStakeByUser(ctx, user)
	if err != nil {
		t.Fatalf("PhoenixStakeByUser: %v", err)
	}
	if len(phoenixFolds) != 1 || phoenixFolds[0].NetAmount != "350" {
		t.Errorf("PhoenixStakeByUser = %+v, want NetAmount=350 (500 bond - 150 unbond)", phoenixFolds)
	}

	// ─── defindex vault: vault-layer deposit df_tokens=800, withdraw
	// df_tokens=200 -> net 600. A strategy-layer row (actor = the
	// vault contract, NOT the user) must not leak into the user's fold.
	const vault = "CVAULT00000000000000000000000000000000000000000000000000"
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_000, LedgerCloseTime: t0, TxHash: pad64("s", 0), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerVault, Direction: timescale.DefindexDeposit,
		Actor: user, AmountsVec: []string{"800"}, DfTokens: "800",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (vault deposit): %v", err)
	}
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("s", 1), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerVault, Direction: timescale.DefindexWithdraw,
		Actor: user, AmountsVec: []string{"200"}, DfTokens: "200",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (vault withdraw): %v", err)
	}
	if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
		Ledger: 73_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("s", 2), OpIndex: 0, EventIndex: 0,
		ContractID: vault, Layer: timescale.DefindexLayerStrategy, Direction: timescale.DefindexDeposit,
		Actor: vault, Amount: "999999",
	}); err != nil {
		t.Fatalf("InsertDefindexFlow (strategy leg): %v", err)
	}

	defindexFolds, err := store.DefindexVaultSharesByUser(ctx, user)
	if err != nil {
		t.Fatalf("DefindexVaultSharesByUser: %v", err)
	}
	if len(defindexFolds) != 1 || defindexFolds[0].SharesNet != "600" {
		t.Errorf("DefindexVaultSharesByUser = %+v, want SharesNet=600 (800 deposit - 200 withdraw, strategy leg excluded)", defindexFolds)
	}

	// ─── sorocredit: open a position, publish a statement, and verify
	// the LATEST statement wins when a second, newer one lands. Also
	// open a SECOND position and mark it withdrawn.
	const collat1 = "CCOLLAT100000000000000000000000000000000000000000000000"
	const collat2 = "CCOLLAT200000000000000000000000000000000000000000000000"
	if err := store.InsertCreditPosition(ctx, timescale.CreditPosition{
		CollateralContract: collat1, PositionUUID: "uuid-1", PositionName: "Collateral-uuid-1", Owner: user,
		Ledger: 74_000_000, LedgerCloseTime: t0, TxHash: pad64("t", 0), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditPosition (1): %v", err)
	}
	if err := store.InsertCreditStatement(ctx, timescale.CreditStatement{
		StatementUUID: "stmt-1", PositionUUID: "uuid-1", CollateralContract: collat1,
		Amount: "500", StatementTime: t0.Add(time.Minute),
		Ledger: 74_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("t", 1), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditStatement (stmt-1): %v", err)
	}
	if err := store.InsertCreditStatement(ctx, timescale.CreditStatement{
		StatementUUID: "stmt-2", PositionUUID: "uuid-1", CollateralContract: collat1,
		Amount: "480", StatementTime: t0.Add(2 * time.Minute),
		Ledger: 74_000_002, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: pad64("t", 2), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditStatement (stmt-2, latest): %v", err)
	}
	if err := store.InsertCreditPosition(ctx, timescale.CreditPosition{
		CollateralContract: collat2, PositionUUID: "uuid-2", PositionName: "Collateral-uuid-2", Owner: user,
		Ledger: 74_000_003, LedgerCloseTime: t0.Add(3 * time.Minute), TxHash: pad64("t", 3), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditPosition (2): %v", err)
	}
	if err := store.InsertCreditEvent(ctx, timescale.CreditEvent{
		EventType: "withdrawal", CollateralContract: collat2, Asset: "CUSDC", Account: user, Amount: "50",
		Ledger: 74_000_004, LedgerCloseTime: t0.Add(4 * time.Minute), TxHash: pad64("t", 4), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditEvent (withdrawal): %v", err)
	}

	creditFolds, err := store.CreditPositionsByOwner(ctx, user)
	if err != nil {
		t.Fatalf("CreditPositionsByOwner: %v", err)
	}
	if len(creditFolds) != 2 {
		t.Fatalf("CreditPositionsByOwner rows = %d, want 2: %+v", len(creditFolds), creditFolds)
	}
	byContract := map[string]timescale.CreditPositionFold{}
	for _, f := range creditFolds {
		byContract[f.CollateralContract] = f
	}
	if got := byContract[collat1]; got.LatestAmount != "480" || got.Withdrawn {
		t.Errorf("credit position 1 = %+v, want LatestAmount=480 (the NEWER statement), Withdrawn=false", got)
	}
	if got := byContract[collat2]; !got.Withdrawn {
		t.Errorf("credit position 2 = %+v, want Withdrawn=true", got)
	}

	// ─── aquarius gauge: position_update delta +2000 then -700 -> net
	// 1300. A non-position_update kind (claim_reward) must not leak in.
	const gaugePool = "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT"

	if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
		ContractID: gaugePool, Ledger: 75_000_000, LedgerCloseTime: t0, TxHash: pad64("u", 0), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusRewardsPositionUpdate, UserAddress: user,
		Attributes: map[string]any{"range_from": 1, "range_to": 2, "delta": "2000"},
	}); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent (position_update +2000): %v", err)
	}
	if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
		ContractID: gaugePool, Ledger: 75_000_001, LedgerCloseTime: t0.Add(time.Minute), TxHash: pad64("u", 1), OpIndex: 0, EventIndex: 0,
		Kind: timescale.AquariusRewardsPositionUpdate, UserAddress: user,
		Attributes: map[string]any{"range_from": 1, "range_to": 2, "delta": "-700"},
	}); err != nil {
		t.Fatalf("InsertAquariusRewardsEvent (position_update -700): %v", err)
	}

	aquariusFolds, err := store.AquariusGaugeByUser(ctx, user)
	if err != nil {
		t.Fatalf("AquariusGaugeByUser: %v", err)
	}
	if len(aquariusFolds) != 1 || aquariusFolds[0].NetDelta != "1300" {
		t.Errorf("AquariusGaugeByUser = %+v, want NetDelta=1300 (2000 - 700)", aquariusFolds)
	}
}

// TestPositionsFold_VenueCapIsDeterministic gives one user more venues
// than the fold cap (500) in every protocol and pins which venues
// survive: the lowest 500 by venue key, in key order, on every fold.
func TestPositionsFold_VenueCapIsDeterministic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		user     = "GCAPUSERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		venueCap = 500
	)
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	venue := func(i int) string { return fmt.Sprintf("CVENUE%03d", i) }
	want := make([]string, venueCap)
	for i := range want {
		want[i] = venue(i)
	}

	// Descending insert order so physical row order never matches key order.
	for i := venueCap; i >= 0; i-- {
		ledger := uint32(80_000_000 + i) //nolint:gosec // small test index
		tx := fmt.Sprintf("%064x", i)
		ts := t0.Add(time.Duration(i) * time.Second)
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: venue(i), Kind: blend.EventSupply, Asset: "CASSET", User: user,
			TokenAmount: big.NewInt(1), BOrDAmount: big.NewInt(1),
			Ledger: ledger, TxHash: tx, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendPositionEvent %d: %v", i, err)
		}
		if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
			ContractID: "CBACKSTOP", Ledger: ledger, TxHash: tx, ObservedAt: ts,
			EventType: timescale.BackstopDeposit, Pool: venue(i), UserAddress: user,
			Amount: "1", Amount2: "1",
		}); err != nil {
			t.Fatalf("InsertBlendBackstopEvent %d: %v", i, err)
		}
		if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
			StakeContract: venue(i), Ledger: ledger, ObservedAt: ts, TxHash: tx,
			Action: timescale.PhoenixBond, User: user, LPToken: "CLPTOKEN", Amount: "1",
		}); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent %d: %v", i, err)
		}
		if err := store.InsertDefindexFlow(ctx, timescale.DefindexFlow{
			Ledger: ledger, LedgerCloseTime: ts, TxHash: tx,
			ContractID: venue(i), Layer: timescale.DefindexLayerVault, Direction: timescale.DefindexDeposit,
			Actor: user, AmountsVec: []string{"1"}, DfTokens: "1",
		}); err != nil {
			t.Fatalf("InsertDefindexFlow %d: %v", i, err)
		}
		if err := store.InsertCreditPosition(ctx, timescale.CreditPosition{
			CollateralContract: venue(i), PositionUUID: fmt.Sprintf("uuid-%03d", i), PositionName: "Collateral", Owner: user,
			Ledger: ledger, LedgerCloseTime: ts, TxHash: tx,
		}); err != nil {
			t.Fatalf("InsertCreditPosition %d: %v", i, err)
		}
		if err := store.InsertAquariusRewardsEvent(ctx, timescale.AquariusRewardsEvent{
			ContractID: venue(i), Ledger: ledger, LedgerCloseTime: ts, TxHash: tx,
			Kind: timescale.AquariusRewardsPositionUpdate, UserAddress: user,
			Attributes: map[string]any{"delta": "1"},
		}); err != nil {
			t.Fatalf("InsertAquariusRewardsEvent %d: %v", i, err)
		}
	}

	assertVenueCapFolds(ctx, t, "default plan", store, user, want)

	// A hash-aggregate plan emits groups in hash order, so the cap is only
	// deterministic if each fold sorts before its LIMIT.
	hashStore, err := timescale.Open(ctx, dsn+"&enable_sort=off")
	if err != nil {
		t.Fatalf("store open (enable_sort=off): %v", err)
	}
	t.Cleanup(func() { _ = hashStore.Close() })
	assertVenueCapFolds(ctx, t, "hash-aggregate plan", hashStore, user, want)
}

// assertVenueCapFolds asserts every positions fold returns exactly want,
// in order, for user.
func assertVenueCapFolds(ctx context.Context, t *testing.T, plan string, store *timescale.Store, user string, want []string) {
	t.Helper()
	check := func(label string, got []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s (%s): %v", label, plan, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s (%s) returned %d venues, want the lowest %d by key in key order; %s",
				label, plan, len(got), len(want), firstVenueDiff(got, want))
		}
	}
	keys := func(n int, key func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = key(i)
		}
		return out
	}

	blendFolds, err := store.BlendPositionsByUser(ctx, user)
	check("BlendPositionsByUser", keys(len(blendFolds), func(i int) string { return blendFolds[i].Pool }), err)
	backstopFolds, err := store.BlendBackstopSharesByUser(ctx, user)
	check("BlendBackstopSharesByUser", keys(len(backstopFolds), func(i int) string { return backstopFolds[i].Pool }), err)
	phoenixFolds, err := store.PhoenixStakeByUser(ctx, user)
	check("PhoenixStakeByUser", keys(len(phoenixFolds), func(i int) string { return phoenixFolds[i].StakeContract }), err)
	defindexFolds, err := store.DefindexVaultSharesByUser(ctx, user)
	check("DefindexVaultSharesByUser", keys(len(defindexFolds), func(i int) string { return defindexFolds[i].ContractID }), err)
	creditFolds, err := store.CreditPositionsByOwner(ctx, user)
	check("CreditPositionsByOwner", keys(len(creditFolds), func(i int) string { return creditFolds[i].CollateralContract }), err)
	aquariusFolds, err := store.AquariusGaugeByUser(ctx, user)
	check("AquariusGaugeByUser", keys(len(aquariusFolds), func(i int) string { return aquariusFolds[i].ContractID }), err)
}

// firstVenueDiff reports where got first diverges from want.
func firstVenueDiff(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("length: got %d want %d", len(got), len(want))
}

// assertBlendFlashLoanFolds pins the flash-loan debt leg through real
// SQL: a flash_loan settled by a repay in the same tx folds to a closed
// (zero) borrow, and a kept flash_loan is an open borrow — never a
// negative debt and never a missing leg.
func assertBlendFlashLoanFolds(ctx context.Context, t *testing.T, store *timescale.Store, pool string, t0 time.Time) {
	t.Helper()
	const (
		flashUser    = "GBFLASHUSERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		settledAsset = "CSETTLEDASSETAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		keptAsset    = "CKEPTASSETAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		receiver     = "CAXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXY32S"
	)
	rows := []struct {
		kind, asset string
		amount      int64
		eventIndex  uint32
		txSeed      string
	}{
		{blend.EventFlashLoan, settledAsset, 1000, 0, "f"},
		{blend.EventRepay, settledAsset, 1000, 1, "f"},
		{blend.EventFlashLoan, keptAsset, 500, 0, "k"},
	}
	for i, r := range rows {
		ev := domain.BlendPositionEvent{
			Pool: pool, Kind: r.kind, Asset: r.asset, User: flashUser,
			TokenAmount: big.NewInt(r.amount), BOrDAmount: big.NewInt(r.amount),
			Ledger: 70_100_000, TxHash: pad64(r.txSeed, 0), OpIndex: 0, EventIndex: r.eventIndex,
			Timestamp: t0.Add(time.Duration(i) * time.Second),
		}
		if r.kind == blend.EventFlashLoan {
			ev.Counterparty = receiver
		}
		if err := store.InsertBlendPositionEvent(ctx, ev); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s %s): %v", r.kind, r.asset, err)
		}
	}
	folds, err := store.BlendPositionsByUser(ctx, flashUser)
	if err != nil {
		t.Fatalf("BlendPositionsByUser (flash): %v", err)
	}
	want := map[string]string{settledAsset: "0", keptAsset: "500"}
	if len(folds) != len(want) {
		t.Fatalf("flash folds = %+v, want one per asset %v", folds, want)
	}
	for _, f := range folds {
		if !f.HasBorrowLeg || f.HasSupplyLeg || f.BorrowNet != want[f.Asset] {
			t.Errorf("flash fold %s = borrow:%v supply:%v net %q, want borrow leg only, net %q",
				f.Asset, f.HasBorrowLeg, f.HasSupplyLeg, f.BorrowNet, want[f.Asset])
		}
	}
}
