// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// curated-rwa-sync — cache the totals a third party PUBLISHES about
// tokenized real-world assets on Stellar into curated_rwa_published_series
// (migration 0162) for the RWA surface's CURATED arm.
//
// # What it reads, and why only that
//
// The first curator is the Stellar team's "RWAs on Stellar" dashboard
// (dune.com/stellar/rwas). That dashboard values `stellar.token_balances`
// against two CSV uploads, `dune.stellar.dataset_recognized_assets`
// (membership, company, subclass) and `dune.stellar.dataset_asset_prices`
// (a close_usd per contract). Both are PRIVATE to the uploading team: a
// SQL execution over either from any outside account is refused with
// "Uploaded table (...) does not exist or it is private". So the
// per-asset list this arm was first built to read cannot be read, by
// anyone, with any key.
//
// What the curator does let anyone read is the latest RESULT of the
// dashboard's public queries, via GET /api/v1/query/{id}/results: the
// monthly RWA market-cap total and that total split by the curator's own
// subclass labels. This run reads exactly those two results, as
// published, and stores them under the curator's name. It executes
// nothing: a read of a public query's latest result bills by datapoints
// (fractions of a credit) and never by execution.
//
// Nothing read here attests to anything. A row is the curator's
// arithmetic over the curator's private inputs: no signature, no
// per-asset breakdown, no market. The surface serves it as
// `curated.published` beside the verified set and never inside it, with
// the signed gap between the two.

const (
	curatedRWACuratorDune  = "dune:stellar"
	curatedRWADuneBaseURL  = "https://api.dune.com"
	curatedRWAFetchTimeout = 90 * time.Second
	curatedRWAPageSize     = 1000
	// curatedRWAMaxRows bounds one query's result. The monthly total is
	// under twenty rows and the split a few hundred; a result past this
	// is not one of those queries.
	curatedRWAMaxRows = 5000
)

// The dashboard's PUBLIC queries, by id and title as the curator names
// them. A read of either needs any Dune API key and no execution.
const (
	// curatedRWAQueryMonthlyTotal — "RWAs on Stellar: RWA Mcap by Month":
	// rows {month_end, total_rwa_market_cap_usd}, one per month. Its
	// latest row is the dashboard's headline figure.
	curatedRWAQueryMonthlyTotal int64 = 6961845
	// curatedRWAQueryMonthlyBySubclass — "RWAs on Stellar: Mcap by Month
	// by Asset Subclass": rows {month_end, asset_subclass,
	// market_cap_usd}, one per month and subclass.
	curatedRWAQueryMonthlyBySubclass int64 = 6961847
)

// curatedRWACounts is one run's accounting, printed and emitted.
type curatedRWACounts struct {
	// Rows is every row the two results carried; Kept is what parsed;
	// Malformed is the difference.
	Rows      int
	Kept      int
	Malformed int
	// Datapoints is what the curator's platform reported metering for
	// the results read — the run's whole cost, in the platform's unit.
	Datapoints int
	// Months counts points on the total series; LatestMonthEnd and
	// LatestTotalUSD are its last point, the curator's headline.
	Months         int
	LatestMonthEnd string
	LatestTotalUSD string
	// ExecutedAt is when the curator's total query last ran.
	ExecutedAt time.Time
}

func curatedRWASync(args []string) error {
	fs := flag.NewFlagSet("curated-rwa-sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	baseURL := fs.String("base-url", curatedRWADuneBaseURL, "Dune API base URL (https only)")
	timeout := fs.Duration("timeout", 5*time.Minute, "Wall-clock timeout for the whole run")
	textfile := fs.String("textfile", "", "node_exporter textfile to write run metrics to (optional)")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if !strings.HasPrefix(*baseURL, "https://") {
		return fmt.Errorf("-base-url must be https:// (got %q)", *baseURL)
	}
	key := strings.TrimSpace(os.Getenv("DUNE_API_KEY"))
	if key == "" {
		// Refused, not failed: the run still stamps its textfile so the
		// staleness alert measures the TIMER's cadence, and a separate
		// `refused` gauge says why nothing was read. A fresh install's
		// empty placeholder would otherwise read as a sync that never
		// ran, which is a different problem with a different fix.
		fmt.Fprintln(os.Stderr, "curated-rwa-sync: REFUSED — DUNE_API_KEY is not set; this run cannot read the curator and will not pretend it did")
		if *textfile != "" {
			if err := writeCuratedRWATextfile(*textfile, curatedRWACounts{}, true, true); err != nil {
				fmt.Fprintf(os.Stderr, "curated-rwa-sync: WARN textfile: %v\n", err)
			}
		}
		return nil
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := newCuratedRWAClient(*baseURL, key)
	fmt.Printf("Curator %s via %s.\n", curatedRWACuratorDune, client.baseURL)

	rows, counts, err := client.fetch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Read %d rows; kept %d (%d months on the total series, latest %s = %s USD, executed %s); %d skipped-malformed; %d datapoints metered.\n",
		counts.Rows, counts.Kept, counts.Months, counts.LatestMonthEnd, counts.LatestTotalUSD,
		counts.ExecutedAt.UTC().Format(time.RFC3339), counts.Malformed, counts.Datapoints)

	if *textfile != "" {
		if err := writeCuratedRWATextfile(*textfile, counts, dryRun, false); err != nil {
			fmt.Fprintf(os.Stderr, "curated-rwa-sync: WARN textfile: %v\n", err)
		}
	}
	if dryRun {
		fmt.Println("Dry run — nothing written.")
		return nil
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	inserted, err := store.ReplaceCuratedRWAPublished(ctx, curatedRWACuratorDune, rows)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d rows replaced across %d series (curator=%s).\n", inserted, 2, curatedRWACuratorDune)
	return nil
}

// ─── client ─────────────────────────────────────────────────────────

type curatedRWAClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func newCuratedRWAClient(baseURL, key string) *curatedRWAClient {
	return &curatedRWAClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http:    &http.Client{Timeout: curatedRWAFetchTimeout},
	}
}

// get performs one read. Every route this file touches is a GET of a
// public query's latest result; nothing here can execute a query.
func (c *curatedRWAClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil) //nolint:gosec // G704: base URL defaults to a constant and an operator override is validated https-only; path is one of this file's fixed API routes
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Dune-API-Key", c.key)
	req.Header.Set("User-Agent", "stellarindex-curated-rwa-sync/2")
	resp, err := c.http.Do(req) //nolint:gosec // G704: see the request construction above
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("curated-rwa-sync: GET %s: HTTP %d: %s",
			path, resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 512)])))
	}
	return b, nil
}

// duneQueryResultsPage is one page of GET /api/v1/query/{id}/results.
// The envelope carries more members than these (submission times,
// execution ids); they are ignored. Rows are kept raw here and decoded
// STRICTLY per query below.
type duneQueryResultsPage struct {
	State            string    `json:"state"`
	ExecutionEndedAt time.Time `json:"execution_ended_at"`
	NextOffset       *int      `json:"next_offset"`
	Result           struct {
		Rows     []json.RawMessage `json:"rows"`
		Metadata struct {
			TotalRowCount  int `json:"total_row_count"`
			DatapointCount int `json:"datapoint_count"`
		} `json:"metadata"`
	} `json:"result"`
}

// duneQueryResult is one query's whole latest result, paged to the row
// count the curator declared.
type duneQueryResult struct {
	Rows       []json.RawMessage
	ExecutedAt time.Time
	Datapoints int
}

// fetch reads both public results and turns them into the rows one
// sync writes. Either result failing to read, or parsing to nothing,
// fails the run: a half-read pair would be written as a whole.
func (c *curatedRWAClient) fetch(ctx context.Context) ([]timescale.CuratedRWAPublishedRow, curatedRWACounts, error) {
	var counts curatedRWACounts

	total, err := c.readQueryResult(ctx, curatedRWAQueryMonthlyTotal)
	if err != nil {
		return nil, counts, err
	}
	counts.Datapoints += total.Datapoints
	counts.ExecutedAt = total.ExecutedAt
	totalRows := parseCuratedRWAMonthlyTotal(total, &counts)
	if len(totalRows) == 0 {
		return nil, counts, fmt.Errorf("curated-rwa-sync: query %d printed no usable row (%d read, %d malformed)",
			curatedRWAQueryMonthlyTotal, len(total.Rows), counts.Malformed)
	}

	split, err := c.readQueryResult(ctx, curatedRWAQueryMonthlyBySubclass)
	if err != nil {
		return nil, counts, err
	}
	counts.Datapoints += split.Datapoints
	splitRows := parseCuratedRWAMonthlyBySubclass(split, &counts)
	if len(splitRows) == 0 {
		return nil, counts, fmt.Errorf("curated-rwa-sync: query %d printed no usable row (%d read, %d malformed)",
			curatedRWAQueryMonthlyBySubclass, len(split.Rows), counts.Malformed)
	}

	counts.Months = len(totalRows)
	last := totalRows[len(totalRows)-1]
	counts.LatestMonthEnd = last.MonthEnd.Format("2006-01-02")
	counts.LatestTotalUSD = last.ValueUSD
	return append(totalRows, splitRows...), counts, nil
}

// readQueryResult pages one query's latest result until the row count
// the curator declared is in hand. A result that ends early, exceeds the
// bound, or is not a completed execution is an error, never a partial
// series.
func (c *curatedRWAClient) readQueryResult(ctx context.Context, queryID int64) (duneQueryResult, error) {
	var out duneQueryResult
	declared := -1
	offset := 0
	for {
		page, err := c.readQueryPage(ctx, queryID, offset)
		if err != nil {
			return out, err
		}
		if declared < 0 {
			declared = page.Result.Metadata.TotalRowCount
			if declared > curatedRWAMaxRows {
				return out, fmt.Errorf("curated-rwa-sync: query %d declares %d rows, over the %d bound — this is not the expected result",
					queryID, declared, curatedRWAMaxRows)
			}
			out.ExecutedAt = page.ExecutionEndedAt.UTC()
			out.Datapoints = page.Result.Metadata.DatapointCount
		}
		out.Rows = append(out.Rows, page.Result.Rows...)
		if len(out.Rows) > curatedRWAMaxRows {
			return out, fmt.Errorf("curated-rwa-sync: query %d result exceeds %d rows", queryID, curatedRWAMaxRows)
		}
		if len(out.Rows) >= declared {
			if out.ExecutedAt.IsZero() {
				return out, fmt.Errorf("curated-rwa-sync: query %d result carries no execution_ended_at", queryID)
			}
			return out, nil
		}
		if page.NextOffset == nil || *page.NextOffset <= offset || len(page.Result.Rows) == 0 {
			return out, fmt.Errorf("curated-rwa-sync: query %d printed %d of the %d rows it declared and offered no next page",
				queryID, len(out.Rows), declared)
		}
		offset = *page.NextOffset
	}
}

// readQueryPage fetches and decodes one page of a query's latest result,
// refusing a page whose execution is anything but completed: the curator's
// own failed run is not a result this index will read.
func (c *curatedRWAClient) readQueryPage(ctx context.Context, queryID int64, offset int) (duneQueryResultsPage, error) {
	var page duneQueryResultsPage
	b, err := c.get(ctx, fmt.Sprintf("/api/v1/query/%d/results?limit=%d&offset=%d", queryID, curatedRWAPageSize, offset))
	if err != nil {
		return page, err
	}
	if err := json.Unmarshal(b, &page); err != nil {
		return page, fmt.Errorf("curated-rwa-sync: query %d results: %w", queryID, err)
	}
	if page.State != "" && page.State != "QUERY_STATE_COMPLETED" {
		return page, fmt.Errorf("curated-rwa-sync: query %d latest execution is %s, not completed", queryID, page.State)
	}
	return page, nil
}

// ─── parse ──────────────────────────────────────────────────────────

// The row shapes, decoded STRICTLY: an unknown or renamed column is a
// malformed row, counted and skipped, never guessed at. Values arrive as
// json.Number so the literal the curator printed is what is stored
// (ADR-0003) and never a float re-rendering.

type duneMonthlyTotalRow struct {
	MonthEnd             string      `json:"month_end"`
	TotalRWAMarketCapUSD json.Number `json:"total_rwa_market_cap_usd"`
}

type duneMonthlyBySubclassRow struct {
	MonthEnd      string      `json:"month_end"`
	AssetSubclass string      `json:"asset_subclass"`
	MarketCapUSD  json.Number `json:"market_cap_usd"`
}

func decodeStrict(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	return dec.Decode(into)
}

func parseCuratedRWAMonthlyTotal(res duneQueryResult, counts *curatedRWACounts) []timescale.CuratedRWAPublishedRow {
	out := make([]timescale.CuratedRWAPublishedRow, 0, len(res.Rows))
	for _, raw := range res.Rows {
		counts.Rows++
		var r duneMonthlyTotalRow
		if err := decodeStrict(raw, &r); err != nil {
			counts.Malformed++
			continue
		}
		month, ok := curatedRWAMonthEnd(r.MonthEnd)
		value, okV := curatedRWADecimal(r.TotalRWAMarketCapUSD)
		if !ok || !okV {
			counts.Malformed++
			continue
		}
		out = append(out, timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyTotal, MonthEnd: month, ValueUSD: value,
			SourceQuery: curatedRWAQueryMonthlyTotal, ExecutedAt: res.ExecutedAt,
		})
		counts.Kept++
	}
	sortPublishedRowsByMonth(out)
	return out
}

func parseCuratedRWAMonthlyBySubclass(res duneQueryResult, counts *curatedRWACounts) []timescale.CuratedRWAPublishedRow {
	out := make([]timescale.CuratedRWAPublishedRow, 0, len(res.Rows))
	for _, raw := range res.Rows {
		counts.Rows++
		var r duneMonthlyBySubclassRow
		if err := decodeStrict(raw, &r); err != nil {
			counts.Malformed++
			continue
		}
		month, ok := curatedRWAMonthEnd(r.MonthEnd)
		value, okV := curatedRWADecimal(r.MarketCapUSD)
		subclass := strings.TrimSpace(r.AssetSubclass)
		if !ok || !okV || subclass == "" {
			counts.Malformed++
			continue
		}
		out = append(out, timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyBySubclass, MonthEnd: month, Subclass: subclass, ValueUSD: value,
			SourceQuery: curatedRWAQueryMonthlyBySubclass, ExecutedAt: res.ExecutedAt,
		})
		counts.Kept++
	}
	sortPublishedRowsByMonth(out)
	return out
}

// curatedRWAMonthEnd accepts the month bucket as the curator prints it —
// a date, or a timestamp at midnight — and pins it to that day at
// midnight UTC.
func curatedRWAMonthEnd(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04:05.000 UTC", "2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

// curatedRWADecimal accepts a finite decimal literal and returns it as
// printed. Exponent forms are refused: the store carries the literal,
// and "4.0e9" is not a figure a reader can compare by eye.
func curatedRWADecimal(n json.Number) (string, bool) {
	s := strings.TrimSpace(n.String())
	if s == "" || strings.ContainsAny(s, "eE") {
		return "", false
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return "", false
	}
	return s, true
}

func sortPublishedRowsByMonth(rows []timescale.CuratedRWAPublishedRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].MonthEnd.Before(rows[j-1].MonthEnd); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// ─── textfile ───────────────────────────────────────────────────────

// writeCuratedRWATextfile records the run for node_exporter. Written
// whole to a sibling temp file and renamed, so the collector never reads
// a half-written exposition; every family shares the file's fate.
func writeCuratedRWATextfile(path string, c curatedRWACounts, dryRun, refused bool) error {
	var b strings.Builder
	lbl := fmt.Sprintf(`{curator=%q}`, curatedRWACuratorDune)
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_last_run_unix Unix time the most recent curated-RWA sync finished, pass or fail.\n# TYPE stellarindex_curated_rwa_sync_last_run_unix gauge\nstellarindex_curated_rwa_sync_last_run_unix%s %d\n", lbl, time.Now().Unix())
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_rows Rows of the curator's published series kept by the most recent sync (monthly totals plus the per-subclass split).\n# TYPE stellarindex_curated_rwa_sync_rows gauge\nstellarindex_curated_rwa_sync_rows%s %d\n", lbl, c.Kept)
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_datapoints_read Datapoints the curator's platform reported metering for the results the most recent sync read. Reads of a public query's latest result bill by datapoint (a fraction of a credit) and never execute the query; there is no execution cost to report.\n# TYPE stellarindex_curated_rwa_sync_datapoints_read gauge\nstellarindex_curated_rwa_sync_datapoints_read%s %d\n", lbl, c.Datapoints)
	var executed int64
	if !c.ExecutedAt.IsZero() {
		executed = c.ExecutedAt.Unix()
	}
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_executed_at_unix Unix time the curator's public total query last ran, as read by the most recent sync (0 when nothing was read). The published figure is as fresh as this, not as fresh as the sync.\n# TYPE stellarindex_curated_rwa_sync_executed_at_unix gauge\nstellarindex_curated_rwa_sync_executed_at_unix%s %d\n", lbl, executed)
	written := 1
	if dryRun {
		written = 0
	}
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_written Whether the most recent sync wrote the cache (0 on a dry run).\n# TYPE stellarindex_curated_rwa_sync_written gauge\nstellarindex_curated_rwa_sync_written%s %d\n", lbl, written)
	refusedV := 0
	if refused {
		refusedV = 1
	}
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_refused Whether the most recent run refused to read the curator (1: no API key configured).\n# TYPE stellarindex_curated_rwa_sync_refused gauge\nstellarindex_curated_rwa_sync_refused%s %d\n", lbl, refusedV)

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // world-readable metrics file by design: the collector reads it
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
