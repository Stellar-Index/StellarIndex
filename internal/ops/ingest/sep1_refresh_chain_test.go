package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1ChainStub answers the live home_domain re-read from a fixed map.
type sep1ChainStub struct {
	live map[string]clickhouse.AccountAuthFlags
	err  error
}

func (s sep1ChainStub) BulkAccountAuthFlags(_ context.Context, keys []string) (map[string]clickhouse.AccountAuthFlags, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]clickhouse.AccountAuthFlags, len(keys))
	for _, k := range keys {
		if f, ok := s.live[k]; ok {
			out[k] = f
		}
	}
	return out, nil
}

// sep1ChainFor is a chain that still declares every candidate's stored domain.
func sep1ChainFor(cands ...timescale.IssuerSep1Candidate) sep1ChainStub {
	live := make(map[string]clickhouse.AccountAuthFlags, len(cands))
	for _, c := range cands {
		live[c.GStrkey] = clickhouse.AccountAuthFlags{HomeDomain: c.HomeDomain, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 1}
	}
	return sep1ChainStub{live: live}
}

// tomlListing is a stellar.toml whose [[CURRENCIES]] lists issuer back, i.e.
// one that passes the bidirectional org check for it.
func tomlListing(org, issuer string) string {
	return fmt.Sprintf("VERSION = \"2.0.0\"\n\n[DOCUMENTATION]\nORG_NAME = %q\n\n[[CURRENCIES]]\ncode = \"USD\"\nissuer = %q\n", org, issuer)
}

// TestSep1RefreshRebindsToTheChainsDomainBeforeFetching: a stale stored domain is re-bound to the chain's declared domain and only that one is fetched.
func TestSep1RefreshRebindsToTheChainsDomainBeforeFetching(t *testing.T) {
	const issuer = "GMOVED"
	log := &sep1CallLog{}
	storedSrv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	_, current := sep1TestDomain(t, log, "current", tomlListing("Current Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {HomeDomain: current, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(storedSrv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	want := "[rebind " + issuer + " " + current + " mark " + issuer + " fetch current write " + issuer + "]"
	if got := fmt.Sprint(log.steps()); got != want {
		t.Errorf("call order = %s;\n            want %s — the domain the account no longer declares must never be fetched", got, want)
	}
}

// TestSep1RefreshClearsADomainTheChainNoLongerDeclares: a cleared home_domain is cleared in the row, not fetched, and not a failed attempt.
func TestSep1RefreshClearsADomainTheChainNoLongerDeclares(t *testing.T) {
	const issuer = "GCLEARED"
	log := &sep1CallLog{}
	srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[clear "+issuer+"]" {
		t.Errorf("call order = %s; want [clear %s]", got, issuer)
	}
}

// TestSep1RefreshDoesNotFetchAnUnconfirmedDomain: an unconfirmable domain is not fetched; the attempt is marked failed so a lake outage stays in the systemic unwind.
func TestSep1RefreshDoesNotFetchAnUnconfirmedDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		chain   sep1ChainStub
		ourSide bool
	}{
		"no live entry": {},
		"lake error":    {chain: sep1ChainStub{err: errors.New("clickhouse down")}, ourSide: true},
	} {
		t.Run(name, func(t *testing.T) {
			const issuer = "GUNREAD"
			log := &sep1CallLog{}
			srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))

			ok, failed, ourFault := sep1RefreshLoop(context.Background(), log, tc.chain, sep1TestResolver(srv),
				[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, false)
			if ok != 0 || len(failed) != 1 || failed[0] != issuer {
				t.Fatalf("sep1RefreshLoop = (%d, %v); want (0, [%s])", ok, failed, issuer)
			}
			if got := len(ourFault) == 1; got != tc.ourSide {
				t.Errorf("our-fault keys = %v; want our-side=%v — only a lake failure is ours to unwind", ourFault, tc.ourSide)
			}
			if got := fmt.Sprint(log.steps()); got != "[mark "+issuer+"]" {
				t.Errorf("call order = %s; want [mark %s] — nothing may be fetched or written", got, issuer)
			}
		})
	}
}

// TestSep1RefreshDryRunRebindsNothing: a dry run fetches the chain's domain but writes no row.
func TestSep1RefreshDryRunRebindsNothing(t *testing.T) {
	const issuer = "GMOVED"
	log := &sep1CallLog{}
	srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
	_, current := sep1TestDomain(t, log, "current", tomlListing("Current Org", issuer))
	chain := sep1ChainStub{live: map[string]clickhouse.AccountAuthFlags{
		issuer: {HomeDomain: current, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}

	ok, failed, _ := sep1RefreshLoop(context.Background(), log, chain, sep1TestResolver(srv),
		[]timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}, true)
	if ok != 1 || len(failed) != 0 {
		t.Fatalf("sep1RefreshLoop = (%d, %v); want (1, [])", ok, failed)
	}
	if got := fmt.Sprint(log.steps()); got != "[fetch current]" {
		t.Errorf("dry run call log = %s; want [fetch current]", got)
	}
}

// TestSep1RefreshClassifiesAFailedWrite: a content-refused payload is the document's fault and keeps its ladder step; other write failures are ours and unwound.
func TestSep1RefreshClassifiesAFailedWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		ourSide bool
	}{
		"payload rejected": {err: fmt.Errorf("timescale: %w: 22P05", timescale.ErrSep1PayloadRejected)},
		"store down":       {err: errors.New("connection refused"), ourSide: true},
	} {
		t.Run(name, func(t *testing.T) {
			const issuer = "GWRITE"
			log := &sep1CallLog{writeErr: map[string]error{issuer: tc.err}}
			srv, stored := sep1TestDomain(t, log, "stored", tomlListing("Stored Domain Org", issuer))
			cands := []timescale.IssuerSep1Candidate{{GStrkey: issuer, HomeDomain: stored}}

			ok, failed, ourFault := sep1RefreshLoop(context.Background(), log, sep1ChainFor(cands...), sep1TestResolver(srv), cands, false)
			if ok != 0 || len(failed) != 1 || failed[0] != issuer {
				t.Fatalf("sep1RefreshLoop = (%d, %v); want (0, [%s])", ok, failed, issuer)
			}
			if got := len(ourFault) == 1; got != tc.ourSide {
				t.Errorf("our-fault keys = %v; want our-side=%v", ourFault, tc.ourSide)
			}
		})
	}
}

// TestSep1FetchDomain pins that the override redirects the FETCH while the on-chain value stays authoritative for identity.
func TestSep1FetchDomain(t *testing.T) {
	if got := sep1FetchDomain("GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "circle.com"); got != "centre.io" {
		t.Errorf("USDC issuer fetch domain = %q, want centre.io", got)
	}
	if got := sep1FetchDomain("GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", "aqua.network"); got != "aqua.network" {
		t.Errorf("non-overridden issuer = %q, want on-chain domain", got)
	}
}

// sep1Run builds an attempted batch: reachedOK/reachedFailed rows that had
// served a payload before, and neverOK/neverFailed rows that never had.
func sep1Run(reachedOK, reachedFailed, neverOK, neverFailed int) ([]timescale.IssuerSep1Candidate, []string) {
	var cands []timescale.IssuerSep1Candidate
	var failed []string
	add := func(n int, reached, fail bool) {
		for range n {
			key := fmt.Sprintf("G%06d", len(cands))
			cands = append(cands, timescale.IssuerSep1Candidate{GStrkey: key, HomeDomain: "example.org", Reached: reached})
			if fail {
				failed = append(failed, key)
			}
		}
	}
	add(reachedOK, true, false)
	add(reachedFailed, true, true)
	add(neverOK, false, false)
	add(neverFailed, false, true)
	return cands, failed
}

// TestSep1RunVerdict pins the systemic-outage guard: it trips on domains that served a stellar.toml before and now fail, never on never-answered ones.
// A retry backoff cannot tell a dead domain from our DNS being down, and would walk the population to the 30-day cap with the freshness watchdog still green.
func TestSep1RunVerdict(t *testing.T) {
	r := defaultSystemicFailureRate
	for _, tc := range []struct {
		name                                         string
		reachedOK, reachedFailed, neverOK, neverFail int
		rate                                         float64
		want                                         bool
	}{
		{
			// The testnet shape that failed the unit every hour: 19 real
			// domains answer, 731 junk home_domains never have.
			name:      "testnet healthy run (19 reached ok / 731 never-reached failed)",
			reachedOK: 19, neverFail: 731, rate: r, want: false,
		},
		{
			// The r1 baseline: the 291 failures were all never-reached rows.
			name:      "r1 healthy run (209 ok / 291 never-reached failed)",
			reachedOK: 209, neverFail: 291, rate: r, want: false,
		},
		{
			name:          "true outage: previously-verified domains fail en masse",
			reachedFailed: 200, neverFail: 550, rate: r, want: true,
		},
		{
			name:          "testnet outage: the 19 real domains fail, below the sample floor",
			reachedFailed: 19, neverFail: 731, rate: r, want: false,
		},
		{
			name:      "just under the threshold",
			reachedOK: 11, reachedFailed: 89, neverFail: 500, rate: r, want: false,
		},
		{
			name:      "exactly at the threshold trips it",
			reachedOK: 10, reachedFailed: 90, rate: r, want: true,
		},
		{
			name:          "reached sample below the floor never trips, even at 100%",
			reachedFailed: systemicMinAttempts - 1, neverFail: 700, rate: r, want: false,
		},
		{
			name:          "reached sample exactly at the floor is eligible",
			reachedFailed: systemicMinAttempts, rate: r, want: true,
		},
		{
			// `sep1-refresh -issuer G…` resolves one domain. A transient
			// failure there must not unwind anything or fail the unit.
			name:          "targeted single-issuer refresh",
			reachedFailed: 1, rate: r, want: false,
		},
		{
			// The documented escape hatch for a known mass outage.
			name:          "rate above 1 disables the guard",
			reachedFailed: 750, rate: 1.5, want: false,
		},
		{
			name: "empty run",
			rate: r, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands, failed := sep1Run(tc.reachedOK, tc.reachedFailed, tc.neverOK, tc.neverFail)
			if got := sep1RunVerdict(cands, failed, tc.rate); got != tc.want {
				t.Errorf("sep1RunVerdict(reached ok=%d failed=%d, never ok=%d failed=%d, rate=%v) = %v, want %v",
					tc.reachedOK, tc.reachedFailed, tc.neverOK, tc.neverFail, tc.rate, got, tc.want)
			}
		})
	}
}

// TestSep1RunVerdictTestnetShape guards the fixture: the testnet batch is 97% failures, so only the reached-domain population keeps the verdict green.
func TestSep1RunVerdictTestnetShape(t *testing.T) {
	cands, failed := sep1Run(19, 0, 0, 731)
	if len(cands) != 750 || len(failed) != 731 {
		t.Fatalf("fixture = %d attempts / %d failed; want 750 / 731", len(cands), len(failed))
	}
	if raw := float64(len(failed)) / float64(len(cands)); raw < defaultSystemicFailureRate {
		t.Fatalf("raw failure rate %.3f is below %.2f; fixture no longer reproduces the testnet run", raw, defaultSystemicFailureRate)
	}
	if sep1RunVerdict(cands, failed, defaultSystemicFailureRate) {
		t.Error("testnet healthy run judged SYSTEMIC; never-reached domains must not count as regressions")
	}
}

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

// sep1TestDomain starts a TLS server serving body at the SEP-1 well-known path and returns its host:port, which the resolver
// takes as a home_domain. Each hit logs "fetch <label>" so a caller can assert where the fetch falls relative to store writes.
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
