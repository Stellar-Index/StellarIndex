package timescale

import "testing"

const (
	sep1ImgCircle   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sep1ImgAttacker = "GAS4V4XZ3JHFTGKHCTMTWIIVFHGLBUMSQMGZM4RIWYPQHNXBAOGZBKHR"
)

// A SEP-1 payload may only declare images for the issuer that served
// it. The global image map is keyed on (code, issuer) taken from the
// TOML, so without this rule any Stellar account could publish
//
//	[[CURRENCIES]] code = "USDC" issuer = "<Circle's G-key>"
//	               image = "https://attacker.example/x.png"
//
// and take over the logo served for USDC on /v1/assets and the
// explorer homepage — a per-visitor beacon under a verified brand.
// Nothing upstream filters on org_verified, and projectCatalogueRows
// assigns the result unconditionally, so this check is the only thing
// standing between a hostile TOML and a curated asset's identity
// (cold audit 2026-08-03).
//
// The rule moved from whole-payload parsing to a per-row check when the
// scan was projected server-side (see [allSep1ImagesQuery]); the attack
// it must refuse did not move, so these cases did not change either.
// TestAllSep1ImagesProjection in test/integration re-runs this exact
// hijack through the real SQL.
func TestSep1ImageFrom_OnlyTheServingIssuerMayDeclareImages(t *testing.T) {
	t.Parallel()

	// The attacker's own row claims BOTH its own asset and Circle's.
	// Its own is kept; the one naming Circle must be refused.
	if _, ok := sep1ImageFrom(
		sep1ImgAttacker, "USDC", sep1ImgCircle, "https://attacker.example/usdc.png",
	); ok {
		t.Errorf("an attacker's payload declared an image for %s — brand hijack", sep1ImgCircle)
	}

	got, ok := sep1ImageFrom(
		sep1ImgAttacker, "SCAM", sep1ImgAttacker, "https://attacker.example/scam.png",
	)
	if !ok {
		t.Fatal("the attacker's own self-declaration should still be kept")
	}
	if got.Code != "SCAM" || got.Issuer != sep1ImgAttacker {
		t.Errorf("kept the wrong entry: %+v", got)
	}
}

// The legitimate issuer's own declaration still flows through.
func TestSep1ImageFrom_SelfDeclarationIsKept(t *testing.T) {
	t.Parallel()

	got, ok := sep1ImageFrom(sep1ImgCircle, "USDC", sep1ImgCircle, "https://circle.com/usdc.png")
	if !ok || got.Image != "https://circle.com/usdc.png" {
		t.Fatalf("got %+v ok=%v, want the issuer's own declaration preserved", got, ok)
	}
	// Issuer is stamped from the SERVING account, never from the TOML —
	// so a canonicalisable-but-differently-spelled declaration cannot
	// change the key the map is written under.
	if got.Issuer != sep1ImgCircle {
		t.Errorf("Issuer = %q, want the serving account %q", got.Issuer, sep1ImgCircle)
	}
}

// Incomplete entries yield nothing rather than erroring — one bad entry
// must not blank the whole map. Malformed JSON is no longer reachable
// here (the column is jsonb, and the projection hands over already-typed
// text), so the cases that remain are the ones a well-formed but useless
// [[CURRENCIES]] block produces.
func TestSep1ImageFrom_IncompleteEntriesAreSkipped(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ code, issuer, image string }{
		"no image":         {"USDC", sep1ImgCircle, ""},
		"no code":          {"", sep1ImgCircle, "https://x/y.png"},
		"blank code":       {"   ", sep1ImgCircle, "https://x/y.png"},
		"no issuer":        {"USDC", "", "https://x/y.png"},
		"issuer not a key": {"USDC", "not-a-strkey", "https://x/y.png"},
		"issuer too short": {"USDC", "GA5ZSEJY", "https://x/y.png"},
		"contract issuer":  {"USDC", "CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "https://x/y.png"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := sep1ImageFrom(sep1ImgCircle, tc.code, tc.issuer, tc.image); ok {
				t.Errorf("got %+v, want none", got)
			}
		})
	}
}
