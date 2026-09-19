package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1RefreshCmd resolves the SEP-1 stellar.toml for every issuer
// with a home_domain set and writes the parsed payload back to
// `issuers.sep1_payload` + bumps `sep1_resolved_at`.
//
// Run from cron once an hour (sep1-refresh.timer):
//
//	stellarindex-ops sep1-refresh -config /etc/stellarindex/api.toml \
//	    -limit 750 -older-than 24h -timeout 25m
//
// Per-issuer fetch failures are logged + counted; they don't abort
// the run. The resolver respects its built-in 10s per-request
// timeout + SSRF guard, the parser refuses a document nested past its
// structural-depth budget before decoding it, and each issuer runs on
// its own [sep1PerIssuerBudget] — so no single slow or malicious
// operator domain can stall the whole batch.
//
// A failure also advances that issuer's retry ladder (migration 0159),
// so a home_domain that serves nothing settles at ~1 attempt/month
// instead of one a day, and the budget goes to domains that answer. A
// success clears the ladder, so a recovering domain is back on the fast
// cadence the moment it publishes a document. The loop is deliberately
// SEQUENTIAL: the TOML parser's cost is superlinear in input size on
// attacker-authored input (a measured 4.5 GB from 38 KB), and the unit
// runs under MemoryMax=2G — concurrent parses would multiply the one
// thing that ceiling exists to bound.
//
// Once a payload is written, /v1/issuers list responses surface
// `org_name` from `sep1_payload->>'OrgName'`.
//
// sep1DomainOverrides maps issuers whose ON-CHAIN home_domain no
// longer serves a stellar.toml to the domain that DOES. Curated the
// same way the API's knownIssuers map is (hand-vetted, reviewed in
// PR): the on-chain value is authoritative for identity, but the
// TOML's physical location can rot independently — Circle's
// circle.com/.well-known/stellar.toml 404s (redirect chain to
// www.circle.com then "Invalid .well-known request", verified
// 2026-07-03) while the legacy Centre consortium domain still serves
// the full document, incl. the USDC image + org metadata wallets
// need (board #47).
var sep1DomainOverrides = map[string]string{
	// USDC / EURC issuers — Circle (Centre) toml.
	"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "centre.io",
	"GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2": "centre.io",
}

func sep1RefreshCmd(args []string) error {
	fs := flag.NewFlagSet("sep1-refresh", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	limit := fs.Int("limit", 100,
		fmt.Sprintf("Max issuers to refresh per run (1-%d)", timescale.Sep1RefreshMaxLimit))
	olderThan := fs.Duration("older-than", 24*time.Hour, "Skip issuers refreshed more recently than this")
	timeout := fs.Duration("timeout", 5*time.Minute, "Wall-clock timeout for the whole run")
	gate := opsutil.RegisterWriteGate(fs)
	issuer := fs.String("issuer", "", "Refresh ONLY this issuer G-strkey, bypassing the staleness queue")
	systemicRate := fs.Float64("systemic-failure-rate", defaultSystemicFailureRate,
		"Failure fraction at or above which a run is judged a fault on OUR side: "+
			"the retry backoff it applied is unwound and the run exits non-zero. "+
			"Set above 1 to disable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	dryRun := !gate.Banner() // Banner prints the mode + returns Enabled()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	candidates, err := sep1Candidates(ctx, store, *issuer, *olderThan, *limit)
	if err != nil || len(candidates) == 0 {
		return err
	}

	resolver := metadata.NewResolver(metadata.Options{Timeout: 10 * time.Second})
	ok, failedKeys := sep1RefreshLoop(ctx, store, resolver, candidates, dryRun)
	failed := len(failedKeys)
	fmt.Printf("\n%d succeeded, %d failed\n", ok, failed)
	if dryRun {
		fmt.Println("(dry-run; no rows written)")
	}
	if sep1RunVerdict(ok, failed, *systemicRate) {
		return reportSep1Systemic(store, ok+failed, failedKeys, dryRun)
	}
	return nil
}

// sep1Store is the slice of the store the refresh loop writes. Narrowed
// from *timescale.Store so the loop's ordering guarantees (below) can be
// asserted without a database — the parser, the HTTP client and the
// queue semantics under test are all the real ones either way.
type sep1Store interface {
	MarkIssuerSep1Failed(ctx context.Context, gStrkey string) (int, error)
	SetIssuerSep1Payload(ctx context.Context, gStrkey string, payload []byte) error
}

// sep1Resolver is the slice of [metadata.Resolver] the refresh loop
// uses.
type sep1Resolver interface {
	Resolve(ctx context.Context, domain string) (*metadata.SEP1, error)
}

// sep1PerIssuerBudget bounds one issuer's fetch + parse.
//
// The resolver already carries a 10s per-REQUEST timeout, which is not
// the same thing: a domain that answers slowly but steadily, or a body
// that takes a long time to decode, spends wall-clock that the request
// timeout never sees. This ceiling is what makes "one issuer cannot
// starve the other 76,000" a property of the loop rather than a
// property of whichever timeout happened to fire first.
const sep1PerIssuerBudget = 30 * time.Second

// sep1RefreshLoop resolves each candidate in turn and returns the
// success count plus the g_strkeys of every attempt that produced no
// payload. The failed keys are carried out (rather than just counted) so
// a systemic verdict can take the ladder step back for exactly those
// rows and nothing else.
//
// Sequential on purpose — see the package docblock: the TOML parser's
// cost is superlinear on attacker-authored input and the unit runs under
// a 2G ceiling.
func sep1RefreshLoop(
	ctx context.Context, store sep1Store, resolver sep1Resolver,
	candidates []timescale.IssuerSep1Candidate, dryRun bool,
) (int, []string) {
	var ok int
	failedKeys := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			fmt.Printf("\nAborted at %d/%d (deadline): %v\n", ok+len(failedKeys), len(candidates), err)
			break
		}
		if refreshOneSep1Issuer(ctx, store, resolver, c, dryRun) {
			ok++
			continue
		}
		failedKeys = append(failedKeys, c.GStrkey)
	}
	return ok, failedKeys
}

// refreshOneSep1Issuer fetches, parses and stores one issuer's
// stellar.toml, reporting whether a payload was written.
//
// # Why the attempt is marked FIRST
//
// The mark is queue hygiene: the candidate query is `ORDER BY
// sep1_resolved_at ASC NULLS FIRST`, so a row that is never stamped
// stays candidate #1 on every subsequent run. Marking it only on the way
// OUT of a failure assumes the worker survives to get there — and the
// one input class that most needs marking is the class that kills the
// worker. An oversized or hostile document decoded under the unit's
// MemoryMax=2G earns a cgroup SIGKILL mid-loop, nothing is written, and
// the identical row heads the queue again an hour later, forever,
// freezing issuer metadata for every issuer behind it. Stamping BEFORE
// the fetch is what makes the marker survive the kill: the poison row is
// deferred by the retry ladder and the next run reaches the rest of the
// population.
//
// The pre-mark costs a healthy issuer nothing. SetIssuerSep1Payload sets
// sep1_consecutive_failures = 0 and sep1_next_attempt_after = NULL in
// the same statement that writes the payload, so a success erases the
// ladder step its own pre-mark took (proved against Postgres in
// test/integration/sep1_retry_backoff_test.go). And the mark happens
// exactly once per issuer per run, so the systemic-outage unwind — which
// takes back exactly one ladder step per failed key — still balances.
func refreshOneSep1Issuer(
	ctx context.Context, store sep1Store, resolver sep1Resolver,
	c timescale.IssuerSep1Candidate, dryRun bool,
) bool {
	markSep1Attempted(ctx, store, c.GStrkey, dryRun)

	// The fetch+parse runs on its own budget so a single domain cannot
	// consume the whole run's deadline; the write below deliberately uses
	// the run context, which outlives it.
	fetchCtx, cancel := context.WithTimeout(ctx, sep1PerIssuerBudget)
	defer cancel()
	sep, err := resolver.Resolve(fetchCtx, sep1FetchDomain(c.GStrkey, c.HomeDomain))
	if err != nil {
		fmt.Printf("FAIL  %s  %s  %v\n", c.GStrkey, c.HomeDomain, err)
		return false
	}
	// Bidirectional SEP-1 verification: the org is only "verified" if the
	// fetched toml's [[CURRENCIES]] lists THIS issuer back — i.e. the domain
	// owner attests to this account. Without it, anyone can set their
	// account's home_domain to a reputable domain and inherit its ORG_NAME
	// (spoofing). Callers MUST only merge/group issuers by org when this is
	// true; one-directional matches are "claimed, unverified".
	orgVerified := tomlListsIssuer(sep.Currencies, c.GStrkey)
	payload, jerr := marshalSep1Payload(sep, orgVerified)
	if jerr != nil {
		// Reachable from attacker-authored TOML: a NUL codepoint in a
		// string field marshals to an escape Postgres jsonb rejects. The
		// pre-mark has already taken this row off the head of the queue.
		// Cold audit 2026-08-03.
		fmt.Printf("FAIL  %s  marshal: %v\n", c.GStrkey, jerr)
		return false
	}
	if !dryRun {
		if err := store.SetIssuerSep1Payload(ctx, c.GStrkey, payload); err != nil {
			fmt.Printf("FAIL  %s  write: %v\n", c.GStrkey, err)
			return false
		}
	}
	fmt.Printf("OK    %s  %s  org=%q verified=%v\n", c.GStrkey, c.HomeDomain, sep.OrgName, orgVerified)
	return true
}

// sep1Candidates picks the run's work: one named issuer, or the head of
// the staleness queue. An empty slice with a nil error means "nothing to
// do" and the caller returns cleanly.
//
// The targeted path deliberately bypasses BOTH the staleness filter and
// the retry ladder — it is the operator's override for a domain the queue
// has deferred (a newly-onboarded org, or one that just fixed its TOML).
func sep1Candidates(
	ctx context.Context, store *timescale.Store, issuer string, olderThan time.Duration, limit int,
) ([]timescale.IssuerSep1Candidate, error) {
	if issuer != "" {
		c, err := store.IssuerSep1CandidateByStrkey(ctx, issuer)
		if err != nil {
			return nil, err
		}
		fmt.Printf("Refreshing 1 issuer (targeted: %s)…\n", issuer)
		return []timescale.IssuerSep1Candidate{c}, nil
	}
	candidates, err := store.IssuersNeedingSep1Refresh(ctx, olderThan, limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		fmt.Println("No issuers need refresh.")
		return nil, nil
	}
	fmt.Printf("Refreshing %d issuer(s) (older than %s)…\n", len(candidates), olderThan)
	return candidates, nil
}

// Systemic-outage guard.
//
// THE PROBLEM WITH A BACKOFF, stated plainly: it cannot tell "this
// domain is dead" from "our DNS is down". Both look like a failed
// fetch. Left alone, an outage on our side walks the WHOLE population
// up the ladder — six bad days is enough to reach the 30-day cap — and
// then the job goes quiet. Nothing catches it downstream either: the
// data-freshness watchdog reads `max(sep1_resolved_at)` over `issuers`,
// and a failed attempt stamps that column just as a success does, so
// the gauge stays green while the refresh is doing nothing useful.
//
// So the run judges ITSELF. A failure fraction this high over a sample
// this large is not a property of the issuer population; it is a
// property of the run. When that verdict lands the run does two things
// no silent backoff would:
//
//  1. Unwinds the ladder step it just applied to every domain it
//     failed, so a bad night leaves no trace on the schedule and
//     recovery is immediate rather than metered out over 30 days.
//  2. Returns an error, so the systemd oneshot enters `failed` and the
//     existing stellarindex_systemd_unit_failed alert (infra.yml, 15m)
//     tickets it. Loud beats quiet: the alternative is a job that
//     reports "0 succeeded, 750 failed" to a journal nobody reads and
//     exits 0.
//
// The threshold is calibrated against the measured baseline, not
// guessed. On r1, 2026-09-12, a healthy run failed 291 of 500 — 58%,
// because more than half of the population genuinely serves nothing.
// 90% is comfortably above anything the population can produce and
// comfortably below "everything is broken". minAttempts keeps a short
// run (a nearly-drained queue, a deadline-truncated batch, a targeted
// -issuer refresh) from tripping it on a handful of samples.
const (
	defaultSystemicFailureRate = 0.90
	systemicMinAttempts        = 50
)

// sep1RunVerdict reports whether a run's failures should be read as a
// fault on our side rather than on the issuers'. Pure, so the
// calibration is testable without a network or a database.
func sep1RunVerdict(ok, failed int, rate float64) bool {
	attempts := ok + failed
	if attempts < systemicMinAttempts || rate > 1 {
		return false
	}
	return float64(failed)/float64(attempts) >= rate
}

// reportSep1Systemic applies the systemic verdict: unwind, then fail.
func reportSep1Systemic(store *timescale.Store, attempts int, failedKeys []string, dryRun bool) error {
	fmt.Printf("\nSYSTEMIC: %d of %d attempts failed — reading this as a fault on OUR side, "+
		"not %d newly-dead domains.\n", len(failedKeys), attempts, len(failedKeys))
	if !dryRun {
		// The run's own context may already be past its deadline (that is
		// one of the ways a run ends up looking systemic), and the unwind
		// is the whole point of the verdict — it must not be skipped
		// because the budget that produced the verdict has expired.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) //nolint:contextcheck // deliberately detached: the expired run budget must not cancel the corrective write
		defer cancel()
		n, err := store.UnwindIssuerSep1Backoff(ctx, failedKeys)
		if err != nil {
			fmt.Printf("WARN  unwind-backoff: %v\n", err)
		} else {
			fmt.Printf("Unwound the retry backoff on %d issuer(s); their schedule is untouched.\n", n)
		}
	}
	return fmt.Errorf("sep1-refresh: %d of %d attempts failed — refusing to treat that as %d "+
		"dead domains; check DNS/egress from this host, then re-run",
		len(failedKeys), attempts, len(failedKeys))
}

// tomlListsIssuer reports whether the fetched SEP-1 toml's [[CURRENCIES]] lists
// the given issuer back — the bidirectional half of org verification. Without
// this match, ORG_NAME from a self-declared home_domain is spoofable.
// markSep1Attempted bumps sep1_resolved_at so an issuer under attempt
// moves to the BACK of the refresh queue, and advances its retry ladder
// so a domain that serves nothing stops costing an attempt a day.
//
// The queue is `ORDER BY sep1_resolved_at ASC NULLS FIRST`, so a row
// left NULL stays candidate #1 on every subsequent run. It is called
// exactly ONCE per issuer per run, BEFORE the fetch — see
// [refreshOneSep1Issuer] for why the ordering is the whole point, and
// why a success (which clears the ladder in the same statement that
// writes its payload) pays nothing for it. Best-effort: a failure to
// mark is logged, not fatal.
//
// The streak count is echoed so the journal shows WHY a domain went
// quiet. Without it a reader of a later run cannot tell a domain that
// was skipped from one that was never a candidate.
func markSep1Attempted(ctx context.Context, store sep1Store, gStrkey string, dryRun bool) {
	if dryRun {
		return
	}
	streak, err := store.MarkIssuerSep1Failed(ctx, gStrkey)
	if err != nil {
		fmt.Printf("WARN  %s  mark-attempted: %v\n", gStrkey, err)
		return
	}
	if streak > 1 {
		fmt.Printf("      %s  consecutive failures: %d\n", gStrkey, streak)
	}
}

func tomlListsIssuer(currencies []metadata.Currency, issuer string) bool {
	for _, cur := range currencies {
		if cur.Issuer == issuer {
			return true
		}
	}
	return false
}

// marshalSep1Payload builds the compact sep1_payload JSON persisted to the
// issuers row: OrgName/OrgVerified/Documentation for /v1/issuers, plus the
// per-currency overlay /v1/assets/{id} reads (that handler used to live-fetch
// per request; this cron is now the source of truth so it's a DB lookup). Raw
// + NetworkPassphrase are excluded — nothing reads them.
func marshalSep1Payload(sep *metadata.SEP1, orgVerified bool) ([]byte, error) {
	currencies := make([]map[string]any, 0, len(sep.Currencies))
	for _, c := range sep.Currencies {
		currencies = append(currencies, map[string]any{
			"Code":            c.Code,
			"Issuer":          c.Issuer,
			"Decimals":        c.Decimals,
			"DisplayDecimals": c.DisplayDecimals,
			"Name":            c.Name,
			"Description":     c.Description,
			"Conditions":      c.Conditions,
			"Image":           c.Image,
			"FixedNumber":     c.FixedNumber,
			"MaxNumber":       c.MaxNumber,
			"IsUnlimited":     c.IsUnlimited,
			"AnchorAsset":     c.AnchorAsset,
			"AnchorAssetType": c.AnchorAssetType,
			"Status":          c.Status,
		})
	}
	out := map[string]any{
		"OrgName":       sep.OrgName,
		"OrgVerified":   orgVerified,
		"Version":       sep.Version,
		"Documentation": sep.Documentation,
		"Currencies":    currencies,
		"FetchedAt":     sep.FetchedAt.UTC().Format(time.RFC3339),
	}
	// A document that did not parse whole travels with the list of
	// tables that were skipped. Without it the storage boundary erases
	// the difference between "the issuer declared nothing here" and
	// "we could not read the table that would have said", and every
	// consumer downstream of this row reads the first when the second
	// is true. Absent on the documents that parsed whole, which is
	// almost all of them, so no row grows that did not have to.
	if len(sep.RecoveredSections) > 0 {
		skipped := make([]map[string]any, 0, len(sep.RecoveredSections))
		for _, sec := range sep.RecoveredSections {
			skipped = append(skipped, map[string]any{"Header": sec.Header, "Err": sec.Err})
		}
		out["RecoveredSections"] = skipped
	}
	return json.Marshal(out)
}

// sep1FetchDomain returns the domain to fetch an issuer's TOML from:
// the curated override when one exists, else the on-chain
// home_domain. Split out for the funlen budget + direct testing.
func sep1FetchDomain(gStrkey, homeDomain string) string {
	if override, ok := sep1DomainOverrides[gStrkey]; ok {
		return override
	}
	return homeDomain
}
