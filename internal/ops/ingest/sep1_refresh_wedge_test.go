package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// hostileTOML is 4,000 nested inline tables: 16 KB, inside the resolver's 1 MiB cap, yet 1.81 GiB of decoder allocation
// before the parse budget existed, a cgroup SIGKILL under the unit's MemoryMax=2G.
func hostileTOML() string {
	return "a = " + strings.Repeat("{b=", 4000) + "1" + strings.Repeat("}", 4000)
}

const healthyTOML = `VERSION = "2.0.0"

[DOCUMENTATION]
ORG_NAME = "Second In Queue"

[[CURRENCIES]]
code = "GOOD"
issuer = "GBEHIND"
`

// sep1CallLog records, in one interleaved sequence, every store write and every stellar.toml fetch, because the property under test
// is ordering (the marker is durable before the decode can kill the worker). Fetches arrive from the server goroutine, hence the mutex.
type sep1CallLog struct {
	mu  sync.Mutex
	seq []string
	// moved names issuers whose home_domain the store reports as changed
	// since the candidate was read, so the payload write lands nowhere.
	moved map[string]bool
	// writeErr is what SetIssuerSep1Payload returns, per issuer.
	writeErr map[string]error
}

func (l *sep1CallLog) add(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq = append(l.seq, step)
}

func (l *sep1CallLog) steps() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seq...)
}

func (l *sep1CallLog) MarkIssuerSep1Failed(_ context.Context, gStrkey string) (int, error) {
	l.add("mark " + gStrkey)
	return 1, nil
}

func (l *sep1CallLog) SetIssuerSep1Payload(_ context.Context, gStrkey, _ string, _ []byte) (bool, error) {
	l.add("write " + gStrkey)
	if err := l.writeErr[gStrkey]; err != nil {
		return false, err
	}
	return !l.moved[gStrkey], nil
}

func (l *sep1CallLog) SyncIssuerHomeDomain(_ context.Context, gStrkey, homeDomain string) (bool, error) {
	l.add("rebind " + gStrkey + " " + homeDomain)
	return true, nil
}

func (l *sep1CallLog) ClearIssuerHomeDomain(_ context.Context, gStrkey string) (bool, error) {
	l.add("clear " + gStrkey)
	return true, nil
}

// count returns how many times step appears in the log.
func (l *sep1CallLog) count(step string) int {
	n := 0
	for _, s := range l.steps() {
		if s == step {
			n++
		}
	}
	return n
}

// sep1TestDomain starts a TLS server that serves body at the SEP-1
// well-known path and returns its host:port, which is what the resolver
// takes as a home_domain. Every hit is logged as "fetch <label>" so a
// caller can assert where the fetch falls relative to the store writes.
func sep1TestDomain(t *testing.T, log *sep1CallLog, label, body string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/stellar.toml" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.add("fetch " + label)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "https://")
}

// sep1TestResolver returns the REAL resolver, wired to trust the test
// servers' certificate. The parser, the HTTP client, the SSRF guard and
// the body cap are all production code; only the store is a double.
func sep1TestResolver(srv *httptest.Server) *metadata.Resolver {
	return metadata.WithClient(
		metadata.NewResolver(metadata.Options{Timeout: 10 * time.Second, AllowPrivateIPs: true}),
		srv.Client(),
	)
}

// TestSep1RefreshLoopSurvivesAHostileTOML: a hostile document must not stop the refresh reaching later issuers or return as candidate #1 on every run. Asserts: the run completes and the healthy issuer behind is written; the poison issuer is marked exactly once, BEFORE its fetch; the loop stays inside a small allocation budget.
func TestSep1RefreshLoopSurvivesAHostileTOML(t *testing.T) {
	const (
		poison  = "GPOISON"
		healthy = "GBEHIND"
	)
	log := &sep1CallLog{}
	poisonSrv, poisonDomain := sep1TestDomain(t, log, poison, hostileTOML())
	_, healthyDomain := sep1TestDomain(t, log, healthy, healthyTOML)
	resolver := sep1TestResolver(poisonSrv)

	// The poison issuer is FIRST, exactly as a vanity-ground key sorting
	// to the head of `ORDER BY sep1_resolved_at ASC NULLS FIRST, g_strkey
	// ASC` would be.
	candidates := []timescale.IssuerSep1Candidate{
		{GStrkey: poison, HomeDomain: poisonDomain},
		{GStrkey: healthy, HomeDomain: healthyDomain},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ok, failed, _ := sep1RefreshLoop(ctx, log, sep1ChainFor(candidates...), resolver, candidates, false)
	runtime.ReadMemStats(&after)

	// 1. The run completed and the issuer BEHIND the poison one was served.
	if ok != 1 || len(failed) != 1 || failed[0] != poison {
		t.Fatalf("sep1RefreshLoop = (%d ok, %v failed); want (1, [%s]) — "+
			"one hostile document must not cost the issuers behind it", ok, failed, poison)
	}

	// 2. Marked, marked before the fetch, and the healthy issuer reached.
	// Asserted as the exact interleaving so the mark cannot drift back
	// behind the fetch unnoticed.
	want := "[mark " + poison + " fetch " + poison + " mark " + healthy +
		" fetch " + healthy + " write " + healthy + "]"
	if got := fmt.Sprint(log.steps()); got != want {
		t.Errorf("call order = %s;\n            want %s", got, want)
	}
	// Exactly once: the systemic-outage unwind takes back exactly one
	// ladder step per failed key, so a double mark would leave the row
	// deferred after an unwind that believed it had undone the run.
	if n := log.count("mark " + poison); n != 1 {
		t.Errorf("the poison issuer was marked %d times; want exactly 1 (the unwind decrements by one)", n)
	}

	// 3. The hostile document was refused, not decoded.
	const allocBudget = 64 << 20
	if spent := after.TotalAlloc - before.TotalAlloc; spent > allocBudget {
		t.Errorf("the refresh loop allocated %d bytes over two issuers; want under %d — "+
			"the hostile document reached the decoder", spent, allocBudget)
	}
}

// TestSep1RefreshLoopMarksBeforeASuccessToo: a healthy issuer is marked before its fetch as well, so the pre-mark never depends on predicting the outcome; SetIssuerSep1Payload clears the ladder in the same statement (test/integration/pg_assets_sep1_test.go).
func TestSep1RefreshLoopMarksBeforeASuccessToo(t *testing.T) {
	log := &sep1CallLog{}
	srv, domain := sep1TestDomain(t, log, "GBEHIND", healthyTOML)
	cands := []timescale.IssuerSep1Candidate{{GStrkey: "GBEHIND", HomeDomain: domain}}
	ok, failed, _ := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands...), sep1TestResolver(srv), cands, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	want := "[mark GBEHIND fetch GBEHIND write GBEHIND]"
	if got := fmt.Sprint(log.steps()); got != want {
		t.Errorf("call order = %s; want %s", got, want)
	}
}

// TestSep1RefreshLoopMovedDomainIsNotAFailure: a payload discarded because home_domain moved mid-fetch already reset the ladder, so the key stays out of the unwind list.
func TestSep1RefreshLoopMovedDomainIsNotAFailure(t *testing.T) {
	log := &sep1CallLog{moved: map[string]bool{"GBEHIND": true}}
	srv, domain := sep1TestDomain(t, log, "GBEHIND", healthyTOML)
	cands := []timescale.IssuerSep1Candidate{{GStrkey: "GBEHIND", HomeDomain: domain}}
	ok, failed, _ := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands...), sep1TestResolver(srv), cands, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
}

// TestSep1RefreshLoopDryRunWritesNothing: -dry-run must still touch no rows despite the pre-mark.
func TestSep1RefreshLoopDryRunWritesNothing(t *testing.T) {
	log := &sep1CallLog{}
	srv, domain := sep1TestDomain(t, log, "GBEHIND", healthyTOML)
	cands := []timescale.IssuerSep1Candidate{{GStrkey: "GBEHIND", HomeDomain: domain}}
	ok, failed, _ := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands...), sep1TestResolver(srv), cands, true)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop(dryRun) = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[fetch GBEHIND]" {
		t.Errorf("dry run call log = %s; want [fetch GBEHIND] — no row may be written", got)
	}
}
