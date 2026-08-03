package timescale

import "testing"

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
func TestSep1ImagesFromPayload_OnlyTheServingIssuerMayDeclareImages(t *testing.T) {
	t.Parallel()

	const (
		circle   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		attacker = "GAS4V4XZ3JHFTGKHCTMTWIIVFHGLBUMSQMGZM4RIWYPQHNXBAOGZBKHR"
	)

	// The attacker's own row: it claims BOTH its own asset and Circle's.
	payload := `{"Currencies":[
		{"Code":"SCAM","Issuer":"` + attacker + `","Image":"https://attacker.example/scam.png"},
		{"Code":"USDC","Issuer":"` + circle + `","Image":"https://attacker.example/usdc.png"}
	]}`

	got := sep1ImagesFromPayload(attacker, payload)

	if len(got) != 1 {
		t.Fatalf("got %d images %+v, want exactly 1 — the entry claiming another issuer must be dropped", len(got), got)
	}
	if got[0].Code != "SCAM" || got[0].Issuer != attacker {
		t.Errorf("kept the wrong entry: %+v", got[0])
	}
	for _, img := range got {
		if img.Issuer == circle {
			t.Errorf("an attacker's payload declared an image for %s — brand hijack", circle)
		}
	}
}

// The legitimate issuer's own declaration still flows through.
func TestSep1ImagesFromPayload_SelfDeclarationIsKept(t *testing.T) {
	t.Parallel()

	const circle = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	payload := `{"Currencies":[{"Code":"USDC","Issuer":"` + circle + `","Image":"https://circle.com/usdc.png"}]}`

	got := sep1ImagesFromPayload(circle, payload)
	if len(got) != 1 || got[0].Image != "https://circle.com/usdc.png" {
		t.Fatalf("got %+v, want the issuer's own declaration preserved", got)
	}
}

// Corrupt or incomplete payloads yield nothing rather than erroring —
// one bad row must not blank the whole map.
func TestSep1ImagesFromPayload_MalformedAndIncompleteAreSkipped(t *testing.T) {
	t.Parallel()

	const g = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for name, payload := range map[string]string{
		"not json":      `{"Currencies":[`,
		"empty":         ``,
		"no image":      `{"Currencies":[{"Code":"USDC","Issuer":"` + g + `"}]}`,
		"no code":       `{"Currencies":[{"Issuer":"` + g + `","Image":"https://x/y.png"}]}`,
		"no issuer":     `{"Currencies":[{"Code":"USDC","Image":"https://x/y.png"}]}`,
		"no currencies": `{"OrgName":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := sep1ImagesFromPayload(g, payload); len(got) != 0 {
				t.Errorf("got %+v, want none", got)
			}
		})
	}
}
