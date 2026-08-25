package pricingguard

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestScamFlagTagSet_MatchesFrontend pins the exact scam-tag set. It MUST
// equal DIRECTORY_SCAM_FLAG_TAGS in web/explorer/src/lib/directory-tags.ts
// — a gate/warning split (an asset shows a scam banner but keeps its
// price, or vice-versa) is the drift this pin prevents. If you change one
// list, change the other and both tests.
func TestScamFlagTagSet_MatchesFrontend(t *testing.T) {
	want := []string{"fraud", "hack", "malicious", "phishing", "scam", "unsafe"}
	var got []string
	for k := range scamFlagTagSet {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("scam tag set = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scam tag set = %v, want %v", got, want)
		}
	}
}

func TestIsDirectoryScamFlagged(t *testing.T) {
	cases := []struct {
		tags []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"unsafe"}, true},                // RIO's tag
		{[]string{"MALICIOUS"}, true},             // case-insensitive
		{[]string{" Scam "}, true},                // trimmed
		{[]string{"hack"}, true},                  // the tag the 4-set would have missed
		{[]string{"phishing"}, true},              // ditto
		{[]string{"memo-required", "kyc"}, false}, // benign directory tags
		{[]string{"deprecated"}, false},           // not in the scam set on its own
	}
	for _, c := range cases {
		if got := IsDirectoryScamFlagged(c.tags); got != c.want {
			t.Errorf("IsDirectoryScamFlagged(%v) = %v, want %v", c.tags, got, c.want)
		}
	}
}

type fakeDir struct {
	entry timescale.DirectoryEntry
	found bool
	err   error
	calls int
}

func (f *fakeDir) DirectoryEntryByAddress(_ context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	f.calls++
	return f.entry, f.found, f.err
}

func classic(code, issuer string) canonical.Asset {
	return canonical.Asset{Type: canonical.AssetClassic, Code: code, Issuer: issuer}
}

func TestScamGate_Withheld(t *testing.T) {
	ctx := context.Background()

	// nil gate withholds nothing.
	var nilGate *ScamGate
	if nilGate.Withheld(ctx, classic("RIO", "GBNL"), "price_read") {
		t.Error("nil gate must not withhold")
	}

	// scam-flagged issuer → withheld.
	fd := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"unsafe"}}, found: true}
	g := NewScamGate(fd, nil)
	if !g.Withheld(ctx, classic("RIO", "GBNL"), "price_read") {
		t.Error("flagged issuer must be withheld")
	}
	// second call hits the cache (no extra directory lookup).
	g.Withheld(ctx, classic("RIO", "GBNL"), "price_read")
	if fd.calls != 1 {
		t.Errorf("directory lookups = %d, want 1 (verdict cached)", fd.calls)
	}

	// clean issuer → not withheld.
	g2 := NewScamGate(&fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"kyc"}}, found: true}, nil)
	if g2.Withheld(ctx, classic("USDC", "GA5Z"), "price_read") {
		t.Error("clean issuer must not be withheld")
	}

	// unlisted issuer → not withheld.
	g3 := NewScamGate(&fakeDir{found: false}, nil)
	if g3.Withheld(ctx, classic("FOO", "GXXX"), "price_read") {
		t.Error("unlisted issuer must not be withheld")
	}

	// non-classic assets have no directory-flaggable issuer.
	gd := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"scam"}}, found: true}
	gg := NewScamGate(gd, nil)
	if gg.Withheld(ctx, canonical.Asset{Type: canonical.AssetNative}, "price_read") {
		t.Error("native asset cannot be scam-gated")
	}
	if gd.calls != 0 {
		t.Errorf("native asset triggered %d directory lookups, want 0", gd.calls)
	}

	// fail-OPEN: a directory error must NOT withhold, and must not cache.
	fe := &fakeDir{err: errors.New("db down")}
	ge := NewScamGate(fe, nil)
	if ge.Withheld(ctx, classic("RIO", "GBNL"), "price_read") {
		t.Error("directory error must fail OPEN (not withhold)")
	}
	ge.Withheld(ctx, classic("RIO", "GBNL"), "price_read")
	if fe.calls != 2 {
		t.Errorf("fail-open must not cache: lookups = %d, want 2", fe.calls)
	}
}
