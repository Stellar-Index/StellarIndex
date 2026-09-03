package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// issuerFlagsStore is the served-tier seam the drain writes through —
// timescale.Store satisfies it.
type issuerFlagsStore interface {
	IssuerGStrkeysNeedingFlags(ctx context.Context, limit int) ([]string, error)
	IssuerGStrkeysNeedingRecheck(ctx context.Context, limit int) ([]string, error)
	PersistIssuerAuthFlags(ctx context.Context, flags []timescale.IssuerAuthFlags) (int, error)
}

// issuerFlagsReader is the lake seam the drain reads through —
// clickhouse.ExplorerReader satisfies it.
type issuerFlagsReader interface {
	BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
	RemovedAccountsLastKnownAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
}

// issuerFlagsCounts is one run's accounting. Every candidate the drain is
// handed lands in exactly one of resolvedLive / resolvedLastKnown / absent,
// so `resolved* + absent == candidates` is checkable by the operator rather
// than inferred from silence.
type issuerFlagsCounts struct {
	candidates        int // rows the primary queue offered
	resolvedLive      int // a live AccountEntry in the lake
	resolvedLastKnown int // merged away; recovered from the pre-image
	absent            int // neither — outside the lake's captured window
	written           int // rows actually changed in Postgres

	recheckCandidates int // rows the re-check queue offered
	recheckSeen       int // rows the re-check pass actually examined
	revived           int // last-known rows whose account is live again
}

// issuerFlagsCmd persists issuer AccountEntry auth flags into the
// `issuers` table, so the API's read-time enrichment has a durable
// fallback.
//
// The flags ALREADY resolve on the read path
// (Server.enrichIssuerFromAccountState decodes them from the lake per
// request, 39 of the top 40 issuers measured 2026-08-27). What that path
// cannot survive is a cold account-state cache: under burst the refresh
// gate degrades and an issuer page renders "not yet resolved". The
// Postgres columns were created for exactly this fallback in migration
// 0023 and have never been populated — 0 of 59,189.
//
// So this is durability work, not a missing capability, and it is
// deliberately incremental: -limit bounds a run, and the queue is
// "auth_required IS NULL" ordered by primary key, so repeated bounded
// runs make forward progress instead of re-walking the same head.
//
// # MERGED ISSUERS (#374)
//
// A live-entry read alone leaves every issuer that has MERGED ITS ACCOUNT
// AWAY permanently unresolved — r1 2026-09-03: 10,239 of 59,241, and a
// 1,000-key sample says 985 (98.5%) are merged accounts, not coverage gaps.
// Their flags ARE knowable, so a miss now falls through to
// RemovedAccountsLastKnownAuthFlags, which recovers the pre-image the
// account_merge left in the removing ledger. Such a reading is persisted
// with its provenance (`last_known_before_removal` + the removal ledger)
// because it is a historical record, not the issuer's current authorisation
// policy, and WITHOUT the account's self-declared home_domain, which can no
// longer be checked against SEP-1.
//
// A second pass then re-checks the rows already labelled that way, so an
// account re-created at the same address flips back to `live`. Without it
// the provenance column would be a one-way latch: those rows have
// auth_required set, so the primary queue can never see them again.
func issuerFlagsCmd(args []string) error {
	fs := flag.NewFlagSet("issuer-flags", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	limit := fs.Int("limit", 5000, "Max issuers per pass this run — bounds the unresolved queue AND the last-known re-check queue independently (<=0 = every candidate)")
	batch := fs.Int("batch", 500, "Issuers per ClickHouse query")
	timeout := fs.Duration("timeout", 30*time.Minute, "Wall-clock timeout for the whole run")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *batch <= 0 {
		return errors.New("-batch must be positive")
	}
	dryRun := !gate.Banner()

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

	reader, err := clickhouse.NewExplorerReader(ctx, *chAddr)
	if err != nil {
		return fmt.Errorf("issuer-flags: clickhouse: %w", err)
	}

	return runIssuerFlags(ctx, store, reader, issuerFlagsOpts{
		limit:  *limit,
		batch:  *batch,
		dryRun: dryRun,
		out:    os.Stderr,
	})
}

type issuerFlagsOpts struct {
	limit  int
	batch  int
	dryRun bool
	out    io.Writer
}

// runIssuerFlags is the drain proper, split from the flag/config wiring so
// the resolve → fall back → persist → re-check loop is testable against
// stub seams.
func runIssuerFlags(ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader, o issuerFlagsOpts) error {
	var c issuerFlagsCounts

	candidates, err := store.IssuerGStrkeysNeedingFlags(ctx, o.limit)
	if err != nil {
		return err
	}
	c.candidates = len(candidates)
	_, _ = fmt.Fprintf(o.out, "issuer-flags: %d issuer(s) with unresolved flags\n", c.candidates)

	if err := issuerFlagsResolvePass(ctx, store, reader, o, candidates, &c); err != nil {
		return err
	}
	if err := issuerFlagsRecheckPass(ctx, store, reader, o, &c); err != nil {
		return err
	}

	// Self-accounting: every candidate lands in exactly one bucket, so an
	// operator can check the arithmetic rather than trust the summary. A
	// short fall means the run stopped early on its timeout — which is not
	// an error, the queues are resumable by construction.
	_, _ = fmt.Fprintf(o.out,
		"issuer-flags: processed %d of %d candidate(s) — resolved_live=%d resolved_last_known=%d absent=%d written=%d (dry-run=%v)\n",
		c.resolvedLive+c.resolvedLastKnown+c.absent, c.candidates,
		c.resolvedLive, c.resolvedLastKnown, c.absent, c.written, o.dryRun)
	_, _ = fmt.Fprintf(o.out,
		"issuer-flags: re-check processed %d of %d last-known row(s) — revived_to_live=%d still_merged=%d\n",
		c.recheckSeen, c.recheckCandidates, c.revived, c.recheckSeen-c.revived)
	// `absent` is expected and not a failure: an issuer merged before the
	// current-state projection's floor has no `removed` row to recover a
	// pre-image from, and one whose account entry is outside the lake's
	// captured window simply keeps rendering "not yet resolved" until it is
	// captured. Reported so the operator can see coverage rather than infer
	// it from silence.
	return nil
}

// issuerFlagsResolvePass fills the unresolved queue: a live AccountEntry
// where one exists, otherwise the last-known pre-image of a merged account.
func issuerFlagsResolvePass(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, candidates []string, c *issuerFlagsCounts,
) error {
	for start := 0; start < len(candidates); start += o.batch {
		if ctx.Err() != nil {
			// Timed out mid-walk. NOT an error: the queue is
			// resumable by construction, so a bounded run that stops
			// early has still made progress.
			_, _ = fmt.Fprintf(o.out, "issuer-flags: timeout reached — stopping early (queue is resumable)\n")
			break
		}
		end := min(start+o.batch, len(candidates))
		if err := issuerFlagsResolveChunk(ctx, store, reader, o, candidates[start:end], c); err != nil {
			return err
		}
	}
	return nil
}

// issuerFlagsResolveChunk resolves one batch: the live reader first, then the
// last-known reader on whatever it missed.
func issuerFlagsResolveChunk(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, chunk []string, c *issuerFlagsCounts,
) error {
	live, err := reader.BulkAccountAuthFlags(ctx, chunk)
	if err != nil {
		return err
	}
	// Only the misses go to the fallback. Ordering matters and is not an
	// optimisation: a live AccountEntry is the authority on its own account,
	// so an account re-created at an address that was once merged must
	// resolve `live`, never to its own pre-image.
	misses := make([]string, 0, len(chunk)-len(live))
	for _, g := range chunk {
		if _, ok := live[g]; !ok {
			misses = append(misses, g)
		}
	}
	lastKnown, err := reader.RemovedAccountsLastKnownAuthFlags(ctx, misses)
	if err != nil {
		return err
	}

	c.resolvedLive += len(live)
	c.resolvedLastKnown += len(lastKnown)
	c.absent += len(misses) - len(lastKnown)

	// Built by walking the chunk rather than ranging the maps, so the persist
	// order is deterministic and a key can only be taken from one of the two
	// readers.
	rows := make([]timescale.IssuerAuthFlags, 0, len(live)+len(lastKnown))
	for _, g := range chunk {
		f, ok := live[g]
		if !ok {
			if f, ok = lastKnown[g]; !ok {
				continue
			}
		}
		rows = append(rows, issuerAuthFlagsRow(g, f))
	}
	if o.dryRun {
		return nil
	}
	n, err := store.PersistIssuerAuthFlags(ctx, rows)
	if err != nil {
		return err
	}
	c.written += n
	return nil
}

// issuerFlagsRecheckPass re-offers every persisted `last_known_before_removal`
// row to the LIVE reader, so an account re-created at the same address is
// relabelled `live` with its current flags.
//
// Only live hits are re-persisted. A row whose account is still merged has
// not changed — its removal ledger is fixed — so re-writing it would be
// ~10k no-op UPDATEs per run for nothing.
func issuerFlagsRecheckPass(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, c *issuerFlagsCounts,
) error {
	candidates, err := store.IssuerGStrkeysNeedingRecheck(ctx, o.limit)
	if err != nil {
		return err
	}
	c.recheckCandidates = len(candidates)
	if len(candidates) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(o.out, "issuer-flags: re-checking %d last-known row(s) for re-creation\n", len(candidates))

	for start := 0; start < len(candidates); start += o.batch {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(o.out, "issuer-flags: timeout reached during re-check — stopping early (queue is resumable)\n")
			break
		}
		end := min(start+o.batch, len(candidates))
		if err := issuerFlagsRecheckChunk(ctx, store, reader, o, candidates[start:end], c); err != nil {
			return err
		}
	}
	return nil
}

// issuerFlagsRecheckChunk re-reads one batch of last-known rows and persists
// only those whose account is live again.
func issuerFlagsRecheckChunk(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, chunk []string, c *issuerFlagsCounts,
) error {
	c.recheckSeen += len(chunk)

	live, err := reader.BulkAccountAuthFlags(ctx, chunk)
	if err != nil {
		return err
	}
	rows := make([]timescale.IssuerAuthFlags, 0, len(live))
	for _, g := range chunk {
		f, ok := live[g]
		if !ok {
			continue
		}
		rows = append(rows, issuerAuthFlagsRow(g, f))
	}
	c.revived += len(rows)
	if o.dryRun || len(rows) == 0 {
		return nil
	}
	n, err := store.PersistIssuerAuthFlags(ctx, rows)
	if err != nil {
		return err
	}
	c.written += n
	return nil
}

// issuerAuthFlagsRow maps one lake reading onto its persist row, carrying the
// provenance across the package boundary unchanged.
//
// HomeDomain is copied verbatim, NOT re-derived: the lake reader is what
// guarantees a last-known reading has none (a merged account's self-declared
// domain can no longer be checked against SEP-1's bidirectional
// [[CURRENCIES]] back-reference), and PersistIssuerAuthFlags refuses one that
// slipped through. Blanking it here as well would hide a reader regression
// from both.
func issuerAuthFlagsRow(gStrkey string, f clickhouse.AccountAuthFlags) timescale.IssuerAuthFlags {
	row := timescale.IssuerAuthFlags{
		GStrkey:    gStrkey,
		Required:   f.Required,
		Revocable:  f.Revocable,
		Immutable:  f.Immutable,
		Clawback:   f.Clawback,
		HomeDomain: f.HomeDomain,
		Source:     string(f.Source),
	}
	if f.AsOfLedger > 0 {
		asOf := f.AsOfLedger
		row.AsOfLedger = &asOf
	}
	return row
}
