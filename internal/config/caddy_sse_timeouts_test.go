package config_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// ─── Proxy timeouts vs the SSE heartbeat (GH #886) ──────────────────
//
// An idle SSE stream stays alive only because the API writes a
// `:keepalive` comment every streaming.DefaultHeartbeatInterval. That
// works only while every bound on the stream path outlasts a heartbeat
// gap, and nothing checked that: a `read_timeout` on the stream proxy's
// transport shorter than the heartbeat, or any server `write` timeout
// or `stream_timeout` (both absolute caps, not idle bounds), would cut
// every quiet stream on a schedule while passing `caddy validate`.
//
// The rule, applied to the stream path in both Caddyfiles:
//   - absolute caps (server write timeout, stream_timeout): forbidden;
//   - per-read bounds (transport read/write/response-header timeouts):
//     unset, or at least caddySSEMinIdleBound — two heartbeats, so one
//     late heartbeat does not end the stream.
// Server read_body/read_header/idle timeouts are left alone: they bound
// the request and the gap between requests, not a response in flight.

// caddySSEMinIdleBound is the shortest per-read timeout the stream path
// may carry.
const caddySSEMinIdleBound = 2 * streaming.DefaultHeartbeatInterval

// caddySSEAbsoluteCaps are timeouts that end a response at a fixed age.
var caddySSEAbsoluteCaps = map[string]bool{"stream_timeout": true, "timeouts": true, "timeouts>write": true}

// caddySSEIdleBounds are per-read timeouts a heartbeat resets.
var caddySSEIdleBounds = map[string]bool{"read_timeout": true, "write_timeout": true, "response_header_timeout": true}

// caddySSEBoundViolation returns why a timeout on the stream path breaks
// SSE, or "" when it is compatible with the heartbeat.
func caddySSEBoundViolation(name string, d time.Duration) string {
	switch {
	case caddySSEAbsoluteCaps[name] && d > 0:
		return "an absolute cap: it ends every stream at that age however regularly it heartbeats"
	case caddySSEIdleBounds[name] && d > 0 && d < caddySSEMinIdleBound:
		return "shorter than two " + streaming.DefaultHeartbeatInterval.String() +
			" heartbeats, so a quiet stream is cut between keepalives"
	}
	return ""
}

type caddyBound struct {
	line  int
	name  string
	value string
}

// caddySSETextBounds lists the timeouts in a Caddyfile that a live
// stream can hit: every server `timeouts` setting and every timeout
// inside the `reverse_proxy @sse` stanza (its transport included).
func caddySSETextBounds(src string) []caddyBound {
	var out []caddyBound
	depth, timeoutsDepth, sseDepth := 0, -1, -1
	for i, raw := range strings.Split(src, "\n") {
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if text == "}" {
			depth--
			if depth == timeoutsDepth {
				timeoutsDepth = -1
			}
			if depth == sseDepth {
				sseDepth = -1
			}
			continue
		}
		fields := strings.Fields(text)
		if name, ok := caddySSEBoundName(fields, timeoutsDepth >= 0, sseDepth >= 0); ok {
			out = append(out, caddyBound{i + 1, name, fields[1]})
		}
		if strings.HasSuffix(text, "{") {
			switch {
			case fields[0] == "timeouts":
				timeoutsDepth = depth
			case strings.HasPrefix(text, "reverse_proxy @sse "):
				sseDepth = depth
			}
			depth++
		}
	}
	return out
}

// caddySSEBoundName names the stream-path timeout a `name value` line
// sets, given whether it sits in a server `timeouts` block or in the
// stream proxy stanza.
func caddySSEBoundName(fields []string, inTimeouts, inSSE bool) (string, bool) {
	if len(fields) != 2 || fields[1] == "{" {
		return "", false
	}
	switch {
	case fields[0] == "timeouts":
		return "timeouts", true
	case inTimeouts:
		return "timeouts>" + fields[0], true
	case inSSE && (caddySSEAbsoluteCaps[fields[0]] || caddySSEIdleBounds[fields[0]]):
		return fields[0], true
	}
	return "", false
}

// caddySSETextViolations applies the rule to every bound in src.
func caddySSETextViolations(src string) []string {
	var out []string
	for _, b := range caddySSETextBounds(src) {
		d, err := time.ParseDuration(b.value)
		if err != nil {
			out = append(out, b.name+" "+b.value+": not a Go duration this test can check — extend the parser")
			continue
		}
		if why := caddySSEBoundViolation(b.name, d); why != "" {
			out = append(out, b.name+" "+b.value+" (line "+strconv.Itoa(b.line)+"): "+why)
		}
	}
	return out
}

// TestCaddySSETimeoutsOutlastTheHeartbeat is the text-level pin; it
// runs whether or not caddy is installed.
func TestCaddySSETimeoutsOutlastTheHeartbeat(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			for _, v := range caddySSETextViolations(readCaddyfile(t, path)) {
				t.Errorf("%s: %s", path, v)
			}
		})
	}
}

// TestCaddySSETimeoutPinIsNotVacuous feeds the real files, each with a
// stream-breaking timeout spliced in, through the same check: a
// checker that cannot see the SSE stanza or the server block would
// otherwise pass the real files trivially.
func TestCaddySSETimeoutPinIsNotVacuous(t *testing.T) {
	mutants := map[string]struct{ from, to string }{
		"short transport read_timeout": {"flush_interval -1\n", "flush_interval -1\n\t\t\ttransport http {\n\t\t\t\tread_timeout 10s\n\t\t\t}\n"},
		"stream_timeout":               {"flush_interval -1\n", "flush_interval -1\n\t\t\tstream_timeout 1h\n"},
		"server write timeout":         {"\tservers {\n", "\tservers {\n\t\ttimeouts {\n\t\t\twrite 5m\n\t\t}\n"},
	}
	for _, path := range caddyfiles {
		src := readCaddyfile(t, path)
		for name, m := range mutants {
			if !strings.Contains(src, m.from) {
				t.Fatalf("%s: mutation anchor %q not found — update the mutant", path, m.from)
			}
			if got := caddySSETextViolations(strings.Replace(src, m.from, m.to, 1)); len(got) != 1 {
				t.Errorf("%s with %s: got violations %q, want exactly one", filepath.Base(path), name, got)
			}
		}
	}
}

// TestCaddySSETimeoutsCompiled asks caddy for the timeouts it will
// actually apply to the stream proxy and to the server, so a timeout
// arriving through a snippet or an import is caught too.
func TestCaddySSETimeoutsCompiled(t *testing.T) {
	caddyBin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skipf("caddy binary not on PATH (%v) — TestCaddySSETimeoutsOutlastTheHeartbeat pins the same "+
			"property on the text", err)
	}
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			bounds, sse := caddySSECompiledBounds(t, caddyAdaptJSON(t, caddyBin, path))
			if sse == 0 {
				t.Fatalf("%s: adapted config has no `flush_interval: -1` reverse_proxy — the stream proxy is missing", path)
			}
			for name, d := range bounds {
				if why := caddySSEBoundViolation(name, d); why != "" {
					t.Errorf("%s: compiled %s = %s: %s", path, name, d, why)
				}
			}
		})
	}
}

// caddyAdaptJSON returns the adapted JSON of a Caddyfile as a generic
// tree, the ansible placeholder rendered first.
func caddyAdaptJSON(t *testing.T, caddyBin, path string) any {
	t.Helper()
	src := strings.ReplaceAll(readCaddyfile(t, path), "{{ caddy_site_addresses }}", "api.stellarindex.io")
	rendered := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(rendered, []byte(src), 0o600); err != nil {
		t.Fatalf("write rendered caddyfile: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), caddyBin, "adapt", "--config", rendered, "--adapter", "caddyfile").Output()
	if err != nil {
		t.Fatalf("caddy adapt %s: %v", path, err)
	}
	var tree any
	if err := json.Unmarshal(out, &tree); err != nil {
		t.Fatalf("parse adapted config for %s: %v", path, err)
	}
	return tree
}

// caddySSECompiledBounds walks the adapted config and returns the
// largest value of each stream-path timeout (keyed like the Caddyfile
// names) plus the number of stream proxies found.
func caddySSECompiledBounds(t *testing.T, tree any) (map[string]time.Duration, int) {
	t.Helper()
	bounds := map[string]time.Duration{}
	sse := 0
	keep := func(name string, v any) {
		if d := caddyJSONDuration(t, name, v); d > bounds[name] {
			bounds[name] = d
		}
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case []any:
			for _, c := range v {
				walk(c)
			}
		case map[string]any:
			if _, isServer := v["listen"]; isServer {
				keep("timeouts>write", v["write_timeout"])
			}
			if v["handler"] == "reverse_proxy" && v["flush_interval"] == float64(-1) {
				sse++
				keep("stream_timeout", v["stream_timeout"])
				if tr, ok := v["transport"].(map[string]any); ok {
					for name := range caddySSEIdleBounds {
						keep(name, tr[name])
					}
				}
			}
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(tree)
	return bounds, sse
}

// caddyJSONDuration decodes a caddy JSON duration: integer nanoseconds
// or a Go duration string. Absent is zero.
func caddyJSONDuration(t *testing.T, name string, v any) time.Duration {
	t.Helper()
	switch d := v.(type) {
	case nil:
		return 0
	case float64:
		return time.Duration(d)
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil {
			t.Fatalf("compiled %s = %q: not a duration this test can check", name, d)
		}
		return parsed
	}
	t.Fatalf("compiled %s has unexpected type %T", name, v)
	return 0
}
