package v1

import (
	"testing"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/currency"
)

// sacFor derives the deterministic Stellar-Asset-Contract C-strkey for an
// asset id, the same address the token appears under in a pool's token set.
func sacFor(t *testing.T, a canonical.Asset) string {
	t.Helper()
	sac, err := a.SacContractID()
	if err != nil {
		t.Fatalf("SacContractID(%s): %v", a.String(), err)
	}
	return sac
}

func classicSAC(t *testing.T, assetID string) string {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", assetID, err)
	}
	return sacFor(t, a)
}

// The resolver maps a token contract to a human symbol off the verified
// catalogue (SAC → classic/native asset → ticker), and degrades an
// unresolvable token to a short truncated contract — never an error.
func TestTokenSymbolResolver(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	s := New(Options{VerifiedCurrencies: cat})
	r := s.newTokenSymbolResolver()

	xlmSAC := sacFor(t, canonical.NativeAsset())
	usdcSAC := classicSAC(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	aquaSAC := classicSAC(t, "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")

	const unresolvable = "CUNKNOWNTOKEN00000000000000000000000000000000000000000ZZ"

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"native XLM SAC", xlmSAC, "XLM"},
		{"USDC SAC → ticker", usdcSAC, "USDC"},
		{"AQUA SAC → ticker", aquaSAC, "AQUA"},
		{"unresolvable → truncated", unresolvable, truncContract(unresolvable)},
		{"empty token → empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.symbol(tc.token); got != tc.want {
				t.Fatalf("symbol(%q) = %q, want %q", tc.token, got, tc.want)
			}
		})
	}

	// The truncated fallback is the "CAS3…OWMA" shape (first 4 + last 4),
	// not the raw 56-char contract.
	if got := truncContract("CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"); got != "CAS3…OWMA" {
		t.Fatalf("truncContract = %q, want CAS3…OWMA", got)
	}
	if got := truncContract("SHORT"); got != "SHORT" {
		t.Fatalf("truncContract(short) = %q, want passthrough", got)
	}

	// Caching: a second lookup returns the memoised value.
	if r.cache[usdcSAC] != "USDC" {
		t.Fatalf("resolver did not cache USDC lookup: %v", r.cache[usdcSAC])
	}
}

// The resolver must not panic when the catalogue is absent (nil
// VerifiedCurrencies) — native still resolves, everything else falls back.
func TestTokenSymbolResolver_NoCatalogue(t *testing.T) {
	s := New(Options{})
	r := s.newTokenSymbolResolver()

	xlmSAC := sacFor(t, canonical.NativeAsset())
	if got := r.symbol(xlmSAC); got != "XLM" {
		t.Fatalf("native symbol without catalogue = %q, want XLM", got)
	}
	usdcSAC := classicSAC(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	// No catalogue → USDC SAC is unknown → truncated fallback (never a panic).
	if got := r.symbol(usdcSAC); got != truncContract(usdcSAC) {
		t.Fatalf("USDC symbol without catalogue = %q, want truncated fallback", got)
	}
}
