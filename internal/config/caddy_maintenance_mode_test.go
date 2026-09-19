package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── The serving kill-switch (audit-2026-09-02 F137 / Q239) ─────────
//
// `sudo touch /etc/caddy/MAINTENANCE_MODE` is the documented way to
// stop serving during a live data-integrity incident
// (docs/operations/runbooks/api-down.md). It answered 503 on
// /v1/price — and kept streaming the wrong prices on
// /v1/price/stream, /v1/price/tip/stream, /v1/observations/stream and
// /v1/ledger/stream, with no signal that the switch was partial.
//
// The cause is not visible in the source order of the Caddyfile:
// OUTSIDE a `route` block Caddy sorts a site's directives by its own
// fixed directive order, in which `handle` ranks ABOVE `respond`. With
// the stream proxy in a bare `handle @sse { … }` block, the compiled
// srv0 subroute came out
//
//	[vars+headers, encode, sse-handle, maintenance-503, /metrics, proxy]
//
// so a stream request was proxied and returned before the 503 was ever
// reached. Measured, not reasoned: `caddy adapt` on the pre-fix file
// produces exactly that order, and real caddy 2.11.4 in front of a
// stub SSE upstream answered 200 + event data on all four stream
// routes with MAINTENANCE_MODE present.
//
// `route` is the one construct that makes source order authoritative,
// so the kill-switch and the stream proxy now live in ONE `route`
// block with the 503 first. These tests pin that in both copies of the
// file — the hand-kept configs/caddy/Caddyfile.api and the ansible
// template that is what actually reaches the host — because a fix in
// only one of them is cosmetic.
//
// Two properties, both directions:
//   - switch ENGAGED: nothing, stream routes included, gets past the 503;
//   - switch DISENGAGED: every stream route still proxies with
//     `flush_interval -1`, i.e. the fix must not buy the kill-switch by
//     breaking streaming (r1 2026-08-03: buffered SSE = zero bytes in
//     25 s).

// caddyDirective is one non-comment line of a Caddyfile together with
// the brace depth its content sits at: depth 1 is a site-block
// directive, depth 2 is inside a `route` / `handle` block, and so on.
type caddyDirective struct {
	line  int
	depth int
	text  string
}

// caddyDirectives strips comments and blank lines and annotates what is
// left with its brace depth. Caddyfiles are formatted one directive per
// line by `caddy fmt`, a block opens with a trailing `{` and closes
// with a line that is nothing but `}` — placeholders such as
// `{client_ip}` never sit at either end of a line, so they do not
// disturb the count.
func caddyDirectives(t *testing.T, path string) []caddyDirective {
	t.Helper()
	var out []caddyDirective
	depth := 0
	for i, raw := range strings.Split(readCaddyfile(t, path), "\n") {
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if text == "}" {
			depth--
		}
		out = append(out, caddyDirective{line: i + 1, depth: depth, text: text})
		if strings.HasSuffix(text, "{") {
			depth++
		}
	}
	if depth != 0 {
		t.Fatalf("%s: unbalanced braces (ended at depth %d)", path, depth)
	}
	return out
}

// TestCaddyKillSwitchOutranksTheStreamProxy is the ordering property
// itself, asserted on the text so it holds wherever the suite runs
// (TestCaddyKillSwitchCompiledOrder below proves the same thing through
// caddy itself when the binary is on PATH).
//
// The assertion is deliberately structural rather than "line 190 is
// before line 218": file order is exactly what does NOT decide this.
// What decides it is that both directives sit in one `route` block,
// maintenance first.
func TestCaddyKillSwitchOutranksTheStreamProxy(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			dirs := caddyDirectives(t, path)

			// A `handle` block anywhere in the site outranks `respond`
			// and reintroduces the defect, whatever the source order
			// says. `handle_path` has the same rank.
			for _, d := range dirs {
				if strings.HasPrefix(d.text, "handle ") || strings.HasPrefix(d.text, "handle_path ") ||
					d.text == "handle {" {
					t.Errorf("%s:%d: `%s` — a handle block ranks ABOVE `respond` in Caddy's directive order, "+
						"so the MAINTENANCE_MODE 503 no longer covers what it wraps; put it in the route block instead",
						path, d.line, d.text)
				}
			}

			maint, sse := -1, -1
			routeDepth := 0
			for i, d := range dirs {
				switch {
				case d.text == "route {":
					routeDepth = d.depth + 1
				case strings.HasPrefix(d.text, "respond @maintenance ") && d.depth == routeDepth:
					maint = i
				case strings.HasPrefix(d.text, "reverse_proxy @sse ") && d.depth == routeDepth:
					sse = i
				}
			}
			if routeDepth == 0 {
				t.Fatalf("%s: no `route` block — outside one, Caddy ignores source order and the "+
					"stream proxy wins over the kill-switch", path)
			}
			if maint < 0 {
				t.Fatalf("%s: the `respond @maintenance … 503` is not inside the route block, so its "+
					"position relative to the stream proxy is decided by Caddy's directive order, not by this file", path)
			}
			if sse < 0 {
				t.Fatalf("%s: no `reverse_proxy @sse …` inside the route block — the stream proxy must sit "+
					"in the same route as the kill-switch for the 503 to precede it", path)
			}
			if maint > sse {
				t.Errorf("%s: the stream proxy (line %d) runs BEFORE the MAINTENANCE_MODE 503 (line %d) — "+
					"inside a route block the first match wins, so every /v1/*/stream request is proxied "+
					"while the operator believes serving is stopped", path, dirs[sse].line, dirs[maint].line)
			}

			// The other direction: with the switch disengaged the
			// stream must still stream. `flush_interval -1` is what
			// keeps Caddy from buffering an event stream to death.
			if !strings.Contains(caddyStreamProxyBlock(t, path), "flush_interval -1") {
				t.Errorf("%s: the stream proxy lost `flush_interval -1` — SSE consumers get a 200, the right "+
					"Content-Type and then nothing", path)
			}
		})
	}
}

// caddyStreamProxyBlock returns the `reverse_proxy @sse … { … }` stanza
// verbatim. It opens at two tabs (inside the route block) so the first
// line that is exactly two tabs and a closing brace ends it.
func caddyStreamProxyBlock(t *testing.T, path string) string {
	t.Helper()
	src := readCaddyfile(t, path)
	start := strings.Index(src, "\t\treverse_proxy @sse ")
	if start < 0 {
		t.Fatalf("%s: no `reverse_proxy @sse …` stanza inside the route block", path)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n\t\t}\n")
	if end < 0 {
		t.Fatalf("%s: stream proxy stanza is not closed", path)
	}
	return rest[:end]
}

// ─── The same property, proved through caddy ────────────────────────
//
// Caddy's directive order is precisely the thing a careful human reader
// gets wrong — that is how this shipped — so when the binary is
// available the compiled config is the witness, not the text.

// caddyCompiledRoute is one route of an adapted config, flattened into
// evaluation order (a subroute's children run where the subroute sits).
type caddyCompiledRoute struct {
	match   string
	handler string
	status  float64
	flush   float64
}

// caddyAdapt runs `caddy adapt` over a Caddyfile and returns srv0's
// routes flattened into evaluation order. The ansible template's one
// jinja placeholder is rendered first so the adapter can parse it.
func caddyAdapt(t *testing.T, caddyBin, path string) []caddyCompiledRoute {
	t.Helper()
	src := strings.ReplaceAll(readCaddyfile(t, path), "{{ caddy_site_addresses }}", "api.stellarindex.io")
	rendered := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(rendered, []byte(src), 0o600); err != nil {
		t.Fatalf("write rendered caddyfile: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), caddyBin, "adapt", "--config", rendered, "--adapter", "caddyfile")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("caddy adapt %s: %v", path, err)
	}
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("parse adapted config for %s: %v", path, err)
	}
	var flat []caddyCompiledRoute
	for _, srv := range cfg.Apps.HTTP.Servers {
		flat = append(flat, caddyFlatten(t, srv.Routes, "")...)
	}
	return flat
}

// caddyFlatten walks adapted routes depth-first, which is the order
// Caddy evaluates them in. A `handle` block compiles to a subroute
// carrying the matcher with its children matchless underneath, so the
// enclosing match is carried down — otherwise the handler that the
// matcher actually guards looks unconditional.
func caddyFlatten(t *testing.T, routes []json.RawMessage, inherited string) []caddyCompiledRoute {
	t.Helper()
	var flat []caddyCompiledRoute
	for _, raw := range routes {
		var r struct {
			Match  json.RawMessage `json:"match"`
			Handle []struct {
				Handler       string            `json:"handler"`
				StatusCode    float64           `json:"status_code"`
				FlushInterval float64           `json:"flush_interval"`
				Routes        []json.RawMessage `json:"routes"`
			} `json:"handle"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("parse adapted route: %v", err)
		}
		match := string(r.Match)
		if match == "" || match == "null" {
			match = inherited
		}
		for _, h := range r.Handle {
			flat = append(flat, caddyCompiledRoute{
				match:   match,
				handler: h.Handler,
				status:  h.StatusCode,
				flush:   h.FlushInterval,
			})
			if h.Handler == "subroute" {
				flat = append(flat, caddyFlatten(t, h.Routes, match)...)
			}
		}
	}
	return flat
}

// TestCaddyKillSwitchCompiledOrder asks caddy what it will actually do.
// On the pre-fix file the stream subroute compiles ahead of the
// maintenance 503; this fails there and passes on the route-block form.
func TestCaddyKillSwitchCompiledOrder(t *testing.T) {
	caddyBin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skipf("caddy binary not on PATH (%v) — TestCaddyKillSwitchOutranksTheStreamProxy "+
			"pins the same property on the text", err)
	}

	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			maint, sse := -1, -1
			routes := caddyAdapt(t, caddyBin, path)
			for i, r := range routes {
				switch {
				case r.handler == "static_response" && r.status == 503 &&
					strings.Contains(r.match, "MAINTENANCE_MODE"):
					if maint < 0 {
						maint = i
					}
				case r.handler == "reverse_proxy" && strings.Contains(r.match, `^/v1/.*/stream$`):
					if sse < 0 {
						sse = i
					}
					if r.flush != -1 {
						t.Errorf("%s: compiled stream proxy has flush_interval %v, want -1 — "+
							"SSE responses would be buffered", path, r.flush)
					}
				}
			}
			if maint < 0 {
				t.Fatalf("%s: adapted config has no MAINTENANCE_MODE 503 responder:\n%s", path, caddyDump(routes))
			}
			if sse < 0 {
				t.Fatalf("%s: adapted config has no /v1/*/stream reverse_proxy — the stream routes lost their "+
					"flush settings:\n%s", path, caddyDump(routes))
			}
			if maint > sse {
				t.Errorf("%s: caddy compiles the stream proxy (position %d) AHEAD of the MAINTENANCE_MODE 503 "+
					"(position %d) — the kill-switch does not stop the SSE streams:\n%s",
					path, sse, maint, caddyDump(routes))
			}
		})
	}
}

func caddyDump(routes []caddyCompiledRoute) string {
	var b strings.Builder
	for i, r := range routes {
		fmt.Fprintf(&b, "  [%d] %s match=%s\n", i, r.handler, r.match)
	}
	return b.String()
}
