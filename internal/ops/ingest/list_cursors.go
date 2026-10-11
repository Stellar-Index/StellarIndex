package ingest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listCursors prints every per-source ingestion cursor (source, sub, last ledger, age); empty
// output usually means a fresh deploy.
func listCursors(args []string) error {
	fs := flag.NewFlagSet("list-cursors", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	cursors, err := store.ListCursors(ctx)
	if err != nil {
		return err
	}
	if len(cursors) == 0 {
		fmt.Println("(no cursors stored — fresh deploy or ingestion hasn't written yet)")
		return nil
	}

	now := time.Now().UTC()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SOURCE\tSUB\tLAST LEDGER\tAGE\tUPDATED")
	for _, c := range cursors {
		sub := c.Sub
		if sub == "" {
			sub = "-"
		}
		age := now.Sub(c.UpdatedAt.UTC()).Round(time.Second)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			c.Source, sub, c.LastLedger, age, c.UpdatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}
