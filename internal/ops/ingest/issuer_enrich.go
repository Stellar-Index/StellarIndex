package ingest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// issuerEnrich populates issuers.home_domain from the on-chain account state in
// the ClickHouse lake (DATA-TRUTH-PLAN G5 prerequisite). The issuers table only
// ever gets g_strkey written by the indexer; home_domain was never synced, so
// sep1-refresh (which selects issuers WITH a home_domain) had zero candidates
// and org_name stayed null. This reads each issuer's account home_domain from
// ledger_entries_current (complete after the G2 account backfill) in batches
// and writes it back, unblocking sep1-refresh → org_name.
//
// creation_ledger is NOT set here: it needs the create_account op from full
// history (operation_participants only covers ~1 day), which is a separate
// genesis op-scan — tracked, not done here.
func issuerEnrich(args []string) error {
	fs := flag.NewFlagSet("issuer-enrich", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "config path")
	chAddr := fs.String("ch", "127.0.0.1:9300", "ClickHouse native address")
	batch := fs.Int("batch", 1000, "issuers per ClickHouse lookup batch")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer func() { _ = store.Close() }()
	er, err := clickhouse.NewExplorerReader(ctx, *chAddr)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer func() { _ = er.Close() }()

	ids, err := loadIssuerGStrkeys(ctx, store)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "issuer-enrich: %d issuers; resolving home_domain from the lake (batch=%d, dry-run=%v)\n",
		len(ids), *batch, dryRun)

	found, updated, failedBatches := issuerEnrichLoop(ctx, er, store, ids, *batch, dryRun)
	if failedBatches > 0 {
		fmt.Printf("\n⚠️  issuer-enrich: %d issuers, %d have a home_domain, %d rows updated, %d batch(es) failed — see stderr above; safe to re-run.\n",
			len(ids), found, updated, failedBatches)
		fmt.Printf("   Next: run `stellarindex-ops sep1-refresh` to fetch their tomls → org_name.\n")
		return fmt.Errorf("issuer-enrich: %d batch(es) failed", failedBatches)
	}
	fmt.Printf("\n✅ issuer-enrich: %d issuers, %d have a home_domain, %d rows updated.\n", len(ids), found, updated)
	fmt.Printf("   Next: run `stellarindex-ops sep1-refresh` to fetch their tomls → org_name.\n")
	return nil
}

// homeDomainLookup narrows *clickhouse.ExplorerReader to what
// issuerEnrichLoop needs, so the loop can be exercised without a lake.
type homeDomainLookup interface {
	AccountHomeDomains(ctx context.Context, accounts []string) (map[string]string, error)
}

// homeDomainWriter narrows *timescale.Store to what issuerEnrichLoop
// needs, so the loop can be exercised without Postgres.
type homeDomainWriter interface {
	SyncIssuerHomeDomain(ctx context.Context, gStrkey, homeDomain string) (bool, error)
}

// issuerEnrichLoop resolves and writes home_domain in fixed-size batches.
//
// It used to abort the entire run — leaving every remaining batch
// unenriched — the moment a single batch's lookup or write failed. For a
// table of thousands of issuers split into hundreds of batches, one
// transient ClickHouse or Postgres hiccup partway through meant the rest
// of the issuers silently never got a chance. It now logs the failing
// batch, counts it, and keeps going; the caller decides whether any
// failedBatches should fail the run.
func issuerEnrichLoop(ctx context.Context, er homeDomainLookup, store homeDomainWriter, ids []string, batchSize int, dryRun bool) (found, updated, failedBatches int) {
	start := time.Now()
	for lo := 0; lo < len(ids); lo += batchSize {
		hi := lo + batchSize
		if hi > len(ids) {
			hi = len(ids)
		}
		domains, derr := er.AccountHomeDomains(ctx, ids[lo:hi])
		if derr != nil {
			fmt.Fprintf(os.Stderr, "  ... home_domain batch [%d,%d) failed, skipping: %v\n", lo, hi, derr)
			failedBatches++
			continue
		}
		found += len(domains)
		if !dryRun {
			n, uerr := updateIssuerHomeDomains(ctx, store, domains)
			updated += n
			if uerr != nil {
				fmt.Fprintf(os.Stderr, "  ... update batch [%d,%d) had failures: %v\n", lo, hi, uerr)
				failedBatches++
			}
		}
		if (lo/batchSize)%20 == 0 {
			fmt.Fprintf(os.Stderr, "  ... %d/%d issuers scanned, %d with home_domain (%s)\n",
				hi, len(ids), found, time.Since(start).Round(time.Second))
		}
	}
	return found, updated, failedBatches
}

func loadIssuerGStrkeys(ctx context.Context, store *timescale.Store) ([]string, error) {
	rows, err := store.DB().QueryContext(ctx, `SELECT g_strkey FROM issuers`)
	if err != nil {
		return nil, fmt.Errorf("select issuers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan g_strkey: %w", err)
		}
		ids = append(ids, g)
	}
	return ids, rows.Err()
}

// updateIssuerHomeDomains writes each issuer's on-chain home_domain onto
// its row and returns how many rows actually changed.
//
// It used to write only into an EMPTY column — "never clobbers a
// resolver-set value" — which made this job unable to CORRECT anything and
// the column write-once. See [timescale.Store.SyncIssuerHomeDomain] for why
// that was an identity defect rather than a conservatism, and why the only
// other writer of the column had the same clause for the same absent reason.
//
// A single row's write failure no longer aborts the rest of the batch —
// every domain in the batch still gets its write attempt, and the caller
// learns via the returned error how many failed.
func updateIssuerHomeDomains(ctx context.Context, store homeDomainWriter, domains map[string]string) (int, error) {
	n, failed := 0, 0
	var lastErr error
	for g, domain := range domains {
		changed, err := store.SyncIssuerHomeDomain(ctx, g, domain)
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		if changed {
			n++
		}
	}
	if failed > 0 {
		return n, fmt.Errorf("%d of %d home_domain write(s) failed, e.g. %w", failed, len(domains), lastErr)
	}
	return n, nil
}
