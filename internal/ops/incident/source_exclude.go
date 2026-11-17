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

const readSideNote = "source-exclude: note: served prices do not read source_exclusions yet; this row takes effect when the read side ships"

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
	req, err := f.resolve()
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
		return listSourceExclusions(ctx, store)
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
	list, lift                                      *bool
	id                                              *int64
	source, base, quote, from, until, reason, actor *string
}

func registerSourceExcludeFlags(fs *flag.FlagSet) sourceExcludeFlags {
	return sourceExcludeFlags{
		list:   fs.Bool("list", false, "list every source_exclusions row, active and lifted, and exit without changing anything"),
		lift:   fs.Bool("lift", false, "lift the active exclusion named by -id (requires -reason); the row is kept"),
		id:     fs.Int64("id", 0, "source_exclusions id to lift (with -lift)"),
		source: fs.String("source", "", "source to exclude, as named in the source registry (e.g. binance, soroswap)"),
		base:   fs.String("base", "", "base asset of the one pair to exclude, canonical wire form (with -quote; omit both for every pair)"),
		quote:  fs.String("quote", "", "quote asset of the one pair to exclude, canonical wire form (with -base)"),
		from:   fs.String("from", "", "start of the excluded window, RFC3339 (required; may be in the past)"),
		until:  fs.String("until", "", "end of the excluded window, RFC3339 (optional; open-ended when omitted)"),
		reason: fs.String("reason", "", "why (required for a mutation; recorded in audit_log and on the row)"),
		actor:  fs.String("actor", "", "who; recorded in audit_log and on the row. Defaults to the OS user."),
	}
}

// sourceExcludeRequest is one validated invocation. base/quote are canonical asset strings in canonical.Orient order.
type sourceExcludeRequest struct {
	mode          sourceExcludeMode
	id            int64
	source        string
	base, quote   string
	flipped       bool
	from, until   time.Time
	reason, actor string
}

func (f sourceExcludeFlags) resolve() (sourceExcludeRequest, error) {
	switch {
	case *f.list && *f.lift:
		return sourceExcludeRequest{}, errors.New("-list and -lift are mutually exclusive")
	case *f.list:
		return sourceExcludeRequest{mode: modeList}, nil
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
		if *f.source != "" || *f.base != "" || *f.quote != "" || *f.from != "" || *f.until != "" {
			return sourceExcludeRequest{}, errors.New("-lift takes only -id, -reason and -actor")
		}
		return sourceExcludeRequest{mode: modeLift, id: *f.id, reason: reason, actor: actor}, nil
	}
	if *f.id != 0 {
		return sourceExcludeRequest{}, errors.New("-id is only valid with -lift")
	}
	req := sourceExcludeRequest{mode: modeCreate, source: *f.source, reason: reason, actor: actor}
	if err := validateSource(req.source); err != nil {
		return sourceExcludeRequest{}, err
	}
	if req.base, req.quote, req.flipped, err = resolvePair(*f.base, *f.quote); err != nil {
		return sourceExcludeRequest{}, err
	}
	if req.from, req.until, err = resolveWindow(*f.from, *f.until); err != nil {
		return sourceExcludeRequest{}, err
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
	ActiveSourceExclusions(ctx context.Context) ([]timescale.SourceExclusion, error)
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

func createSourceExclusion(ctx context.Context, audit keys.AuditSink, store sourceExclusionStore, req sourceExcludeRequest, dryRun bool) error {
	if req.flipped {
		fmt.Printf("source-exclude: pair stored in canonical order as %s/%s\n", req.base, req.quote)
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
	fmt.Println(readSideNote)
	return nil
}

func liftSourceExclusion(ctx context.Context, audit keys.AuditSink, store sourceExclusionStore, req sourceExcludeRequest, dryRun bool) error {
	active, err := store.ActiveSourceExclusions(ctx)
	if err != nil {
		return err
	}
	var row *timescale.SourceExclusion
	for i := range active {
		if active[i].ID == req.id {
			row = &active[i]
			break
		}
	}
	if row == nil {
		return fmt.Errorf("no active source exclusion with id %d (see -list)", req.id)
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
		return fmt.Errorf("lift source exclusion %d (audit row written): %w", req.id, err)
	}
	fmt.Printf("source-exclude: exclusion %d (%s) lifted; the row is kept\n", req.id, row.Source)
	return nil
}

type sourceExclusionLister interface {
	ListSourceExclusions(ctx context.Context, activeOnly bool) ([]timescale.SourceExclusion, error)
}

func listSourceExclusions(ctx context.Context, store sourceExclusionLister) error {
	rows, err := store.ListSourceExclusions(ctx, false)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("source-exclude: no source_exclusions rows")
		return nil
	}
	fmt.Printf("%-6s %-7s %-18s %-60s %-20s %-20s %-16s %s\n", "ID", "STATE", "SOURCE", "PAIR", "FROM", "UNTIL", "ACTOR", "REASON")
	for _, e := range rows {
		state, pair, until := "active", "*", "-"
		if !e.Active() {
			state = "lifted"
		}
		if e.Base != "" {
			pair = e.Base + "/" + e.Quote
		}
		if !e.Until.IsZero() {
			until = e.Until.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-6d %-7s %-18s %-60s %-20s %-20s %-16s %s\n",
			e.ID, state, e.Source, pair, e.From.UTC().Format(time.RFC3339), until, e.Actor, e.Reason)
	}
	return nil
}
