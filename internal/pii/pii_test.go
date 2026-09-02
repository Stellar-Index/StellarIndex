package pii

import (
	"strings"
	"testing"
)

// TestMaskEmail is the single contract test for the redactor. It moved
// here from dashboardauth when the second, untested copy in
// internal/api/v1 was deleted (#346 F8) — that copy's doc comment
// claimed "the contract is pinned by a test in each package", which was
// not true, so a drift that started leaking addresses would have passed
// CI.
func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "a***@example.com",
		"a@b.co":            "***@b.co", // single-char local → fully hidden
		"bob.smith@x.io":    "b***@x.io",
		"":                  "",
		"garbage":           "***", // no @ → hide entirely
		"@nolocal.com":      "***", // empty local part (malformed) → hidden entirely
	}
	for in, want := range cases {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
		// The full local part must never survive in the output (PRV1).
		if at := strings.LastIndex(in, "@"); at > 1 {
			if rest := in[1:at]; strings.Contains(MaskEmail(in), rest) {
				t.Errorf("MaskEmail(%q) leaked the local part %q", in, rest)
			}
		}
	}
}

// TestMaskEmail_HidesRatherThanPassesThrough pins the DIRECTION of the
// failure mode. A redactor that echoes input it cannot parse is worse
// than no redactor, because the caller believes the value is safe.
func TestMaskEmail_HidesRatherThanPassesThrough(t *testing.T) {
	for _, in := range []string{
		"not-an-address",
		"weird@@double.com",
		"  spaced@example.com  ",
		"UPPER@EXAMPLE.COM",
		"unicodeé@example.com",
	} {
		got := MaskEmail(in)
		local := in
		if at := strings.LastIndex(in, "@"); at > 0 {
			local = in[:at]
		}
		if len(local) > 1 && strings.Contains(got, local) {
			t.Errorf("MaskEmail(%q) = %q — the whole local part survived", in, got)
		}
	}
}
