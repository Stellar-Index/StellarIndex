package ingest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	testGenuineBenji      = "BENJI-GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
	testImpersonatorBenji = "BENJI-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// stubScanner serves pre-canned lake pages, keyset-paginated on the asset
// string exactly as the real scanner does, and records the cursors it was
// asked for so a resume can be asserted rather than assumed.
//
// It SORTS its own asset list because the real query is ORDER BY asset: a
// stub that emitted them in declaration order would let a walk that
// mishandles the cursor look correct.
type stubScanner struct {
	assets     []string
	askedAfter []string
	askedLimit []int
	err        error
}

func (s *stubScanner) TrustlineAssetsAfter(_ context.Context, after string, limit int) ([]clickhouse.TrustlineAssetSeed, error) {
	s.askedAfter = append(s.askedAfter, after)
	s.askedLimit = append(s.askedLimit, limit)
	if s.err != nil {
		return nil, s.err
	}
	sorted := slices.Clone(s.assets)
	slices.Sort(sorted)
	out := make([]clickhouse.TrustlineAssetSeed, 0, limit)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, a := range sorted {
		if a <= after {
			continue
		}
		out = append(out, clickhouse.TrustlineAssetSeed{
			Asset:       a,
			FirstLedger: 51_000_000,
			LastLedger:  51_990_000,
			FirstAt:     at,
			LastAt:      at.Add(time.Hour),
		})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// stubRegistryStore records what the walk handed the writer.
type stubRegistryStore struct {
	got   []timescale.ClassicAssetHolding
	stats timescale.ClassicAssetRegistryStats
	err   error
}

func (s *stubRegistryStore) RegisterClassicAssetsHeld(_ context.Context, obs []timescale.ClassicAssetHolding) (int64, int64, error) {
	if s.err != nil {
		return 0, 0, s.err
	}
	s.got = append(s.got, obs...)
	return int64(len(obs)), int64(len(obs)), nil
}

func (s *stubRegistryStore) ClassicAssetRegistryStats(context.Context) (timescale.ClassicAssetRegistryStats, error) {
	return s.stats, nil
}

func testOpts(out *bytes.Buffer) assetRegistryOpts {
	return assetRegistryOpts{
		configPath: "/etc/stellarindex.toml",
		chAddr:     "127.0.0.1:9300",
		page:       2,
		batch:      2,
		out:        out,
		// The pre-flight is skipped in dry-run and, in write mode, these
		// seams keep it off the filesystem.
		volumePath: func(context.Context, string) (string, error) { return "/var/lib/postgresql", nil },
		freeBytes:  func(string) (uint64, error) { return 1 << 40, nil },
	}
}

// TestAssetRegistryBackfill_RegistersHeldButNeverTradedAsset is the unit-level
// mirror of the integration regression test: an asset the lake holds a
// trustline for reaches the registry writer, keyed on (code, issuer).
func TestAssetRegistryBackfill_RegistersHeldButNeverTradedAsset(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	store := &stubRegistryStore{}
	scanner := &stubScanner{assets: []string{testGenuineBenji, testImpersonatorBenji}}

	if err := runAssetRegistryBackfill(context.Background(), store, scanner, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(store.got) != 2 {
		t.Fatalf("observations handed to the writer = %d, want 2", len(store.got))
	}
	seen := map[string]string{}
	for _, h := range store.got {
		if h.Asset.Code != "BENJI" {
			t.Errorf("code = %q, want BENJI", h.Asset.Code)
		}
		seen[h.Asset.String()] = h.Asset.Issuer
	}
	// Identity is (code, issuer). Two same-code assets must arrive as two
	// distinct registrations, never collapsed onto the code.
	if len(seen) != 2 {
		t.Fatalf("distinct assets registered = %d, want 2 — same-code assets were collapsed", len(seen))
	}
	if seen[testGenuineBenji] != "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5" {
		t.Errorf("genuine BENJI issuer = %q", seen[testGenuineBenji])
	}
	if !strings.Contains(out.String(), "walk COMPLETE") {
		t.Errorf("a walk that drained the lake did not report COMPLETE:\n%s", out.String())
	}
}

// TestAssetRegistryBackfill_SkipsAssetsWithNoIssuerIdentity — native and
// pool-share trustlines have no (code, issuer) and must be counted out, not
// guessed at. The SQL filters them; this proves the Go side refuses them
// too, so a lake that starts emitting a new asset spelling cannot smuggle a
// bogus row into the registry.
func TestAssetRegistryBackfill_SkipsAssetsWithNoIssuerIdentity(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.page = 8
	store := &stubRegistryStore{}
	scanner := &stubScanner{assets: []string{
		"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC", // Soroban contract id: parses, but is not classic
		testGenuineBenji,
		"native",                // parses; not classic
		"pool:0123456789abcdef", // pool-share trustline: no (code, issuer)
		"zzz-not-a-strkey",      // would-be classic form with a bad issuer
	}}

	if err := runAssetRegistryBackfill(context.Background(), store, scanner, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(store.got) != 1 || store.got[0].Asset.String() != testGenuineBenji {
		t.Fatalf("registered %d observation(s), want exactly the one classic asset; got %+v", len(store.got), store.got)
	}
	// scanned must equal registered + skipped_*: the summary is checkable
	// arithmetic, not a claim.
	summary := out.String()
	for _, want := range []string{"scanned=5", "registered=1", "skipped_non_classic=2", "skipped_unparsed=2"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

// TestAssetRegistryBackfill_ResumeReEntersAfterTheCursor pins that
// -resume-from is passed through as the FIRST page cursor: a resumed run
// must not re-walk the finished prefix.
func TestAssetRegistryBackfill_ResumeReEntersAfterTheCursor(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.resumeFrom = "AAA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	store := &stubRegistryStore{}
	scanner := &stubScanner{assets: []string{
		"AAA-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		testGenuineBenji,
	}}

	if err := runAssetRegistryBackfill(context.Background(), store, scanner, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(scanner.askedAfter) == 0 || scanner.askedAfter[0] != o.resumeFrom {
		t.Fatalf("first page cursor = %q, want the -resume-from value %q", scanner.askedAfter, o.resumeFrom)
	}
	if len(store.got) != 1 || store.got[0].Asset.String() != testGenuineBenji {
		t.Fatalf("resumed run re-walked the finished prefix: %+v", store.got)
	}
}

// TestAssetRegistryBackfill_LimitStopsEarlyAndPrintsResume — a bounded
// tranche must stop, must NOT claim completion, and must print the exact
// command that continues it.
func TestAssetRegistryBackfill_LimitStopsEarlyAndPrintsResume(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.page = 25
	o.limit = 1
	store := &stubRegistryStore{}
	scanner := &stubScanner{assets: []string{testGenuineBenji, testImpersonatorBenji}}

	if err := runAssetRegistryBackfill(context.Background(), store, scanner, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "walk COMPLETE") {
		t.Errorf("a -limit-bounded run claimed completion:\n%s", got)
	}
	if !strings.Contains(got, "RESUME: stellarindex-ops asset-registry-backfill") {
		t.Fatalf("no RESUME line after an early stop:\n%s", got)
	}
	// The last page is narrowed to what -limit still allows, so a tranche
	// of 1 scans 1 asset, not a whole 25-row page it then discards.
	if len(scanner.askedLimit) != 1 || scanner.askedLimit[0] != 1 {
		t.Errorf("page size(s) requested = %v, want exactly [1] (-limit must narrow the page)", scanner.askedLimit)
	}
	// The walk is in asset order, so the first page is the impersonator
	// (GA5Z… sorts before GBHN…) and that is the cursor to resume at.
	if !strings.Contains(got, "-resume-from "+testImpersonatorBenji) {
		t.Errorf("RESUME line does not carry the cursor it stopped at:\n%s", got)
	}
}

// TestAssetRegistryBackfill_DryRunWritesNothing — the write gate defaults to
// dry-run, and a dry run must reach the lake but never the writer.
func TestAssetRegistryBackfill_DryRunWritesNothing(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.dryRun = true
	store := &stubRegistryStore{}
	scanner := &stubScanner{assets: []string{testGenuineBenji}}

	if err := runAssetRegistryBackfill(context.Background(), store, scanner, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(store.got) != 0 {
		t.Fatalf("dry run wrote %d observation(s)", len(store.got))
	}
	if !strings.Contains(out.String(), "registered=1") {
		t.Errorf("dry run did not report what it would have registered:\n%s", out.String())
	}
}

// TestAssetRegistryBackfill_ErrorPrintsResumeAtTheFailedCursor — a failed
// run is resumable too, and the operator must not have to guess where.
func TestAssetRegistryBackfill_ErrorPrintsResumeAtTheFailedCursor(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	store := &stubRegistryStore{err: errors.New("connection reset")}
	scanner := &stubScanner{assets: []string{testGenuineBenji}}

	err := runAssetRegistryBackfill(context.Background(), store, scanner, o)
	if err == nil {
		t.Fatal("a store error must fail the run")
	}
	if !strings.Contains(out.String(), "-resume-from "+testGenuineBenji) {
		t.Errorf("failed run printed no usable RESUME line:\n%s", out.String())
	}
}

// TestAssetRegistryPreflight_RefusesWhenTheVolumeCannotHoldTheWrite —
// the guard must refuse before a long run starts, and say what it compared.
func TestAssetRegistryPreflight_RefusesWhenTheVolumeCannotHoldTheWrite(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.freeBytes = func(string) (uint64, error) { return 1 << 20, nil } // 1 MiB
	err := assetRegistryPreflight(context.Background(), o, timescale.ClassicAssetRegistryStats{Total: 199793})
	if err == nil {
		t.Fatal("pre-flight allowed a run onto a volume with 1 MiB free")
	}
	if !strings.Contains(err.Error(), "estimated need") {
		t.Errorf("refusal does not show its working: %v", err)
	}
}

// TestAssetRegistryPreflight_TrustsTheOverrideLoudly — an operator running
// off the database host asserts the figure, and the tool must say that
// nothing was measured rather than implying it was.
func TestAssetRegistryPreflight_TrustsTheOverrideLoudly(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(&out)
	o.minFreeBytes = 1 << 40
	o.freeBytes = func(string) (uint64, error) { t.Fatal("override must not measure"); return 0, nil }
	if err := assetRegistryPreflight(context.Background(), o, timescale.ClassicAssetRegistryStats{Total: 199793}); err != nil {
		t.Fatalf("pre-flight refused a 1 TiB assertion: %v", err)
	}
	if !strings.Contains(out.String(), "NOT measured") {
		t.Errorf("override did not warn that nothing was measured:\n%s", out.String())
	}
}
