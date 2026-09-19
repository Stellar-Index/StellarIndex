package scval

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// textCases is the shared matrix for the F052 text-safety contract:
// contract-chosen ScString bytes must become a value a Postgres `text`
// column accepts (valid UTF-8, no NUL) WITHOUT losing a byte.
var textCases = []struct {
	name string
	raw  string
	want string
}{
	// Ordinary values are byte-identical — a re-derive of an existing
	// row must write the same text it wrote before.
	{"empty", "", ""},
	{"ascii tag", "binance-deposit-tag-987654", "binance-deposit-tag-987654"},
	{"multibyte utf8", "commande-№42-é-日本", "commande-№42-é-日本"},
	{"max rune", "a\U0010FFFFb", "a\U0010FFFFb"},
	{"noncharacter U+FFFF", "a￿b", "a￿b"},
	{"backslash not at start", `a\x00`, `a\x00`},
	{"lone backslash", `\`, `\`},
	{"control bytes other than NUL", "a\x01\x1f\x7fb", "a\x01\x1f\x7fb"},

	// The two things Postgres refuses.
	{"NUL", "tag\x00tail", `\x` + "7461670074" + "61696c"},
	{"only NUL", "\x00", `\x00`},
	{"invalid byte 0xff", "\xff", `\xff`},
	{"lone continuation", "a\x80b", `\x618062`},
	{"truncated sequence", "ab\xe2\x82", `\x6162e282`},
	{"overlong NUL c0 80", "\xc0\x80", `\xc080`},
	{"encoded surrogate", "\xed\xa0\x80", `\xeda080`},
	{"above U+10FFFF", "\xf4\x90\x80\x80", `\xf4908080`},

	// A clean value that LOOKS encoded takes the hex arm, so no literal
	// value can collide with an encoded one.
	{"literal prefix only", `\x`, `\x5c78`},
	{"literal that mimics an encoding", `\x00`, `\x5c783030`},
}

func TestToText_MatrixAndRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range textCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ToText(tc.raw)
			if got != tc.want {
				t.Fatalf("ToText(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if !utf8.ValidString(got) || strings.Contains(got, "\x00") {
				t.Fatalf("ToText(%q) = %q is not text-column-safe", tc.raw, got)
			}
			back, err := FromText(got)
			if err != nil {
				t.Fatalf("FromText(%q): %v", got, err)
			}
			if string(back) != tc.raw {
				t.Fatalf("round trip lost bytes: %q -> %q -> %q", tc.raw, got, back)
			}
			if again := ToText(tc.raw); again != got {
				t.Fatalf("ToText is not deterministic: %q then %q", got, again)
			}
		})
	}
}

// TestToText_Injective is the property the prefix rule exists for: two
// different byte strings never share a stored value. The pair below
// collides under any scheme that passes clean values through and
// encodes dirty ones without also encoding prefix-bearing clean ones.
func TestToText_Injective(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string, len(textCases))
	for _, tc := range textCases {
		got := ToText(tc.raw)
		if prev, dup := seen[got]; dup && prev != tc.raw {
			t.Fatalf("ToText(%q) == ToText(%q) == %q", prev, tc.raw, got)
		}
		seen[got] = tc.raw
	}
	if ToText("\x00") == ToText(`\x00`) {
		t.Fatal(`a NUL byte and the four-character literal \x00 share a stored value`)
	}
}

func TestAsText(t *testing.T) {
	t.Parallel()
	raw := xdr.ScString("dep\x00osit\xff")
	got, err := AsText(xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &raw})
	if err != nil {
		t.Fatalf("AsText: %v", err)
	}
	if want := `\x646570006f736974ff`; got != want {
		t.Fatalf("AsText = %q, want %q", got, want)
	}

	// AsString is untouched: still the raw bytes, for callers that
	// compare rather than persist.
	if s, err := AsString(xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &raw}); err != nil || s != string(raw) {
		t.Fatalf("AsString = %q, %v; want the raw bytes", s, err)
	}

	sym := xdr.ScSymbol("memo")
	if _, err := AsText(xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}); !errors.Is(err, ErrScValType) {
		t.Fatalf("AsText(Symbol) err = %v, want ErrScValType", err)
	}
}

func TestFromText_RejectsForgedPrefix(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`\xzz`, `\x0`, `\x00 `} {
		if _, err := FromText(in); !errors.Is(err, ErrScValDecode) {
			t.Errorf("FromText(%q) err = %v, want ErrScValDecode", in, err)
		}
	}
}

func FuzzToTextRoundTrip(f *testing.F) {
	for _, tc := range textCases {
		f.Add(tc.raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := ToText(raw)
		if !utf8.ValidString(got) || strings.Contains(got, "\x00") {
			t.Fatalf("ToText(%q) = %q is not text-column-safe", raw, got)
		}
		back, err := FromText(got)
		if err != nil || string(back) != raw {
			t.Fatalf("round trip: %q -> %q -> %q (%v)", raw, got, back, err)
		}
	})
}
