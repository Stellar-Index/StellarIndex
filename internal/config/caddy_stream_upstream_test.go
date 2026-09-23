package config_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── The stream proxy and the API upstream's health (GH #886) ───────
//
// The /v1/*/stream routes are proxied by their own `reverse_proxy @sse`
// handler (it needs `flush_interval -1`), separate from the catch-all
// proxy that carries the active /v1/healthz check. Caddy keeps active
// health state per handler, so a stream proxy without its own check
// never learns the upstream is down: during a rolling restart ordinary
// routes fail fast with 503 while every new stream request is still
// proxied to the dead listener. These tests pin that every proxy to
// the API carries the same active health check, in both Caddyfiles.

// caddyAPIUpstream is the dial address every API proxy in the
// Caddyfiles points at.
const caddyAPIUpstream = "localhost:3000"

// caddyHealthDirectives are the active-health-check subdirectives the
// catch-all proxy has always carried.
var caddyHealthDirectives = []string{"health_uri ", "health_interval ", "health_timeout ", "health_status "}

// caddyProxyBlocks returns every `reverse_proxy … localhost:3000 {`
// stanza's direct subdirectives, keyed by the stanza's opening line.
func caddyProxyBlocks(t *testing.T, path string) map[caddyDirective][]string {
	t.Helper()
	blocks := map[caddyDirective][]string{}
	var open *caddyDirective
	for _, d := range caddyDirectives(t, path) {
		switch {
		case open == nil && strings.HasPrefix(d.text, "reverse_proxy ") &&
			strings.Contains(d.text, caddyAPIUpstream) && strings.HasSuffix(d.text, "{"):
			opened := d
			open = &opened
			blocks[opened] = nil
		case open != nil && d.depth == open.depth && d.text == "}":
			open = nil
		case open != nil && d.depth == open.depth+1:
			blocks[*open] = append(blocks[*open], d.text)
		}
	}
	return blocks
}

// TestCaddyEveryAPIProxyIsHealthChecked is the text-level pin: it holds
// wherever the suite runs, caddy binary or not.
func TestCaddyEveryAPIProxyIsHealthChecked(t *testing.T) {
	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			blocks := caddyProxyBlocks(t, path)
			if len(blocks) < 2 {
				t.Fatalf("%s: found %d `reverse_proxy … %s {` stanzas, want the stream proxy and the catch-all",
					path, len(blocks), caddyAPIUpstream)
			}
			var want []string
			for open, body := range blocks {
				got := caddyHealthLines(body)
				if len(got) != len(caddyHealthDirectives) {
					t.Errorf("%s:%d: `%s` has health settings %q — every proxy to the API needs the full "+
						"active health check, or it keeps sending new requests to an upstream the other "+
						"proxy has already taken out of rotation", path, open.line, open.text, got)
					continue
				}
				if want == nil {
					want = got
				} else if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("%s:%d: `%s` health settings %q differ from %q — the proxies disagree "+
						"about when the API is down", path, open.line, open.text, got, want)
				}
			}
		})
	}
}

func caddyHealthLines(body []string) []string {
	var out []string
	for _, prefix := range caddyHealthDirectives {
		for _, line := range body {
			if strings.HasPrefix(line, prefix) {
				out = append(out, line)
			}
		}
	}
	return out
}

// TestCaddyStreamRoutesHonourUpstreamHealth runs the real config in
// real caddy in front of a stub API whose /v1/healthz answers 503, and
// asserts a stream request is refused exactly like an ordinary one.
func TestCaddyStreamRoutesHonourUpstreamHealth(t *testing.T) {
	caddyBin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skipf("caddy binary not on PATH (%v) — TestCaddyEveryAPIProxyIsHealthChecked pins the same "+
			"property on the text", err)
	}

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: stub\n\n"))
	}))
	t.Cleanup(stub.Close)

	for _, path := range caddyfiles {
		t.Run(filepath.Base(path), func(t *testing.T) {
			base := caddyRunLocal(t, caddyBin, path, stub.Listener.Addr().String())

			// The catch-all proxy going 503 is the witness that the
			// health checker has run and marked the upstream down.
			if !caddyEventually(t, base+"/v1/price", http.StatusServiceUnavailable) {
				t.Fatalf("%s: /v1/price never answered 503 against an upstream failing /v1/healthz — "+
					"the catch-all's health check did not engage, so this test proves nothing", path)
			}
			// Polled too: each handler's checker runs on its own clock.
			for _, route := range []string{"/v1/price/stream", "/v1/ledger/stream"} {
				if !caddyEventually(t, base+route, http.StatusServiceUnavailable) {
					t.Errorf("%s: GET %s never answered 503 with the upstream failing /v1/healthz, while "+
						"/v1/price did — the stream proxy ignores upstream health", path, route)
				}
			}
		})
	}
}

// caddyRunLocal starts caddy on a rendered copy of the Caddyfile served
// over plain HTTP on a free loopback port, proxying to upstream, and
// returns the base URL. HOME and the XDG dirs point into a temp dir so
// caddy's autosave and storage never touch the real user profile.
func caddyRunLocal(t *testing.T, caddyBin, path, upstream string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	src := readCaddyfile(t, path)
	for from, to := range map[string]string{
		"\n{\n":                            "\n{\n\tadmin off\n",
		"\napi.stellarindex.io {\n":        "\nhttp://" + addr + " {\n",
		"\n{{ caddy_site_addresses }} {\n": "\nhttp://" + addr + " {\n",
		"root * /etc/caddy":                "root * " + dir,
		caddyAPIUpstream:                   upstream,
	} {
		src = strings.ReplaceAll(src, from, to)
	}
	rendered := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(rendered, []byte(src), 0o600); err != nil {
		t.Fatalf("write rendered caddyfile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, caddyBin, "run", "--config", rendered, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_DATA_HOME="+dir)
	logPath := filepath.Join(dir, "caddy.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		cancel()
		t.Fatalf("create caddy log: %v", err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start caddy: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			if out, err := os.ReadFile(logPath); err == nil {
				t.Logf("caddy log tail:\n%s", caddyTail(string(out), 15))
			}
		}
	})

	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return base
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy never listened on %s: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func caddyGetStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// caddyEventually polls url until it answers want, for up to 20 s (two
// of the Caddyfiles' 10 s health intervals).
func caddyEventually(t *testing.T, url string, want int) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if caddyGetStatus(t, url) == want {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func caddyTail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
