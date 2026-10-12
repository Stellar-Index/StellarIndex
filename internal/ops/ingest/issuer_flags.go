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
	IssuersNeedingChainRecheck(ctx context.Context, limit int) ([]timescale.IssuerAuthFlagsOnRecord, error)
	PersistIssuerAuthFlags(ctx context.Context, flags []timescale.IssuerAuthFlags) (int, error)
}

// issuerFlagsReader is the lake seam the drain reads through —
// clickhouse.ExplorerReader satisfies it.
type issuerFlagsReader interface {
	BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
	RemovedAccountsLastKnownAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
}

// issuerFlagsCounts is one run's accounting: every candidate lands in exactly one of resolvedLive / resolvedLastKnown / absent, so
// `resolved* + absent == candidates` is checkable.
type issuerFlagsCounts struct {
	candidates        int // rows the primary queue offered
	resolvedLive      int // a live AccountEntry in the lake
	resolvedLastKnown int // merged away; recovered from the pre-image
	absent            int // neither — outside the lake's captured window
	written           int // rows actually changed in Postgres

	recheckCandidates int // rows the re-check queue offered
	recheckSeen       int // rows the re-check pass actually examined
	revived           int // last-known rows whose account is live again

	chainCandidates int // filled live-sourced rows the chain re-check offered
	chainSeen       int // of those, rows the pass actually examined
	chainAgreed     int // examined rows the chain still agrees with
	chainCorrected  int // examined rows the chain has moved past
	chainUnread     int // examined rows neither reader answered for
	chainMerged     int // of the agreed+corrected, rows answered by the last-known reader
}

// issuerFlagsCmd persists issuer AccountEntry auth flags into `issuers` as a durable fallback for read-time enrichment. Merged issuers
// store `last_known_before_removal` with no home_domain and are re-checked; a third pass writes only differing live rows.
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
		day:    int(time.Now().Unix() / 86400),
		dryRun: dryRun,
		out:    os.Stderr,
	})
}

type issuerFlagsOpts struct {
	limit             int
	chainRecheckLimit int
	batch             int
	// day is the run's day number; it picks the batch the chain re-check
	// starts from (see rotateChainRecheck). 0 starts at the head.
	day    int
	dryRun bool
	out    io.Writer
}

// runIssuerFlags is the resolve -> fall back -> persist -> re-check drain, split from flag wiring so it is testable with stubs.
func runIssuerFlags(ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader, o issuerFlagsOpts) error {
	var c issuerFlagsCounts

	candidates, err := store.IssuerGStrkeysNeedingFlags(ctx, o.limit)
	if err != nil {
		return err
	}
	c.candidates = len(candidates)
	_, _ = fmt.Fprintf(o.out, "issuer-flags: %d issuer(s) with unresolved flags\n", c.candidates)

	passes := []func() error{
		func() error { return issuerFlagsResolvePass(ctx, store, reader, o, candidates, &c) },
		func() error { return issuerFlagsRecheckPass(ctx, store, reader, o, &c) },
		func() error { return issuerFlagsChainRecheckPass(ctx, store, reader, o, &c) },
	}
	for _, pass := range passes {
		err := pass()
		if err == nil {
			continue
		}
		// The deadline usually expires inside a lake read or the next pass's
		// queue query, not between batches; that is the same resumable stop.
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return err
		}
		_, _ = fmt.Fprintf(o.out, "issuer-flags: timeout reached mid-read — stopping early (queue is resumable): %v\n", err)
		break
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
	// The chain re-check accounts the same way: every examined row is
	// corrected, agreed-with, or unread. A short fall against `candidates`
	// means the run stopped on its timeout; the next day's run starts one
	// batch further on, so the rows it missed are not missed every night.
	_, _ = fmt.Fprintf(o.out,
		"issuer-flags: chain re-check processed %d of %d filled row(s) — corrected=%d agreed=%d unread=%d (merged=%d)\n",
		c.chainSeen, c.chainCandidates, c.chainCorrected, c.chainAgreed, c.chainUnread, c.chainMerged)
	// `absent` is expected, not a failure: a merge before the projection floor, or an entry outside the captured window, has no pre-image to recover. Reported so coverage is visible.
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

// issuerFlagsRecheckPass re-offers `last_known_before_removal` rows to the live reader so a re-created account flips back to `live`.
// Only live hits are re-persisted; a still-merged row cannot have changed.
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
	live, err := reader.BulkAccountAuthFlags(ctx, chunk)
	if err != nil {
		return err
	}
	// Counted only once read, so a run cut off mid-read reports what it examined.
	c.recheckSeen += len(chunk)
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

// issuerFlagsChainRecheckPass re-offers every filled, live-sourced row to the live reader and writes back only rows the chain has moved past.
// The other queues are one-shot, so without it a lapsed home_domain would never be re-read. It runs last under its own limit so it cannot take budget from the primary drain.
func issuerFlagsChainRecheckPass(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, c *issuerFlagsCounts,
) error {
	onRecord, err := store.IssuersNeedingChainRecheck(ctx, o.chainRecheckLimit)
	if err != nil {
		return err
	}
	c.chainCandidates = len(onRecord)
	if len(onRecord) == 0 {
		return nil
	}
	onRecord = rotateChainRecheck(onRecord, o.batch, o.day)
	_, _ = fmt.Fprintf(o.out, "issuer-flags: re-reading %d filled row(s) against the chain\n", len(onRecord))

	for start := 0; start < len(onRecord); start += o.batch {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(o.out, "issuer-flags: timeout reached during the chain re-check — stopping early (queue is resumable)\n")
			break
		}
		end := min(start+o.batch, len(onRecord))
		if err := issuerFlagsChainRecheckChunk(ctx, store, reader, o, onRecord[start:end], c); err != nil {
			return err
		}
	}
	return nil
}

// rotateChainRecheck returns recs starting at batch (day mod batches), wrapping to the head, so a timeout-truncated walk does not leave the same tail unexamined, with no persisted cursor.
func rotateChainRecheck(recs []timescale.IssuerAuthFlagsOnRecord, batch, day int) []timescale.IssuerAuthFlagsOnRecord {
	batches := (len(recs) + batch - 1) / batch
	if batches <= 1 || day <= 0 {
		return recs
	}
	start := (day % batches) * batch
	out := make([]timescale.IssuerAuthFlagsOnRecord, 0, len(recs))
	out = append(out, recs[start:]...)
	return append(out, recs[:start]...)
}

// issuerFlagsChainRecheckChunk re-reads one batch and persists only readings that differ, so `written` means rows the chain corrected.
// A key the live reader misses goes to the last-known reader, because only its actual `removed` row may conclude "merged" (a lake gap looks the same); a key neither answers changes nothing.
func issuerFlagsChainRecheckChunk(
	ctx context.Context, store issuerFlagsStore, reader issuerFlagsReader,
	o issuerFlagsOpts, chunk []timescale.IssuerAuthFlagsOnRecord, c *issuerFlagsCounts,
) error {
	keys := make([]string, 0, len(chunk))
	for _, rec := range chunk {
		keys = append(keys, rec.GStrkey)
	}
	live, err := reader.BulkAccountAuthFlags(ctx, keys)
	if err != nil {
		return err
	}
	var lastKnown map[string]clickhouse.AccountAuthFlags
	if misses := chainRecheckMisses(keys, live); len(misses) > 0 {
		if lastKnown, err = reader.RemovedAccountsLastKnownAuthFlags(ctx, misses); err != nil {
			return err
		}
	}
	// Counted only once both reads answered, keeping seen = corrected+agreed+unread.
	c.chainSeen += len(chunk)

	rows := make([]timescale.IssuerAuthFlags, 0, len(live)+len(lastKnown))
	for _, rec := range chunk {
		f, ok := live[rec.GStrkey]
		if !ok {
			if f, ok = lastKnown[rec.GStrkey]; !ok {
				c.chainUnread++
				continue
			}
			c.chainMerged++
		}
		fresh := issuerAuthFlagsRow(rec.GStrkey, f)
		if issuerAuthFlagsOnRecordAgrees(rec, fresh) {
			c.chainAgreed++
			continue
		}
		rows = append(rows, fresh)
	}
	c.chainCorrected += len(rows)
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

// chainRecheckMisses returns the keys the live reader did not answer for, in
// input order.
func chainRecheckMisses(keys []string, live map[string]clickhouse.AccountAuthFlags) []string {
	misses := make([]string, 0, len(keys)-len(live))
	for _, g := range keys {
		if _, ok := live[g]; !ok {
			misses = append(misses, g)
		}
	}
	return misses
}

// issuerAuthFlagsOnRecordAgrees reports whether persisting fresh would leave the row unchanged, mirroring what PersistIssuerAuthFlags writes.
// A labelled reading overwrites home_domain even with empty; only an unlabelled empty is COALESCEd away.
func issuerAuthFlagsOnRecordAgrees(rec timescale.IssuerAuthFlagsOnRecord, fresh timescale.IssuerAuthFlags) bool {
	if !boolPtrIs(rec.Required, fresh.Required) ||
		!boolPtrIs(rec.Revocable, fresh.Revocable) ||
		!boolPtrIs(rec.Immutable, fresh.Immutable) ||
		!boolPtrIs(rec.Clawback, fresh.Clawback) {
		return false
	}
	if (fresh.Source != "" || fresh.HomeDomain != "") && rec.HomeDomain != fresh.HomeDomain {
		return false
	}
	if rec.Source != fresh.Source {
		return false
	}
	return u32PtrEq(rec.AsOfLedger, fresh.AsOfLedger)
}

// boolPtrIs reports whether a persisted nullable flag already holds want. A
// NULL column is "no observation yet", which never agrees with a reading.
func boolPtrIs(got *bool, want bool) bool { return got != nil && *got == want }

// u32PtrEq compares two optional ledger sequences, nil-safe.
func u32PtrEq(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// issuerAuthFlagsRow maps one lake reading onto its persist row, carrying provenance unchanged.
// HomeDomain is copied verbatim, not re-derived: the reader guarantees last-known readings have none and PersistIssuerAuthFlags refuses one that slipped through; blanking here would hide a reader regression.
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
