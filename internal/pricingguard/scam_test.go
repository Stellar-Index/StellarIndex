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
	g := NewScamGate(fd, ScamGateOptions{})
	if !g.Withheld(ctx, classic("RIO", "GBNL"), "price_read") {
		t.Error("flagged issuer must be withheld")
	}
	// second call hits the cache (no extra directory lookup).
	g.Withheld(ctx, classic("RIO", "GBNL"), "price_read")
	if fd.calls != 1 {
		t.Errorf("directory lookups = %d, want 1 (verdict cached)", fd.calls)
	}

	// clean issuer → not withheld.
	g2 := NewScamGate(&fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"kyc"}}, found: true}, ScamGateOptions{})
	if g2.Withheld(ctx, classic("USDC", "GA5Z"), "price_read") {
		t.Error("clean issuer must not be withheld")
	}

	// unlisted issuer → not withheld.
	g3 := NewScamGate(&fakeDir{found: false}, ScamGateOptions{})
	if g3.Withheld(ctx, classic("FOO", "GXXX"), "price_read") {
		t.Error("unlisted issuer must not be withheld")
	}

	// non-classic assets have no directory-flaggable issuer.
	gd := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"scam"}}, found: true}
	gg := NewScamGate(gd, ScamGateOptions{})
	if gg.Withheld(ctx, canonical.Asset{Type: canonical.AssetNative}, "price_read") {
		t.Error("native asset cannot be scam-gated")
	}
	if gd.calls != 0 {
		t.Errorf("native asset triggered %d directory lookups, want 0", gd.calls)
	}

	// fail-OPEN: a directory error must NOT withhold, and must not cache.
	fe := &fakeDir{err: errors.New("db down")}
	ge := NewScamGate(fe, ScamGateOptions{})
	if ge.Withheld(ctx, classic("RIO", "GBNL"), "price_read") {
		t.Error("directory error must fail OPEN (not withhold)")
	}
	ge.Withheld(ctx, classic("RIO", "GBNL"), "price_read")
	if fe.calls != 2 {
		t.Errorf("fail-open must not cache: lookups = %d, want 2", fe.calls)
	}
}

// TestScamGate_WithheldThroughSACSpelling pins the identity half of the
// gate: a Stellar Asset Contract wrapper IS the classic issuance it
// wraps, so naming the wrapper's C-address must reach the same verdict
// as naming `CODE-ISSUER`.
//
// Before the base was resolved to its canonical family form, the
// classic-only guard rejected the SAC spelling outright — the gate
// returned false without ever asking the directory, and the flagged
// issuer's price stayed servable to any caller who spelled the asset as
// its contract id (R8).
//
// NOT parallel: the alias registry is process-global (same convention as
// TestTVLValuer_GateSeesTheCanonicalIdentity).
func TestScamGate_WithheldThroughSACSpelling(t *testing.T) {
	ctx := context.Background()
	const (
		code   = "RIO"
		issuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	flagged, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sacID, err := flagged.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(sacID)
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(map[string]string{sacID: code + ":" + issuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	fd := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"unsafe"}}, found: true}
	g := NewScamGate(fd, ScamGateOptions{})

	if !g.Withheld(ctx, sac, "vwap") {
		t.Error("the SAC wrapper of a flagged classic issuance must be withheld — " +
			"it is the same asset, and the C-address spelling is otherwise a way " +
			"to ask for a price the gate has already refused")
	}
	if fd.calls != 1 {
		t.Errorf("directory lookups = %d, want 1 — the SAC spelling must resolve to "+
			"the classic issuer and ASK, not short-circuit to false", fd.calls)
	}
	// The classic spelling is unchanged, and shares the resolved verdict
	// cache rather than re-asking under a second key.
	if !g.Withheld(ctx, flagged, "price_read") {
		t.Error("the classic spelling must still be withheld")
	}
	if fd.calls != 1 {
		t.Errorf("directory lookups = %d, want 1 — both spellings resolve to one "+
			"issuer key and must share one cached verdict", fd.calls)
	}

	// Blast radius: a wrapper whose classic twin is NOT flagged serves.
	clean := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"kyc"}}, found: true}
	if NewScamGate(clean, ScamGateOptions{}).Withheld(ctx, sac, "vwap") {
		t.Error("a SAC whose classic twin is unflagged must not be withheld")
	}
}

// TestScamGate_BareSorobanHasNoFlaggableIssuer is the other half of the
// same rule. A pure-SEP-41 contract with no classic twin has no
// G-address anywhere in its identity, so the account directory cannot
// speak to it: the gate must return false WITHOUT a lookup, exactly as
// it did before the canonical resolution was added. Resolving an asset
// that is in no family returns the asset itself, so the classic guard
// still rejects it.
func TestScamGate_BareSorobanHasNoFlaggableIssuer(t *testing.T) {
	ctx := context.Background()
	// A registry is installed and knows a DIFFERENT wrapper, so the miss
	// is a genuine "no family", not an empty registry.
	other, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	otherSAC, err := other.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(map[string]string{
		otherSAC: "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
	})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	bare, err := canonical.NewSorobanAsset("CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX")
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	fd := &fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"scam"}}, found: true}
	g := NewScamGate(fd, ScamGateOptions{})
	if g.Withheld(ctx, bare, "price_read") {
		t.Error("a Soroban asset with no classic twin has no directory-flaggable " +
			"issuer — the gate cannot speak to it and must not withhold")
	}
	if fd.calls != 0 {
		t.Errorf("bare Soroban asset triggered %d directory lookups, want 0", fd.calls)
	}

	// XLM's SAC canonicalises to `native`, which likewise carries no
	// issuer: unchanged, and still no lookup.
	xlmSAC, err := canonical.NewSorobanAsset(canonical.XLMSacContractID)
	if err != nil {
		t.Fatalf("xlm sac: %v", err)
	}
	if g.Withheld(ctx, xlmSAC, "price_read") {
		t.Error("XLM's SAC canonicalises to native, which has no issuer to flag")
	}
	if fd.calls != 0 {
		t.Errorf("XLM SAC triggered %d directory lookups, want 0", fd.calls)
	}
}
