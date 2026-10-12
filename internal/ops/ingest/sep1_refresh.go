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
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1DomainOverrides maps issuers whose on-chain home_domain no longer serves a stellar.toml to the domain that does; the on-chain value stays authoritative for identity.
// The refresh loop is sequential on purpose: TOML parsing is superlinear on hostile input and the unit runs under MemoryMax=2G.
var sep1DomainOverrides = map[string]string{
	// USDC / EURC issuers — Circle (Centre) toml.
	"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "centre.io",
	"GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2": "centre.io",
}

func sep1RefreshCmd(args []string) error {
	fs := flag.NewFlagSet("sep1-refresh", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300",
		"ClickHouse native address; each issuer's on-chain home_domain is re-read here before its fetch")
	limit := fs.Int("limit", 100,
		fmt.Sprintf("Max issuers to refresh per run (1-%d)", timescale.Sep1RefreshMaxLimit))
	olderThan := fs.Duration("older-than", 24*time.Hour, "Skip issuers refreshed more recently than this")
	timeout := fs.Duration("timeout", 5*time.Minute, "Wall-clock timeout for the whole run")
	gate := opsutil.RegisterWriteGate(fs)
	issuer := fs.String("issuer", "", "Refresh ONLY this issuer G-strkey, bypassing the staleness queue")
	systemicRate := fs.Float64("systemic-failure-rate", defaultSystemicFailureRate,
		"Failure fraction, over domains that have served a stellar.toml before, at or above which a run is judged a fault on OUR side: "+
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

	chain, err := clickhouse.NewExplorerReader(ctx, *chAddr)
	if err != nil {
		return fmt.Errorf("sep1-refresh: clickhouse: %w", err)
	}
	defer func() { _ = chain.Close() }()

	resolver := metadata.NewResolver(metadata.Options{Timeout: 10 * time.Second})
	ok, failedKeys, ourFaultKeys := sep1RefreshLoop(ctx, store, chain, resolver, candidates, dryRun)
	failed := len(failedKeys)
	fmt.Printf("\n%d succeeded, %d failed\n", ok, failed)
	if dryRun {
		fmt.Println("(dry-run; no rows written)")
	}
	// The loop is sequential, so the attempted rows are a prefix even when the
	// deadline cut the batch short.
	if sep1RunVerdict(candidates[:ok+failed], failedKeys, *systemicRate) {
		return reportSep1Systemic(store, ok+failed, failedKeys, dryRun)
	}
	// A systemic run has unwound every failed key above, these included.
	unwindSep1Backoff(store, ourFaultKeys, dryRun)
	return nil
}

// sep1Store is the slice of the store the refresh loop writes, narrowed so the loop's ordering guarantees can be asserted without a
// database.
type sep1Store interface {
	MarkIssuerSep1Failed(ctx context.Context, gStrkey string) (int, error)
	SetIssuerSep1Payload(ctx context.Context, gStrkey, fetchedFrom string, payload []byte) (bool, error)
	SyncIssuerHomeDomain(ctx context.Context, gStrkey, homeDomain string) (bool, error)
	ClearIssuerHomeDomain(ctx context.Context, gStrkey string) (bool, error)
}

// sep1ChainReader is the lake seam each issuer's on-chain home_domain is
// re-read through before its fetch — clickhouse.ExplorerReader satisfies it.
type sep1ChainReader interface {
	BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
}

// sep1Resolver is the slice of [metadata.Resolver] the refresh loop
// uses.
type sep1Resolver interface {
	Resolve(ctx context.Context, domain string) (*metadata.SEP1, error)
}

// sep1PerIssuerBudget bounds one issuer's fetch and parse; the 10s per-request timeout does not see slow steady answers or long decodes.
const sep1PerIssuerBudget = 30 * time.Second

// sep1RefreshLoop resolves each candidate in turn, returning the success count, the keys of failed attempts and the subset that failed on our side.
// Sequential on purpose (parser cost is superlinear on hostile input under a 2G ceiling).
func sep1RefreshLoop(
	ctx context.Context, store sep1Store, chain sep1ChainReader, resolver sep1Resolver,
	candidates []timescale.IssuerSep1Candidate, dryRun bool,
) (ok int, failedKeys, ourFaultKeys []string) {
	failedKeys = make([]string, 0, len(candidates))
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			fmt.Printf("\nAborted at %d/%d (deadline): %v\n", ok+len(failedKeys), len(candidates), err)
			break
		}
		switch refreshOneSep1Issuer(ctx, store, chain, resolver, c, dryRun) {
		case sep1Stored:
			ok++
		case sep1OurFault:
			ourFaultKeys = append(ourFaultKeys, c.GStrkey)
			failedKeys = append(failedKeys, c.GStrkey)
		case sep1Failed:
			failedKeys = append(failedKeys, c.GStrkey)
		}
	}
	return ok, failedKeys, ourFaultKeys
}

// sep1Outcome is one issuer's attempt, split by whose fault a failure was.
type sep1Outcome int

const (
	// sep1Stored: a payload was written, or there was nothing to fetch.
	sep1Stored sep1Outcome = iota
	// sep1Failed: the domain served nothing usable; the ladder step stands.
	sep1Failed
	// sep1OurFault: the lake read or a store write failed; the domain was
	// never judged, so the ladder step is taken back.
	sep1OurFault
)

// refreshOneSep1Issuer fetches, parses and stores one issuer's stellar.toml, reporting whether a payload was written and whose fault a
// failure was. The attempt is marked FIRST: a hostile document can get the worker SIGKILLed before any on-failure mark, leaving the row
// at the queue head forever.
func refreshOneSep1Issuer(
	ctx context.Context, store sep1Store, chain sep1ChainReader, resolver sep1Resolver,
	c timescale.IssuerSep1Candidate, dryRun bool,
) sep1Outcome {
	c, binding := confirmSep1Domain(ctx, store, chain, c, dryRun)
	if binding == sep1DomainCleared {
		return sep1Stored
	}
	markSep1Attempted(ctx, store, c.GStrkey, dryRun)
	switch binding {
	case sep1DomainUnconfirmed:
		return sep1Failed
	case sep1DomainUnchecked:
		return sep1OurFault
	case sep1DomainConfirmed, sep1DomainCleared:
	}

	// The fetch+parse runs on its own budget so a single domain cannot
	// consume the whole run's deadline; the write below deliberately uses
	// the run context, which outlives it.
	fetchCtx, cancel := context.WithTimeout(ctx, sep1PerIssuerBudget)
	defer cancel()
	sep, err := resolver.Resolve(fetchCtx, sep1FetchDomain(c.GStrkey, c.HomeDomain))
	if err != nil {
		fmt.Printf("FAIL  %s  %s  %v\n", c.GStrkey, c.HomeDomain, err)
		return sep1Failed
	}
	// Bidirectional SEP-1 verification: the org is verified only if the toml's [[CURRENCIES]] lists THIS issuer back; otherwise anyone could claim a reputable home_domain and inherit its ORG_NAME.
	orgVerified := tomlListsIssuer(sep.Currencies, c.GStrkey)
	payload, jerr := marshalSep1Payload(sep, orgVerified)
	if jerr != nil {
		// Reachable from attacker-authored TOML: a NUL codepoint in a
		// string field marshals to an escape Postgres jsonb rejects. The
		// pre-mark has already taken this row off the head of the queue.
		fmt.Printf("FAIL  %s  marshal: %v\n", c.GStrkey, jerr)
		return sep1Failed
	}
	if !dryRun {
		stored, err := store.SetIssuerSep1Payload(ctx, c.GStrkey, c.HomeDomain, payload)
		if err != nil {
			fmt.Printf("FAIL  %s  write: %v\n", c.GStrkey, err)
			if errors.Is(err, timescale.ErrSep1PayloadRejected) {
				return sep1Failed
			}
			return sep1OurFault
		}
		if !stored {
			// home_domain changed mid-fetch. Not a failed attempt: the change
			// already reset this row's ladder, so the systemic unwind must not
			// take a step back from it.
			fmt.Printf("MOVED %s  %s  home_domain changed during the fetch; payload discarded\n", c.GStrkey, c.HomeDomain)
			return sep1Stored
		}
	}
	fmt.Printf("OK    %s  %s  org=%q verified=%v\n", c.GStrkey, c.HomeDomain, sep.OrgName, orgVerified)
	return sep1Stored
}

// sep1DomainBinding is what the chain says about a candidate's stored
// home_domain immediately before its fetch.
type sep1DomainBinding int

const (
	// sep1DomainConfirmed: the chain declares the (possibly re-bound) domain.
	sep1DomainConfirmed sep1DomainBinding = iota
	// sep1DomainCleared: the chain declares no domain; the row was cleared and
	// there is nothing to fetch.
	sep1DomainCleared
	// sep1DomainUnconfirmed: no live entry answered.
	sep1DomainUnconfirmed
	// sep1DomainUnchecked: the lake read or the re-bind failed on our side.
	sep1DomainUnchecked
)

// confirmSep1Domain re-reads the live home_domain and re-binds the row when the chain moved; an account with no live entry cannot be confirmed and is not fetched.
func confirmSep1Domain(
	ctx context.Context, store sep1Store, chain sep1ChainReader,
	c timescale.IssuerSep1Candidate, dryRun bool,
) (timescale.IssuerSep1Candidate, sep1DomainBinding) {
	live, err := chain.BulkAccountAuthFlags(ctx, []string{c.GStrkey})
	if err != nil {
		fmt.Printf("FAIL  %s  %s  chain read: %v\n", c.GStrkey, c.HomeDomain, err)
		return c, sep1DomainUnchecked
	}
	f, ok := live[c.GStrkey]
	if !ok || f.Source != clickhouse.AuthFlagsSourceLive {
		fmt.Printf("FAIL  %s  %s  no live account entry to confirm the domain against\n", c.GStrkey, c.HomeDomain)
		return c, sep1DomainUnconfirmed
	}
	if f.HomeDomain == c.HomeDomain {
		return c, sep1DomainConfirmed
	}
	if !dryRun {
		if f.HomeDomain == "" {
			_, err = store.ClearIssuerHomeDomain(ctx, c.GStrkey)
		} else {
			_, err = store.SyncIssuerHomeDomain(ctx, c.GStrkey, f.HomeDomain)
		}
		if err != nil {
			fmt.Printf("FAIL  %s  %s  re-bind to %q: %v\n", c.GStrkey, c.HomeDomain, f.HomeDomain, err)
			return c, sep1DomainUnchecked
		}
	}
	fmt.Printf("MOVED %s  %s -> %q  re-bound to the on-chain home_domain\n", c.GStrkey, c.HomeDomain, f.HomeDomain)
	if f.HomeDomain == "" {
		return c, sep1DomainCleared
	}
	c.HomeDomain = f.HomeDomain
	return c, sep1DomainConfirmed
}

// sep1Candidates picks one named issuer or the head of the staleness queue; empty with nil error means nothing to do.
// The targeted path bypasses the staleness filter and the retry ladder as the operator override.
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

// Systemic-outage guard: a failed attempt stamps sep1_resolved_at like a success, so the run judges itself only on domains that have served a toml (the row holds a payload).
// When it trips, the ladder step on every failed domain is unwound and the run errors so the systemd-failed alert fires; 90% sits above a healthy run's near-zero regression rate.
const (
	defaultSystemicFailureRate = 0.90
	systemicMinAttempts        = 50
)

// sep1RunVerdict reports whether a run's failures read as our fault rather than the issuers': the failure fraction over previously
// reached candidates. Pure.
func sep1RunVerdict(attempted []timescale.IssuerSep1Candidate, failedKeys []string, rate float64) bool {
	if rate > 1 {
		return false
	}
	failed := make(map[string]struct{}, len(failedKeys))
	for _, k := range failedKeys {
		failed[k] = struct{}{}
	}
	var reached, regressed int
	for _, c := range attempted {
		if !c.Reached {
			continue
		}
		reached++
		if _, ok := failed[c.GStrkey]; ok {
			regressed++
		}
	}
	if reached < systemicMinAttempts {
		return false
	}
	return float64(regressed)/float64(reached) >= rate
}

// reportSep1Systemic applies the systemic verdict: unwind, then fail.
func reportSep1Systemic(store *timescale.Store, attempts int, failedKeys []string, dryRun bool) error {
	fmt.Printf("\nSYSTEMIC: %d of %d attempts failed — reading this as a fault on OUR side, "+
		"not %d newly-dead domains.\n", len(failedKeys), attempts, len(failedKeys))
	unwindSep1Backoff(store, failedKeys, dryRun)
	return fmt.Errorf("sep1-refresh: %d of %d attempts failed — refusing to treat that as %d "+
		"dead domains; check DNS/egress from this host, then re-run",
		len(failedKeys), attempts, len(failedKeys))
}

// unwindSep1Backoff takes back the ladder step this run applied to keys.
func unwindSep1Backoff(store *timescale.Store, keys []string, dryRun bool) {
	if dryRun || len(keys) == 0 {
		return
	}
	// The run's own context may already be past its deadline (that is
	// one of the ways a run ends up looking systemic), and the unwind
	// is corrective — it must not be skipped because the budget that
	// produced the failures has expired.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, err := store.UnwindIssuerSep1Backoff(ctx, keys)
	if err != nil {
		fmt.Printf("WARN  unwind-backoff: %v\n", err)
		return
	}
	fmt.Printf("Unwound the retry backoff on %d issuer(s); their schedule is untouched.\n", n)
}

// markSep1Attempted bumps sep1_resolved_at and advances the retry ladder, called once per issuer per run BEFORE the fetch (see refreshOneSep1Issuer).
// Best-effort: a failure to mark is logged, not fatal.
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

// tomlListsIssuer reports whether the toml's [[CURRENCIES]] lists the issuer back, the bidirectional half of org verification.
// It binds via timescale.Sep1EntryBindsTo, the same canonicalisation the other readers use; a byte-exact compare would disagree with them.
func tomlListsIssuer(currencies []metadata.Currency, issuer string) bool {
	for _, cur := range currencies {
		if timescale.Sep1EntryBindsTo(cur.Issuer, issuer) {
			return true
		}
	}
	return false
}

// marshalSep1Payload builds the compact sep1_payload JSON stored on the issuers row (org fields plus the per-currency overlay /v1/assets/{id} reads); Raw is excluded.
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
			// nil marshals to null, which reads back as "not declared".
			"IsAssetAnchored":        c.IsAssetAnchored,
			"AttestationOfReserve":   c.AttestationOfReserve,
			"RedemptionInstructions": c.RedemptionInstructions,
			"Regulated":              c.Regulated,
			"ApprovalServer":         c.ApprovalServer,
			"ApprovalCriteria":       c.ApprovalCriteria,
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
	// A document that did not parse whole carries its skipped tables, so storage does not turn "could not read the table" into "issuer declared nothing". Absent on whole parses.
	if len(sep.RecoveredSections) > 0 {
		skipped := make([]map[string]any, 0, len(sep.RecoveredSections))
		for _, sec := range sep.RecoveredSections {
			skipped = append(skipped, map[string]any{"Header": sec.Header, "Err": sec.Err})
		}
		out["RecoveredSections"] = skipped
	}
	return json.Marshal(out)
}

// sep1FetchDomain returns the domain to fetch an issuer's TOML from: the curated override if any, else the on-chain home_domain.
func sep1FetchDomain(gStrkey, homeDomain string) string {
	if override, ok := sep1DomainOverrides[gStrkey]; ok {
		return override
	}
	return homeDomain
}
