package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1FailingServer is a TLS stellar.toml host that answers every request with handler.
func sep1FailingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "https://")
}

func TestSep1FailureReasonClassifiesRealResolverErrors(t *testing.T) {
	notFound, notFoundDomain := sep1FailingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	garbage, garbageDomain := sep1FailingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("= = = not toml ]]]\n"))
	})
	untrusted, untrustedDomain := sep1FailingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(healthyTOML))
	})
	untrustingResolver := metadata.WithClient(
		metadata.NewResolver(metadata.Options{Timeout: 10 * time.Second, AllowPrivateIPs: true}), &http.Client{})
	resolve := func(r *metadata.Resolver, domain string) error {
		_, err := r.Resolve(context.Background(), domain)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded; the case needs a failure", domain)
		}
		return err
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"http status":    {resolve(sep1TestResolver(notFound), notFoundDomain), "http"},
		"toml parse":     {resolve(sep1TestResolver(garbage), garbageDomain), "parse"},
		"untrusted cert": {resolve(untrustingResolver, untrustedDomain), "tls"},
		"url as domain":  {resolve(sep1TestResolver(untrusted), "https://example.org"), "invalid_domain"},
		"run deadline":   {fmt.Errorf("sep1: example.org: %w", context.DeadlineExceeded), "timeout"},
		"dns timeout":    {fmt.Errorf("sep1 dialer: %w", &net.DNSError{Err: "i/o timeout", IsTimeout: true}), "timeout"},
		"dns no host":    {fmt.Errorf("sep1 dialer: %w", &net.DNSError{Err: "no such host", IsNotFound: true}), "dns"},
		"dial refused":   {&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "connect"},
		"ssrf":           {fmt.Errorf("sep1 dialer: %w", metadata.ErrSSRFBlocked), "ssrf"},
		"too large":      {metadata.ErrTOMLTooLarge, "too_large"},
		"too deep":       {metadata.ErrTOMLTooDeep, "too_deep"},
		"unclassifiable": {errors.New("sep1: something new"), "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sep1FailureReason(tc.err); got != tc.want {
				t.Errorf("sep1FailureReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestSep1RefreshLoopCountsReachedAndRegressed: the run's counters follow the per-issuer outcomes, with the regression
// denominator limited to domains that had served a toml before.
func TestSep1RefreshLoopCountsReachedAndRegressed(t *testing.T) {
	log := &sep1CallLog{}
	srv, live := sep1TestDomain(t, log, "live", tomlListing("Live Org", "GLIVE"))
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closedDomain := strings.TrimPrefix(closed.URL, "https://")
	closed.Close()
	cands := []timescale.IssuerSep1Candidate{
		{GStrkey: "GLIVE", HomeDomain: live, Reached: true},
		{GStrkey: "GGONE", HomeDomain: closedDomain, Reached: true},
		{GStrkey: "GNOENTRY", HomeDomain: "unconfirmed.example", Reached: false},
	}
	stats, failed, ourFault := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands[0], cands[1]),
		sep1TestResolver(srv), cands, false)
	if len(failed) != 2 || len(ourFault) != 0 {
		t.Fatalf("failed=%v ourFault=%v; want two failures, none ours", failed, ourFault)
	}
	want := sep1RunStats{attempted: 3, stored: 1, failed: 2, reached: 2, regressed: 1, failures: map[sep1FailureKey]int{
		{reason: "connect", reached: true}: 1,
		{reason: "domain", reached: false}: 1,
	}}
	if fmt.Sprint(stats) != fmt.Sprint(want) {
		t.Errorf("stats = %+v\nwant    %+v", stats, want)
	}
	if sep1RunVerdict(stats, 0.5) {
		t.Error("three attempts judged systemic; the verdict needs the minimum reached count")
	}
}

func TestSep1RefreshTextfileListsEveryReasonAndRoundTrips(t *testing.T) {
	var stats sep1RunStats
	stats.record(timescale.IssuerSep1Candidate{Reached: true}, sep1Stored, "")
	stats.record(timescale.IssuerSep1Candidate{Reached: true}, sep1Failed, "timeout")
	stats.record(timescale.IssuerSep1Candidate{Reached: false}, sep1Failed, "timeout")
	stats.record(timescale.IssuerSep1Candidate{Reached: true}, sep1OurFault, "lake")
	stats.aborted = true
	now := time.Unix(1_700_000_000, 0)
	body := sep1RefreshTextfile(stats, true, now)

	for _, line := range []string{
		"stellarindex_sep1_refresh_last_run_unix 1700000000",
		"stellarindex_sep1_refresh_attempted 4",
		`stellarindex_sep1_refresh_issuers{outcome="stored"} 1`,
		`stellarindex_sep1_refresh_issuers{outcome="failed"} 2`,
		`stellarindex_sep1_refresh_issuers{outcome="our_fault"} 1`,
		"stellarindex_sep1_refresh_reached 3",
		"stellarindex_sep1_refresh_regressed 2",
		`stellarindex_sep1_refresh_failures{reason="timeout",reached="true"} 1`,
		`stellarindex_sep1_refresh_failures{reason="timeout",reached="false"} 1`,
		`stellarindex_sep1_refresh_failures{reason="lake",reached="true"} 1`,
		`stellarindex_sep1_refresh_failures{reason="tls",reached="true"} 0`,
		"stellarindex_sep1_refresh_systemic 1",
		"stellarindex_sep1_refresh_aborted 1",
	} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("textfile lacks %q:\n%s", line, body)
		}
	}
	for _, reason := range sep1FailureReasons {
		for _, reached := range []string{"true", "false"} {
			if !strings.Contains(body, fmt.Sprintf("{reason=%q,reached=%q} ", reason, reached)) {
				t.Errorf("reason %s/%s not emitted; a rate over it would start from a missing series", reason, reached)
			}
		}
	}
	if n := strings.Count(body, "# TYPE "); n != 8 {
		t.Errorf("%d # TYPE lines, want 8 (one per family)", n)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sep1_refresh.prom")
	stampSep1RefreshTextfile(path, stats, true, now)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("stamped file differs from the rendered body")
	}
	stampSep1RefreshTextfile(path, sep1RunStats{}, false, now.Add(time.Hour))
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "stellarindex_sep1_refresh_attempted 0\n") ||
		!strings.Contains(string(got), "stellarindex_sep1_refresh_last_run_unix 1700003600\n") {
		t.Errorf("empty-queue stamp did not overwrite the run: %s", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left beside the textfile: %v", entries)
	}
	stampSep1RefreshTextfile("", stats, true, now)
}
