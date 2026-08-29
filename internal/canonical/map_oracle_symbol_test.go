package canonical

import (
	"errors"
	"testing"
)

// Oracle capture-totality PR-2: the shared symbol mapper every
// on-chain oracle decoder (Reflector, Band) routes bare symbols
// through. Known symbols map to their allow-listed type; anything
// else lands VERBATIM as raw:<symbol> instead of being dropped.
func TestMapOracleSymbol(t *testing.T) {
	cases := []struct {
		sym    string
		want   AssetType
		mapped bool
	}{
		{"USD", AssetFiat, true},
		{"EUR", AssetFiat, true},
		{"BTC", AssetCrypto, true},
		// 2026-08-29: the two reflector-fx slots that paged
		// stellarindex_ingestion_oracle_unknown_symbols on r1 v0.48.0
		// (raw:VES / raw:XAU). VES is fiat (ADR-0010); XAU is the spot
		// gold commodity and lands in the rwa: namespace (ADR-0028),
		// NOT fiat — TestIsKnownFiat_AllowList pins the exclusion.
		{"VES", AssetFiat, true},
		{"XAU", AssetRWA, true},
		{"NOTACOIN", AssetOracleRaw, false},
		{"EURC_ETHEREUM", AssetOracleRaw, false},
		// Case-sensitive: the allow-lists are upper-case codes, and a
		// raw row must hold the on-wire spelling verbatim.
		{"btc", AssetOracleRaw, false},
	}
	for _, tc := range cases {
		t.Run(tc.sym, func(t *testing.T) {
			a, err := MapOracleSymbol(tc.sym)
			if err != nil {
				t.Fatalf("MapOracleSymbol(%q): %v", tc.sym, err)
			}
			if a.Type != tc.want {
				t.Errorf("MapOracleSymbol(%q).Type = %q, want %q", tc.sym, a.Type, tc.want)
			}
			if a.Code != tc.sym {
				t.Errorf("MapOracleSymbol(%q).Code = %q, want verbatim symbol", tc.sym, a.Code)
			}
			if a.IsMapped() != tc.mapped {
				t.Errorf("MapOracleSymbol(%q).IsMapped() = %v, want %v", tc.sym, a.IsMapped(), tc.mapped)
			}
			if err := a.Validate(); err != nil {
				t.Errorf("MapOracleSymbol(%q) yielded an invalid asset: %v", tc.sym, err)
			}
		})
	}
}

func TestMapOracleSymbol_rwa(t *testing.T) {
	var code string
	for c := range knownRWACodes {
		code = c
		break
	}
	if code == "" {
		t.Skip("no RWA codes in allow-list")
	}
	a, err := MapOracleSymbol(code)
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != AssetRWA {
		t.Errorf("MapOracleSymbol(%q).Type = %q, want %q", code, a.Type, AssetRWA)
	}
}

// A symbol the raw validator refuses must surface as ErrInvalidAsset
// rather than a silently-empty asset: the decoders wrap and refuse the
// slot instead of persisting something unrepresentable.
func TestMapOracleSymbol_unrepresentableRefused(t *testing.T) {
	for _, sym := range []string{"", "has space", "tab\tin", "ünicode"} {
		if _, err := MapOracleSymbol(sym); !errors.Is(err, ErrInvalidAsset) {
			t.Errorf("MapOracleSymbol(%q) err = %v, want ErrInvalidAsset", sym, err)
		}
	}
}

// The fiat → crypto → rwa precedence in MapOracleSymbol is only
// unobservable while the three allow-lists share no code. Pin that so
// a future overlap is a deliberate decision, not a silent re-typing of
// an oracle row.
func TestMapOracleSymbol_allowListsDisjoint(t *testing.T) {
	for code := range knownCryptoCodes {
		if IsKnownFiat(code) {
			t.Errorf("%q is on both the fiat and crypto allow-lists", code)
		}
	}
	for code := range knownRWACodes {
		if IsKnownFiat(code) || IsKnownCrypto(code) {
			t.Errorf("%q is on the RWA allow-list and a fiat/crypto allow-list", code)
		}
	}
}
