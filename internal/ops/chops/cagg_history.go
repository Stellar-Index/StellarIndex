// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/atomicfile"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// caggHistoryVerb measures each continuous aggregate's materialised history against its source; it writes no row.
const caggHistoryVerb = "cagg-history"

// legacyHistoryGauge maps the views data-freshness.sh judged before this command to the 0/1 family it published them
// under; the deploy/monitoring rules read exactly these series.
var legacyHistoryGauge = map[string]string{
	"prices_1m": "stellarindex_cagg_history_missing", "prices_15m": "stellarindex_cagg_history_missing",
	"prices_1h": "stellarindex_cagg_history_missing", "prices_4h": "stellarindex_cagg_history_missing",
	"prices_1d": "stellarindex_cagg_history_missing", "prices_1w": "stellarindex_cagg_history_missing",
	"prices_1mo": "stellarindex_cagg_history_missing",
	"twap_1h":    "stellarindex_twap_history_missing", "twap_1d": "stellarindex_twap_history_missing",
}

// The legacy 0/1 verdict: a day of slack, and no verdict while the source itself is under two days old so a fresh
// deploy's first materialisation stays quiet.
const (
	legacyHistorySlack     = 24 * time.Hour
	legacyHistoryMinSource = 48 * time.Hour
)

func caggHistory(args []string) error {
	fs := flag.NewFlagSet(caggHistoryVerb, flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml (Postgres DSN)")
	textfile := fs.String("textfile", "", "node_exporter textfile path; empty = print the table only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", caggHistoryVerb, fs.Arg(0))
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
	rows, err := store.CAGGHistoryStarts(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("no continuous aggregate measured")
	}
	now := time.Now()
	if err := printCAGGHistory(os.Stdout, rows); err != nil {
		return err
	}
	if *textfile == "" {
		return nil
	}
	var b bytes.Buffer
	renderCAGGHistoryTextfile(&b, rows, now)
	return atomicfile.WriteBytes(*textfile, 0o644, b.Bytes())
}

func printCAGGHistory(w io.Writer, rows []timescale.CAGGHistoryStart) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VIEW\tSOURCE\tMATERIALISED START\tEXPECTED START\tGAP")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.View, r.Source, caggHistoryTime(r.MaterialisedStart),
			caggHistoryTime(r.ExpectedStart), r.Gap.Round(time.Second))
	}
	if len(rows) > 0 && rows[0].TradesRetentionPresent {
		_, _ = fmt.Fprintln(tw, "WARNING: a retention policy exists on trades; trades is kept forever and it is ignored above")
	}
	return tw.Flush()
}

func caggHistoryTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// renderCAGGHistoryTextfile writes the exposition; the start/expected samples are absent when there is no such time.
func renderCAGGHistoryTextfile(w io.Writer, rows []timescale.CAGGHistoryStart, now time.Time) {
	family := func(name, help string, sample func(r timescale.CAGGHistoryStart) (string, bool)) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, r := range rows {
			if v, ok := sample(r); ok {
				_, _ = fmt.Fprintf(w, "%s{view=%q} %s\n", name, r.View, v)
			}
		}
	}
	unix := func(t time.Time) (string, bool) { return fmt.Sprintf("%d", t.Unix()), !t.IsZero() }
	family("stellarindex_cagg_history_start_seconds",
		"Unix time of the earliest bucket in a continuous aggregate's materialisation hypertable (its real-time tail excluded); absent when nothing is materialised.",
		func(r timescale.CAGGHistoryStart) (string, bool) { return unix(r.MaterialisedStart) })
	family("stellarindex_cagg_expected_start_seconds",
		"Unix time a continuous aggregate's history should start: its source's earliest row, raised by a documented floor or an armed retention policy (never one on trades); absent when the source is empty.",
		func(r timescale.CAGGHistoryStart) (string, bool) { return unix(r.ExpectedStart) })
	family("stellarindex_cagg_history_gap_seconds",
		"Seconds the materialised start of a continuous aggregate trails the bucket holding its expected start; a WITH NO DATA rebuild nobody refilled reads as months.",
		func(r timescale.CAGGHistoryStart) (string, bool) {
			return fmt.Sprintf("%d", int64(r.Gap/time.Second)), true
		})
	legacy := func(name, help string) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, r := range rows {
			if legacyHistoryGauge[r.View] == name {
				_, _ = fmt.Fprintf(w, "%s{view=%q} %d\n", name, r.View, legacyHistoryMissing(r, now))
			}
		}
	}
	legacy("stellarindex_cagg_history_missing",
		"1 when a price continuous aggregate (prices_1m..prices_1mo) holds less history than the trades hypertable it aggregates — a migration recreated it WITH NO DATA, or a replay rewrote its base rows, and the manual refresh_continuous_aggregate follow-up never ran. Its refresh policy re-fills only a trailing sliver, so every newest-bar and last-refresh signal reads green while the back-history serves empty.")
	legacy("stellarindex_twap_history_missing",
		"1 when a TWAP continuous aggregate is missing the history prices_1m holds — a migration recreated/emptied it (WITH NO DATA) and the manual refresh_continuous_aggregate follow-up never ran.")
	present := 0
	if len(rows) > 0 && rows[0].TradesRetentionPresent {
		present = 1
	}
	_, _ = fmt.Fprintf(w, "# HELP stellarindex_trades_retention_present 1 when a retention policy exists on trades, which is kept forever; the history gauges ignore it.\n# TYPE stellarindex_trades_retention_present gauge\nstellarindex_trades_retention_present %d\n", present)
}

func legacyHistoryMissing(r timescale.CAGGHistoryStart, now time.Time) int {
	if r.SourceStart.IsZero() || !r.SourceStart.Before(now.Add(-legacyHistoryMinSource)) || r.Gap <= legacyHistorySlack {
		return 0
	}
	return 1
}
