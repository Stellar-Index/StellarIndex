package incident

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/keys"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const readSideNote = "source-exclude: note:\n" +
	"  WITHHELD (404 reason source_excluded; tip, /v1/vwap, /v1/twap and the headline aggregator tier recompute without the source):\n" +
	"    /v1/price (closed, swapped-in, last-trade, cached, stablecoin proxy, USD-leg cross, frozen held, FX conversions), /v1/price/batch,\n" +
	"    /v1/price/tip + stream, closed-bucket price stream, SEP-40, /v1/vwap, /v1/twap, /v1/ohlc (a covered series bar\n" +
	"    withholds the series; a fiat series is judged on each stablecoin market it pools), asset headline (three tiers, global-market\n" +
	"    reference; never falls back to the listing price; a declared USD peg does not replace it), /v1/price/at and\n" +
	"    /v1/price/changes (a covered bucket withholds the point; a covered horizon reads withheld:true), /v1/history (a covered\n" +
	"    fill is removed from its page, served degraded and uncached; paging continues past the window).\n" +
	"  BLUNT: cached values record no sources. ANY exclusion, pair-scoped too, withholds every cached multi-hop price and every\n" +
	"    closed-bucket stream; a pair-scoped one also withholds every cached direct price whose base is either leg; a source-wide one\n" +
	"    withholds every cache-served price. Use the narrowest exclusion, and expect wide withholding of default-quote prices anyway.\n" +
	"    /v1/price/at and /changes withhold a bucket WHOLE when an excluded source fed it (its recorded sources; a source-wide\n" +
	"    exclusion only where that venue traded) and the bucket's span overlaps the window, so a bucket wider than the window is\n" +
	"    withheld whole: 1m for instants under 48h old, hourly up to 45 days, daily beyond; /changes 7d/30d read hourly/4-hourly\n" +
	"    buckets. A bucket with no recorded sources is withheld by any exclusion naming the asset, or any source-wide one.\n" +
	"    A source-wide exclusion with no -until withholds every /v1/ohlc series with a bar containing that source until it is lifted.\n" +
	"  DELAY: re-read every 15s; an unreadable table keeps the last good set up to 60s, then withholds everything gated.\n" +
	"    /v1/price, batch, SEP-40, vwap, twap 45s (90s while unreadable); tip 15s; tip stream +1 tick; asset detail ~225s;\n" +
	"    /v1/price/at, /changes, /v1/history, /v1/ohlc: the 15s re-read plus a 30s client / 5s CDN cache.\n" +
	"  NOT COVERED (may still include the source until lifted or aged out): /depeg, since-inception,\n" +
	"    chart, asset listing price_usd, sparkline, ATH, transitive fill, RWA assets/history/premium, fiat-asset headline and\n" +
	"    non-USD declared-peg FX, /v1/observations + stream, /v1/oracle/latest, /v1/oracle/streams, /v1/markets, /v1/pairs,\n" +
	"    /v1/protocols + tvl USD, /v1/lending/* USD (catalogue fallback), /v1/stablecoins, /v1/external/assets, /v1/aggregators,\n" +
	"    /v1/divergence, /v1/mev, /v1/assets/verified, /v1/anomalies, /v1/sources, /v1/pools, account usd_value, trades usd_volume,\n" +
	"    graph USD, /v1/accounts usd_value + priced_assets, volume_24h_usd on /v1/network/stats, /v1/sources/{name}/health and\n" +
	"    /v1/diagnostics/ingestion, /v1/changes/{type}/{id} values + ATH/ATL, and the venue-native mid and offer prices of\n" +
	"    /v1/pools/reserves, /v1/liquidity-pools and /v1/sdex/orderbook. No price: positions, /v1/search.\n" +
	"  Check: curl /v1/price for an affected pair, expect 404 reason source_excluded. Lift: source-exclude -lift -id N -reason ... -write"

// maxFutureFrom bounds how far ahead -from may start without -allow-future: a guard against year typos.
const maxFutureFrom = 24 * time.Hour

// SourceExclude records, lists or lifts an operator exclusion of a price source in source_exclusions; it never writes
// trades. Dry-run unless -write; the audit_log row is written before the table changes.
func SourceExclude(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("source-exclude")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	f := registerSourceExcludeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	req, err := f.resolve(time.Now().UTC(), gate.Enabled())
	if err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	if req.mode == modeList {
		return listSourceExclusions(ctx, store, req.now)
	}
	gate.Banner()
	audit := postgresstore.NewAuditStore(postgresstore.New(store.DB()))
	if req.mode == modeLift {
		return liftSourceExclusion(ctx, audit, store, req, gate.DryRun())
	}
	return createSourceExclusion(ctx, audit, store, req, gate.DryRun())
}

type sourceExcludeMode int

const (
	modeCreate sourceExcludeMode = iota
	modeList
	modeLift
)

type sourceExcludeFlags struct {
	list, lift, allowOverlap, allowFuture           *bool
	id                                              *int64
	source, base, quote, from, until, reason, actor *string
}

func registerSourceExcludeFlags(fs *flag.FlagSet) sourceExcludeFlags {
	return sourceExcludeFlags{
		list:         fs.Bool("list", false, "list every source_exclusions row with its state now (pending, in-force, expired, lifted) and exit; takes no other flag but -config and -actor"),
		lift:         fs.Bool("lift", false, "lift the un-lifted exclusion named by -id (requires -reason); the row is kept"),
		id:           fs.Int64("id", 0, "source_exclusions id to lift (with -lift)"),
		source:       fs.String("source", "", "source to exclude, as named in the source registry (e.g. binance, soroswap)"),
		base:         fs.String("base", "", "base asset of the one pair to exclude, canonical wire form (with -quote; omit both for every pair). Stored in canonical orientation; readers match both directions and every alias form"),
		quote:        fs.String("quote", "", "quote asset of the one pair to exclude, canonical wire form (with -base)"),
		from:         fs.String("from", "", "start of the excluded window, RFC3339 (required; may be in the past, at most 24h ahead without -allow-future)"),
		until:        fs.String("until", "", "end of the excluded window, RFC3339 (optional; open-ended when omitted)"),
		reason:       fs.String("reason", "", "why (required for a mutation; recorded in audit_log and on the row)"),
		actor:        fs.String("actor", "", "who; recorded in audit_log and on the row. Defaults to the OS user."),
		allowOverlap: fs.Bool("allow-overlap", false, "record the exclusion even though un-lifted exclusions of the same source overlap its scope and window"),
		allowFuture:  fs.Bool("allow-future", false, "allow -from more than 24h in the future"),
	}
}

// sourceExcludeRequest is one validated invocation. base/quote are canonical asset strings in canonical.Orient order.
type sourceExcludeRequest struct {
	mode          sourceExcludeMode
	now           time.Time
	id            int64
	source        string
	base, quote   string
	flipped       bool
	from, until   time.Time
	reason, actor string
	allowOverlap  bool
}

// mutationFlagSet reports whether any flag that only a create or lift reads was passed.
func (f sourceExcludeFlags) mutationFlagSet() bool {
	return *f.source != "" || *f.base != "" || *f.quote != "" || *f.from != "" || *f.until != "" ||
		*f.reason != "" || *f.id != 0 || *f.allowOverlap || *f.allowFuture
}

func (f sourceExcludeFlags) resolve(now time.Time, write bool) (sourceExcludeRequest, error) {
	switch {
	case *f.list && *f.lift:
		return sourceExcludeRequest{}, errors.New("-list and -lift are mutually exclusive")
	case *f.list:
		if write || f.mutationFlagSet() {
			return sourceExcludeRequest{}, errors.New("-list only reads: drop -source, -base, -quote, -from, -until, -reason, -id, -allow-overlap, -allow-future and -write")
		}
		return sourceExcludeRequest{mode: modeList, now: now}, nil
	}
	reason := strings.TrimSpace(*f.reason)
	if reason == "" {
		return sourceExcludeRequest{}, errors.New("-reason is required: an exclusion changes which sources may price an asset, so the record has to say why")
	}
	if err := keys.ValidateReason(reason); err != nil {
		return sourceExcludeRequest{}, err
	}
	actor, err := opsutil.ResolveActor(*f.actor)
	if err != nil {
		return sourceExcludeRequest{}, err
	}
	if *f.lift {
		if *f.id <= 0 {
			return sourceExcludeRequest{}, errors.New("-lift requires -id N (see -list)")
		}
		if *f.source != "" || *f.base != "" || *f.quote != "" || *f.from != "" || *f.until != "" || *f.allowOverlap || *f.allowFuture {
			return sourceExcludeRequest{}, errors.New("-lift takes only -id, -reason and -actor")
		}
		return sourceExcludeRequest{mode: modeLift, now: now, id: *f.id, reason: reason, actor: actor}, nil
	}
	if *f.id != 0 {
		return sourceExcludeRequest{}, errors.New("-id is only valid with -lift")
	}
	return f.resolveCreate(sourceExcludeRequest{mode: modeCreate, now: now, source: *f.source, reason: reason, actor: actor, allowOverlap: *f.allowOverlap})
}

func (f sourceExcludeFlags) resolveCreate(req sourceExcludeRequest) (sourceExcludeRequest, error) {
	var err error
	if err := validateSource(req.source); err != nil {
		return sourceExcludeRequest{}, err
	}
	if req.base, req.quote, req.flipped, err = resolvePair(*f.base, *f.quote); err != nil {
		return sourceExcludeRequest{}, err
	}
	if req.from, req.until, err = resolveWindow(*f.from, *f.until); err != nil {
		return sourceExcludeRequest{}, err
	}
	if req.from.After(req.now.Add(maxFutureFrom)) && !*f.allowFuture {
		return sourceExcludeRequest{}, fmt.Errorf("-from %s is more than 24h ahead of now (%s); check the date, or pass -allow-future if it is meant",
			req.from.Format(time.RFC3339), req.now.Format(time.RFC3339))
	}
	return req, nil
}

// validateSource accepts only names in external.Registry, the table the aggregator looks every source up in.
func validateSource(source string) error {
	if source == "" {
		return errors.New("-source is required")
	}
	if external.Registered(source) {
		return nil
	}
	names := make([]string, 0, len(external.Registry))
	for name := range external.Registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("-source %q is not a registered source; valid: %s", source, strings.Join(names, ", "))
}

// resolvePair parses both legs and stores them in canonical.Orient order, so one pair has one stored spelling
// whichever way the operator typed it. Identities are kept as parsed: no stablecoin or alias folding.
func resolvePair(baseFlag, quoteFlag string) (base, quote string, flipped bool, err error) {
	if baseFlag == "" && quoteFlag == "" {
		return "", "", false, nil
	}
	if baseFlag == "" || quoteFlag == "" {
		return "", "", false, errors.New("-base and -quote go together: pass both for one pair, or neither for every pair of the source")
	}
	b, err := canonical.ParseAsset(baseFlag)
	if err != nil {
		return "", "", false, fmt.Errorf("-base %q: %w", baseFlag, err)
	}
	q, err := canonical.ParseAsset(quoteFlag)
	if err != nil {
		return "", "", false, fmt.Errorf("-quote %q: %w", quoteFlag, err)
	}
	if b.String() == q.String() {
		return "", "", false, errors.New("-base and -quote name the same asset")
	}
	base, quote, flipped = canonical.Orient(b.String(), q.String())
	return base, quote, flipped, nil
}

func resolveWindow(fromFlag, untilFlag string) (from, until time.Time, err error) {
	if fromFlag == "" {
		return time.Time{}, time.Time{}, errors.New("-from is required (RFC3339, e.g. 2026-11-17T09:00:00Z)")
	}
	if from, err = time.Parse(time.RFC3339, fromFlag); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-from %q: want RFC3339: %w", fromFlag, err)
	}
	if untilFlag == "" {
		return from.UTC(), time.Time{}, nil
	}
	if until, err = time.Parse(time.RFC3339, untilFlag); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-until %q: want RFC3339: %w", untilFlag, err)
	}
	if !until.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("-until %s must be after -from %s", untilFlag, fromFlag)
	}
	return from.UTC(), until.UTC(), nil
}

// sourceExclusionStore is the slice of *timescale.Store the command mutates through, a seam for the ordering tests.
type sourceExclusionStore interface {
	InsertSourceExclusion(ctx context.Context, e timescale.SourceExclusion) (int64, error)
	LiftSourceExclusion(ctx context.Context, id int64, liftedBy, liftReason string) error
	UnliftedSourceExclusions(ctx context.Context) ([]timescale.SourceExclusion, error)
}

func (r sourceExcludeRequest) scope() string {
	if r.base == "" {
		return r.source + " (every pair)"
	}
	return r.source + " " + r.base + "/" + r.quote
}

func (r sourceExcludeRequest) window() string {
	until := "open-ended"
	if !r.until.IsZero() {
		until = r.until.Format(time.RFC3339)
	}
	return r.from.Format(time.RFC3339) + " .. " + until
}

// overlaps reports whether e is the same source as r with an intersecting pair scope and window.
func (r sourceExcludeRequest) overlaps(e timescale.SourceExclusion) bool {
	if e.Source != r.source || (r.base != "" && !e.CoversPair(r.base, r.quote)) {
		return false
	}
	return (r.until.IsZero() || e.From.Before(r.until)) && (e.Until.IsZero() || r.from.Before(e.Until))
}

func createSourceExclusion(ctx context.Context, audit keys.AuditSink, store sourceExclusionStore, req sourceExcludeRequest, dryRun bool) error {
	if req.flipped {
		fmt.Printf("source-exclude: pair stored in canonical order as %s/%s\n", req.base, req.quote)
	}
	unlifted, err := store.UnliftedSourceExclusions(ctx)
	if err != nil {
		return err
	}
	var clash []timescale.SourceExclusion
	for _, e := range unlifted {
		if req.overlaps(e) {
			clash = append(clash, e)
		}
	}
	if len(clash) > 0 {
		fmt.Printf("source-exclude: %d un-lifted exclusion(s) of %s overlap %s over %s:\n", len(clash), req.source, req.scope(), req.window())
		printSourceExclusions(clash, req.now)
		if !req.allowOverlap {
			return fmt.Errorf("refusing to record an overlapping exclusion of %s: lift the rows above, or pass -allow-overlap", req.source)
		}
	}
	if dryRun {
		fmt.Printf("source-exclude: DRY RUN — would audit and exclude %s over %s (actor: %s, reason: %s)\n",
			req.scope(), req.window(), req.actor, req.reason)
		return nil
	}
	target := req.source
	if req.base != "" {
		target += ":" + req.base + "/" + req.quote
	}
	detail := map[string]any{"from": req.from.Format(time.RFC3339)}
	if !req.until.IsZero() {
		detail["until"] = req.until.Format(time.RFC3339)
	}
	if err := keys.AppendOpsAudit(ctx, audit, "source.exclude", "source-exclude", "source_exclusion", target, req.actor, req.reason, detail); err != nil {
		return fmt.Errorf("audit_log append for %s failed, so NOTHING was changed: %w", target, err)
	}
	id, err := store.InsertSourceExclusion(ctx, timescale.SourceExclusion{
		Source: req.source, Base: req.base, Quote: req.quote,
		From: req.from, Until: req.until, Reason: req.reason, Actor: req.actor,
	})
	if err != nil {
		return fmt.Errorf("insert source exclusion for %s (audit row written, table unchanged): %w", target, err)
	}
	fmt.Printf("source-exclude: exclusion %d recorded: %s over %s\n", id, req.scope(), req.window())
	// The id only exists after the insert; this row joins the create audit row to the lift's id:N rows.
	idTarget := "id:" + strconv.FormatInt(id, 10)
	if err := keys.AppendOpsAudit(ctx, audit, "source.exclude.recorded", "source-exclude", "source_exclusion", idTarget, req.actor, req.reason,
		map[string]any{"scope": target}); err != nil {
		return fmt.Errorf("exclusion %d recorded, but its %s audit_log row failed: %w", id, idTarget, err)
	}
	fmt.Println(readSideNote)
	return nil
}

func liftSourceExclusion(ctx context.Context, audit keys.AuditSink, store sourceExclusionStore, req sourceExcludeRequest, dryRun bool) error {
	unlifted, err := store.UnliftedSourceExclusions(ctx)
	if err != nil {
		return err
	}
	var row *timescale.SourceExclusion
	for i := range unlifted {
		if unlifted[i].ID == req.id {
			row = &unlifted[i]
			break
		}
	}
	if row == nil {
		return fmt.Errorf("no un-lifted source exclusion with id %d (see -list)", req.id)
	}
	if dryRun {
		fmt.Printf("source-exclude: DRY RUN — would audit and lift exclusion %d (%s) (actor: %s, reason: %s)\n",
			req.id, row.Source, req.actor, req.reason)
		return nil
	}
	target := "id:" + strconv.FormatInt(req.id, 10)
	if err := keys.AppendOpsAudit(ctx, audit, "source.exclude.lift", "source-exclude", "source_exclusion", target, req.actor, req.reason,
		map[string]any{"source": row.Source}); err != nil {
		return fmt.Errorf("audit_log append for %s failed, so NOTHING was changed: %w", target, err)
	}
	if err := store.LiftSourceExclusion(ctx, req.id, req.actor, req.reason); err != nil {
		if errors.Is(err, timescale.ErrSourceExclusionLifted) {
			return fmt.Errorf("lift source exclusion %d: another lift won after this run checked it; the row is lifted, but not by this run (its audit row was written): %w", req.id, err)
		}
		return fmt.Errorf("lift source exclusion %d (audit row written): %w", req.id, err)
	}
	fmt.Printf("source-exclude: exclusion %d (%s) lifted; the row is kept\n", req.id, row.Source)
	return reportRemaining(ctx, store, row.Source, req.now)
}

// reportRemaining prints the source's other un-lifted rows, so a lift is not mistaken for un-excluding the source.
func reportRemaining(ctx context.Context, store sourceExclusionStore, source string, now time.Time) error {
	unlifted, err := store.UnliftedSourceExclusions(ctx)
	if err != nil {
		return fmt.Errorf("lifted, but listing the remaining exclusions of %s failed: %w", source, err)
	}
	var rest []timescale.SourceExclusion
	for _, e := range unlifted {
		if e.Source == source {
			rest = append(rest, e)
		}
	}
	if len(rest) == 0 {
		fmt.Printf("source-exclude: no un-lifted exclusions of %s remain\n", source)
		return nil
	}
	fmt.Printf("source-exclude: %s still has %d un-lifted exclusion(s):\n", source, len(rest))
	printSourceExclusions(rest, now)
	return nil
}

type sourceExclusionLister interface {
	ListSourceExclusions(ctx context.Context, unliftedOnly bool) ([]timescale.SourceExclusion, error)
}

func listSourceExclusions(ctx context.Context, store sourceExclusionLister, now time.Time) error {
	rows, err := store.ListSourceExclusions(ctx, false)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("source-exclude: no source_exclusions rows")
		return nil
	}
	printSourceExclusions(rows, now)
	return nil
}

// printSourceExclusions writes rows as a table, STATE evaluated at now.
func printSourceExclusions(rows []timescale.SourceExclusion, now time.Time) {
	fmt.Printf("%-6s %-8s %-18s %-60s %-20s %-20s %-16s %s\n", "ID", "STATE", "SOURCE", "PAIR", "FROM", "UNTIL", "ACTOR", "REASON")
	for _, e := range rows {
		pair, until := "*", "-"
		if e.Base != "" {
			pair = e.Base + "/" + e.Quote
		}
		if !e.Until.IsZero() {
			until = e.Until.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-6d %-8s %-18s %-60s %-20s %-20s %-16s %s\n",
			e.ID, e.State(now), e.Source, pair, e.From.UTC().Format(time.RFC3339), until, e.Actor, e.Reason)
	}
}
