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

// hostileTOML is the shape RSEC-Z1 / RLT-458 describe: 4,000 nested
// inline tables. 16 KB — comfortably inside the resolver's 1 MiB body
// cap, publishable by any account with 1 XLM — and measured at 1.81 GiB
// of decoder allocation in 0.72s before the parse budget existed. The
// refresh unit runs under MemoryMax=2G, so that is a cgroup SIGKILL.
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

// sep1CallLog records, in order, every store write the refresh loop
// makes AND every stellar.toml fetch the real resolver performs.
// Ordering ACROSS those two is the property under test — "the marker is
// durable before the decode can kill the worker" is a statement about
// where the fetch sits in the sequence, not about the writes alone — so
// the log is one interleaved sequence and not a set of counters.
//
// The fetch entries are appended from the test server's handler
// goroutine, hence the mutex.
type sep1CallLog struct {
	mu  sync.Mutex
	seq []string
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

func (l *sep1CallLog) SetIssuerSep1Payload(_ context.Context, gStrkey string, _ []byte) error {
	l.add("write " + gStrkey)
	return nil
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

// TestSep1RefreshLoopSurvivesAHostileTOML is the wedge (RSEC-Z1 /
// RLT-458) at the production entry point: one issuer serving a hostile
// document must not stop the refresh reaching the ~76,000 issuers
// behind it, and must not return as candidate #1 on every later run.
//
// Three assertions, one per link of the chain the finding names:
//
//  1. the run COMPLETES and the healthy issuer behind the poison one is
//     reached and written (no starvation);
//  2. the poison issuer is MARKED — so the retry ladder defers it rather
//     than `ORDER BY … NULLS FIRST` re-serving it forever — exactly once,
//     and BEFORE its document is fetched, because the moment the worker
//     can be killed is during the decode and not after it;
//  3. the whole loop stays inside a small allocation budget, i.e. the
//     hostile document was refused rather than decoded.
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
	ok, failed := sep1RefreshLoop(ctx, log, resolver, candidates, false)
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

// TestSep1RefreshLoopMarksBeforeASuccessToo pins the other half of the
// ordering rule: a HEALTHY issuer is marked before its fetch as well.
// Anything less would make the pre-mark conditional on predicting the
// outcome, which is exactly the assumption the wedge exploited. The mark
// costs a success nothing, because SetIssuerSep1Payload clears the
// ladder in the same statement that writes the payload — proved against
// Postgres in test/integration/sep1_retry_backoff_test.go.
func TestSep1RefreshLoopMarksBeforeASuccessToo(t *testing.T) {
	log := &sep1CallLog{}
	srv, domain := sep1TestDomain(t, log, "GBEHIND", healthyTOML)
	ok, failed := sep1RefreshLoop(context.Background(), log, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: "GBEHIND", HomeDomain: domain}}, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	want := "[mark GBEHIND fetch GBEHIND write GBEHIND]"
	if got := fmt.Sprint(log.steps()); got != want {
		t.Errorf("call order = %s; want %s", got, want)
	}
}

// TestSep1RefreshLoopDryRunWritesNothing guards the operator-facing
// invariant the pre-mark could most easily have broken: -dry-run must
// still touch no rows.
func TestSep1RefreshLoopDryRunWritesNothing(t *testing.T) {
	log := &sep1CallLog{}
	srv, domain := sep1TestDomain(t, log, "GBEHIND", healthyTOML)
	ok, failed := sep1RefreshLoop(context.Background(), log, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: "GBEHIND", HomeDomain: domain}}, true)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop(dryRun) = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[fetch GBEHIND]" {
		t.Errorf("dry run call log = %s; want [fetch GBEHIND] — no row may be written", got)
	}
}
