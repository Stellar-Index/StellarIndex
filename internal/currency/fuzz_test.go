package currency

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fuzzAttacker is an issuer no generated entry ever carries, so any
// collision reported against it is an impersonation by construction.
const fuzzAttacker = "GBEO62ZYQXBGDQEHPTMBHRJVUEBNMXAWZFPBQBLPJXLJKMQTOEVEDGRA"

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func swapASCIICase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 32
		case 'A' <= c && c <= 'Z':
			b[i] = c + 32
		}
	}
	return string(b)
}

// checkCatalogueInvariants asserts the lookup contracts every loaded
// catalogue must satisfy, whatever its contents.
func checkCatalogueInvariants(t *testing.T, cat *Catalogue) {
	t.Helper()
	all := cat.All()
	var stellar, external, browse, cg int
	for i, vc := range all {
		if v, ok := cat.LookupBySlug(vc.Slug); !ok || v != vc {
			t.Fatalf("LookupBySlug(%q) does not return its own entry", vc.Slug)
		}
		if v, ok := cat.LookupByTicker(vc.Ticker); !ok || v != vc {
			t.Fatalf("LookupByTicker(%q) does not return its own entry", vc.Ticker)
		}
		if isASCII(vc.Slug) && isASCII(vc.Ticker) {
			if v, ok := cat.LookupBySlug(swapASCIICase(vc.Slug)); !ok || v != vc {
				t.Fatalf("LookupBySlug is not case-insensitive for %q", vc.Slug)
			}
			if v, ok := cat.LookupByTicker(swapASCIICase(vc.Ticker)); !ok || v != vc {
				t.Fatalf("LookupByTicker is not case-insensitive for %q", vc.Ticker)
			}
		}
		if got := cat.Tickers()[i]; got != strings.ToUpper(vc.Ticker) {
			t.Fatalf("Tickers()[%d] = %q, want %q", i, got, strings.ToUpper(vc.Ticker))
		}
		for _, n := range vc.Issuance {
			if n.Network != "stellar" {
				continue
			}
			if n.AssetID != "" {
				if v, ok := cat.LookupByStellarAssetID(n.AssetID); !ok || v != vc {
					t.Fatalf("LookupByStellarAssetID(%q) does not return its owner %q", n.AssetID, vc.Ticker)
				}
			}
			if n.Code == "" || n.Issuer == "" {
				continue
			}
			if v, coll := cat.StellarCollision(n.Code, n.Issuer); v != vc || coll {
				t.Fatalf("StellarCollision on the verified issuance %s-%s = (%v, %v), want (%q, false)", n.Code, n.Issuer, v, coll, vc.Ticker)
			}
			if isASCII(n.Code) {
				if v, coll := cat.StellarCollision(swapASCIICase(n.Code), n.Issuer); v != vc || coll {
					t.Fatalf("code match must be case-insensitive like the lookup: %q", n.Code)
				}
			}
			if v, coll := cat.StellarCollision(n.Code, fuzzAttacker); v != vc || !coll {
				t.Fatalf("an unverified issuer of %q must collide with %q, got (%v, %v)", n.Code, vc.Ticker, v, coll)
			}
		}

		fiatDen, isFiatDen := cat.FiatDenomination(vc.Ticker)
		switch {
		case vc.StellarEntry() != nil:
			stellar++
			// A Stellar identity is StellarCollision's, never a denomination.
			if isFiatDen && fiatDen == vc {
				t.Fatalf("%q has a Stellar issuance yet FiatDenomination answers for it", vc.Ticker)
			}
		case vc.Class == ClassFiat:
			external++
			if !isFiatDen || fiatDen != vc {
				t.Fatalf("fiat %q with no Stellar issuance is not answerable via FiatDenomination", vc.Ticker)
			}
		default:
			external++
			if v, coll := cat.StellarCollision(vc.Ticker, fuzzAttacker); v != vc || !coll {
				t.Fatalf("off-Stellar %q: an attacker's classic %q must collide, got (%v, %v)", vc.Ticker, vc.Ticker, v, coll)
			}
		}
		if !vc.ReferenceOnly {
			browse++
		}
		if vc.CoinGeckoID != "" {
			cg++
		}
		if !IsKnownClass(vc.Class) {
			t.Fatalf("%q loaded with unknown class %q", vc.Ticker, vc.Class)
		}
	}
	if len(cat.StellarIssued()) != stellar || len(cat.External()) != external {
		t.Fatalf("StellarIssued/External (%d/%d) do not partition All (%d/%d)",
			len(cat.StellarIssued()), len(cat.External()), stellar, external)
	}
	if len(cat.Browseable()) != browse {
		t.Fatalf("Browseable = %d, want %d (every non-reference entry)", len(cat.Browseable()), browse)
	}
	if len(cat.CoinGeckoIDs()) != cg {
		t.Fatalf("CoinGeckoIDs = %d, want %d", len(cat.CoinGeckoIDs()), cg)
	}
}

func TestCatalogueInvariants_Embedded(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	checkCatalogueInvariants(t, cat)
}

// FuzzLoadFromBytes: arbitrary YAML never panics the loader, and whatever
// it accepts satisfies every lookup invariant.
func FuzzLoadFromBytes(f *testing.F) {
	f.Add(seedYAML)
	f.Add([]byte("verified_currencies: []\n"))
	f.Add([]byte("verified_currencies:\n  - ticker: USD\n    slug: usd\n    name: Dollar\n    class: fiat\n"))
	f.Add([]byte("verified_currencies:\n  - ticker: XLM\n    slug: xlm\n    name: Lumen\n    networks:\n      - network: stellar\n        asset_id: native\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		cat, err := LoadFromBytes(b)
		if err != nil {
			return
		}
		checkCatalogueInvariants(t, cat)
	})
}

// fuzzEntry builds one raw entry from fuzz knobs. kind selects the shape of
// its issuance: 0 none, 1 classic, 2 native, 3 Soroban contract only,
// 4 a non-Stellar network.
func fuzzEntry(i int, ticker, code string, class, kind uint8, refOnly bool) rawCurrency {
	rc := rawCurrency{
		Ticker:        ticker,
		Slug:          strings.ToLower(ticker),
		Name:          ticker + " name",
		Class:         []string{"", "crypto", "stablecoin", "fiat", "bogus"}[int(class)%5],
		ReferenceOnly: refOnly,
		CoinGeckoID:   code,
	}
	issuer := []string{
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"GDQE7IXJ4HUHV6RQHIUPRJSEZE4DRS5WY577O2FY6YQ5LVWZ7JZTU2V5",
	}[i%2]
	switch kind % 5 {
	case 1:
		rc.Issuance = []rawIssuance{{Network: "stellar", Code: code, Issuer: issuer, AssetID: code + "-" + issuer}}
	case 2:
		rc.Issuance = []rawIssuance{{Network: "stellar", AssetID: "native"}}
	case 3:
		c := "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26Y" + string(rune('A'+i))
		rc.Issuance = []rawIssuance{{Network: "stellar", AssetID: c, Contract: c}}
	case 4:
		rc.Issuance = []rawIssuance{{Network: "Ethereum", Contract: "0xdeadbeef"}}
	}
	return rc
}

// FuzzCatalogueIndex drives the indexer with structured two-entry
// catalogues covering every class × issuance shape, including the
// collisions the loader must reject, and checks the lookup invariants on
// every catalogue it accepts.
func FuzzCatalogueIndex(f *testing.F) {
	f.Add("USDC", "USDC", uint8(2), uint8(1), false, "USD", "", uint8(3), uint8(0), false)
	f.Add("XLM", "", uint8(0), uint8(2), false, "XLM", "XLM", uint8(0), uint8(1), false)
	f.Add("BTC", "", uint8(0), uint8(0), true, "USDT", "USDT", uint8(2), uint8(1), false)
	f.Add("AQUA", "AQUA", uint8(1), uint8(1), false, "EUR", "", uint8(3), uint8(4), false)
	f.Add("usdc", "usdc", uint8(2), uint8(1), false, "USDC", "USDC", uint8(2), uint8(1), false)
	f.Add("TOK", "", uint8(1), uint8(3), false, "GBP", "", uint8(3), uint8(0), false)
	f.Fuzz(func(t *testing.T,
		t1, c1 string, cl1, k1 uint8, r1 bool,
		t2, c2 string, cl2, k2 uint8, r2 bool,
	) {
		raw := rawCatalogue{VerifiedCurrencies: []rawCurrency{
			fuzzEntry(0, t1, c1, cl1, k1, r1),
			fuzzEntry(1, t2, c2, cl2, k2, r2),
		}}
		b, err := yaml.Marshal(raw)
		if err != nil {
			return
		}
		cat, err := LoadFromBytes(b)
		if err != nil {
			return
		}
		if len(cat.All()) != 2 {
			t.Fatalf("loaded %d entries from a 2-entry catalogue", len(cat.All()))
		}
		checkCatalogueInvariants(t, cat)
	})
}
