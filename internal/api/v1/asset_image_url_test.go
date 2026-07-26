package v1

import "testing"

// TestIsSafeImageURL pins C1-031 (audit-2026-07-23). `image` comes
// verbatim from an issuer-controlled stellar.toml [[CURRENCIES]] entry.
// The pre-fix gate was a bare `http://` / `https://` prefix check, which
// let attribute-breakout characters through while the docstring promised
// a consumer rendering `<img src={data.image}>` "must be safe … this
// makes it so".
func TestIsSafeImageURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Ordinary issuer icons — must keep working.
		{"https", "https://example.com/icon.png", true},
		{"http", "http://example.com/icon.png", true},
		{"query_and_fragment", "https://cdn.example.com/i.png?v=2#x", true},
		{"percent_encoded_quote", "https://example.com/a%22b.png", true},

		// Scheme allow-list (pre-existing behaviour, kept).
		{"javascript", "javascript:alert(1)", false},
		{"data", "data:image/svg+xml;base64,AAAA", false},
		{"file", "file:///etc/passwd", false},
		{"blob", "blob:https://example.com/uuid", false},
		{"protocol_relative", "//example.com/icon.png", false},
		{"empty", "", false},

		// Attribute breakout — the C1-031 half. Each of these closes the
		// src attribute (or opens a tag) inside a naive non-escaping
		// template.
		{"double_quote", `https://example.com/a.png" onerror="alert(1)`, false},
		{"single_quote", "https://example.com/a.png' onerror='alert(1)", false},
		{"angle_open", "https://example.com/a.png<script>", false},
		{"angle_close", "https://example.com/a.png>", false},
		{"backtick", "https://example.com/a.png`x`", false},
		{"backslash", `https://example.com\a.png`, false},
		{"space", "https://example.com/a.png onerror=alert(1)", false},
		{"tab", "https://example.com/a.png\tonerror=x", false},
		{"newline", "https://example.com/a.png\nonerror=x", false},
		{"nul", "https://example.com/a.png\x00", false},
		{"del", "https://example.com/a.png\x7f", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSafeImageURL(tc.in); got != tc.want {
				t.Errorf("isSafeImageURL(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
