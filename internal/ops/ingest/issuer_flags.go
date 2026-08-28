package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
func issuerFlagsCmd(args []string) error {
	fs := flag.NewFlagSet("issuer-flags", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	limit := fs.Int("limit", 5000, "Max issuers to resolve this run (<=0 = every candidate)")
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

	candidates, err := store.IssuerGStrkeysNeedingFlags(ctx, *limit)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "issuer-flags: %d issuer(s) with unresolved flags\n", len(candidates))
	if len(candidates) == 0 {
		return nil
	}

	var (
		resolved int // found a live AccountEntry in the lake
		written  int // rows actually changed in Postgres
		absent   int // no entry in the captured window
	)
	for start := 0; start < len(candidates); start += *batch {
		if ctx.Err() != nil {
			// Timed out mid-walk. NOT an error: the queue is
			// resumable by construction, so a bounded run that stops
			// early has still made progress.
			fmt.Fprintf(os.Stderr, "issuer-flags: timeout reached — stopping early (queue is resumable)\n")
			break
		}
		end := min(start+*batch, len(candidates))
		chunk := candidates[start:end]

		flagsByG, err := reader.BulkAccountAuthFlags(ctx, chunk)
		if err != nil {
			return err
		}
		absent += len(chunk) - len(flagsByG)
		resolved += len(flagsByG)

		rows := make([]timescale.IssuerAuthFlags, 0, len(flagsByG))
		for g, f := range flagsByG {
			rows = append(rows, timescale.IssuerAuthFlags{
				GStrkey:    g,
				Required:   f.Required,
				Revocable:  f.Revocable,
				Immutable:  f.Immutable,
				Clawback:   f.Clawback,
				HomeDomain: f.HomeDomain,
			})
		}
		if dryRun {
			continue
		}
		n, err := store.PersistIssuerAuthFlags(ctx, rows)
		if err != nil {
			return err
		}
		written += n
	}

	fmt.Fprintf(os.Stderr,
		"issuer-flags: resolved=%d absent=%d written=%d (dry-run=%v)\n",
		resolved, absent, written, dryRun)
	// `absent` is expected and not a failure: an issuer whose account
	// entry is outside the lake's captured window simply keeps rendering
	// "not yet resolved" until it is captured. Reported so the operator
	// can see coverage rather than infer it from silence.
	return nil
}
