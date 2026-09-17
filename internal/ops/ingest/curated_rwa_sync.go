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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// curated-rwa-sync — cache a third party's list of tokenized real-world
// assets on Stellar, and that party's own price per token, into
// rwa_curated_directory (migration 0161) for the RWA surface's CURATED
// arm.
//
// # What it reads, and why that is the whole point
//
// The first curator is the Stellar team's public Dune uploads. The
// "RWAs on Stellar" dashboard (dune.com/stellar/rwas) values
// `stellar.token_balances` against `dune.stellar.dataset_asset_prices`
// and takes membership from `dune.stellar.dataset_recognized_assets`;
// `dune.<team>.dataset_<name>` is Dune's CSV-upload namespace by its own
// documentation. So the dashboard's figure IS those two tables, and the
// only way to show a reader line by line why this index and that
// dashboard differ is to read the same two tables and publish what they
// admit under their own name.
//
// Nothing read here attests to anything. A row is the curator's word:
// no signature, no proof of control, no market. The surface serves it
// under `basis: third_party_curated` with its own total, beside the
// verified set and never inside it.
//
// # Cost
//
// Every run is one Dune SQL execution on the "small" tier. Credits are
// consumed per execution; the run records `execution_cost_credits` from
// the status endpoint in the textfile it emits, so the cost of this
// arm is a metric and not a surprise on an invoice.

const (
	curatedRWACuratorDune  = "dune:stellar"
	curatedRWADuneBaseURL  = "https://api.dune.com"
	curatedRWAFetchTimeout = 90 * time.Second
	curatedRWAPollEvery    = 2 * time.Second
	curatedRWAPageSize     = 1000
	// curatedRWAMaxRows bounds one sync. The dashboard's asset list is
	// under a hundred rows; a result set past this is not that list.
	curatedRWAMaxRows = 5000
)

// curatedRWADuneSQL is the one statement a run executes. Both tables are
// the curator's uploads; the join takes the LATEST day per address from
// the price table. Every numeric is cast to VARCHAR on Dune's side so the
// literal the curator printed reaches this index as a string and is
// never re-rendered through a float (ADR-0003).
const curatedRWADuneSQL = `
SELECT ra.contract_id AS address,
       COALESCE(ra.asset_code, '')    AS asset_code,
       COALESCE(ra.asset_issuer, '')  AS asset_issuer,
       COALESCE(ra.company, '')       AS company,
       COALESCE(ra.asset_subclass, '') AS asset_subclass,
       CAST(p.close_usd AS VARCHAR)   AS close_usd,
       CAST(p.day AS VARCHAR)         AS priced_day
FROM dune.stellar.dataset_recognized_assets ra
LEFT JOIN (
  SELECT asset_contract_id, close_usd, day,
         ROW_NUMBER() OVER (PARTITION BY asset_contract_id ORDER BY day DESC) AS rn
  FROM dune.stellar.dataset_asset_prices
) p ON p.asset_contract_id = ra.contract_id AND p.rn = 1
WHERE ra.asset_class = 'RWA'`

var (
	curatedRWAContractRe = regexp.MustCompile(`^C[A-Z2-7]{55}$`)
	curatedRWAClassicRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$`)
)

// curatedRWACounts is one run's accounting, printed and emitted.
type curatedRWACounts struct {
	Rows        int
	Kept        int
	Malformed   int
	Priced      int
	Unpriced    int
	Credits     float64
	ExecutionID string
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
		return errors.New("DUNE_API_KEY is not set; this run cannot read the curator and will not pretend it did")
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

	entries, counts, err := client.fetch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Read %d rows; kept %d (%d priced, %d unpriced); %d skipped-malformed. Execution %s cost %.2f credits.\n",
		counts.Rows, counts.Kept, counts.Priced, counts.Unpriced, counts.Malformed, counts.ExecutionID, counts.Credits)

	if *textfile != "" {
		if err := writeCuratedRWATextfile(*textfile, curatedRWACuratorDune, counts, dryRun); err != nil {
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

	source := "dune.stellar.dataset_recognized_assets + dune.stellar.dataset_asset_prices via /api/v1/sql/execute; execution " + counts.ExecutionID
	upserted, pruned, err := store.ReplaceCuratedRWADirectory(ctx, curatedRWACuratorDune, entries, source)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d upserted, %d pruned (curator=%s).\n", upserted, pruned, curatedRWACuratorDune)
	return nil
}

// ─── client ─────────────────────────────────────────────────────────

type curatedRWAClient struct {
	baseURL string
	key     string
	http    *http.Client
	// poll is the status-poll interval; tests shorten it.
	poll time.Duration
}

func newCuratedRWAClient(baseURL, key string) *curatedRWAClient {
	return &curatedRWAClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http:    &http.Client{Timeout: curatedRWAFetchTimeout},
		poll:    curatedRWAPollEvery,
	}
}

func (c *curatedRWAClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("curated-rwa-sync: encode: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr) //nolint:gosec // G704: base URL defaults to a constant and an operator override is validated https-only; path is one of this file's fixed API routes
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dune-API-Key", c.key)
	req.Header.Set("User-Agent", "stellarindex-curated-rwa-sync/1")
	resp, err := c.http.Do(req) //nolint:gosec // G704: see the request construction above
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("curated-rwa-sync: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("curated-rwa-sync: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 512)])))
	}
	return b, nil
}

type duneExecuteResp struct {
	ExecutionID string `json:"execution_id"`
	State       string `json:"state"`
}

type duneStatusResp struct {
	ExecutionID          string  `json:"execution_id"`
	IsExecutionFinished  bool    `json:"is_execution_finished"`
	State                string  `json:"state"`
	ExecutionCostCredits float64 `json:"execution_cost_credits"`
	Error                *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type duneResultsResp struct {
	IsExecutionFinished bool   `json:"is_execution_finished"`
	State               string `json:"state"`
	NextOffset          *int   `json:"next_offset"`
	Result              struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	} `json:"result"`
}

// fetch runs the one statement and pages its result set.
func (c *curatedRWAClient) fetch(ctx context.Context) ([]timescale.CuratedRWAEntry, curatedRWACounts, error) {
	var counts curatedRWACounts
	b, err := c.do(ctx, http.MethodPost, "/api/v1/sql/execute",
		map[string]any{"sql": curatedRWADuneSQL, "performance": "small"})
	if err != nil {
		return nil, counts, err
	}
	var ex duneExecuteResp
	if err := json.Unmarshal(b, &ex); err != nil || ex.ExecutionID == "" {
		return nil, counts, fmt.Errorf("curated-rwa-sync: execute: no execution_id in %q", strings.TrimSpace(string(b[:min(len(b), 200)])))
	}
	counts.ExecutionID = ex.ExecutionID

	status, err := c.waitFinished(ctx, ex.ExecutionID)
	if err != nil {
		return nil, counts, err
	}
	counts.Credits = status.ExecutionCostCredits

	rows, err := c.pageRows(ctx, ex.ExecutionID)
	if err != nil {
		return nil, counts, err
	}
	entries := parseCuratedRWARows(rows, &counts)
	return entries, counts, nil
}

func (c *curatedRWAClient) waitFinished(ctx context.Context, id string) (duneStatusResp, error) {
	for {
		b, err := c.do(ctx, http.MethodGet, "/api/v1/execution/"+id+"/status", nil)
		if err != nil {
			return duneStatusResp{}, err
		}
		var st duneStatusResp
		if err := json.Unmarshal(b, &st); err != nil {
			return duneStatusResp{}, fmt.Errorf("curated-rwa-sync: status: %w", err)
		}
		if st.IsExecutionFinished {
			if st.State != "QUERY_STATE_COMPLETED" {
				msg := st.State
				if st.Error != nil {
					msg += ": " + st.Error.Message
				}
				return st, fmt.Errorf("curated-rwa-sync: execution %s did not complete: %s", id, msg)
			}
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("curated-rwa-sync: execution %s still %s: %w", id, st.State, ctx.Err())
		case <-time.After(c.poll):
		}
	}
}

func (c *curatedRWAClient) pageRows(ctx context.Context, id string) ([]map[string]json.RawMessage, error) {
	var out []map[string]json.RawMessage
	offset := 0
	for {
		b, err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("/api/v1/execution/%s/results?limit=%d&offset=%d", id, curatedRWAPageSize, offset), nil)
		if err != nil {
			return nil, err
		}
		var page duneResultsResp
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("curated-rwa-sync: results: %w", err)
		}
		out = append(out, page.Result.Rows...)
		if len(out) > curatedRWAMaxRows {
			return nil, fmt.Errorf("curated-rwa-sync: result set exceeds %d rows — this is not the curator's asset list", curatedRWAMaxRows)
		}
		if page.NextOffset == nil || *page.NextOffset <= offset || len(page.Result.Rows) == 0 {
			return out, nil
		}
		offset = *page.NextOffset
	}
}

// ─── parse ──────────────────────────────────────────────────────────

func rawString(m map[string]json.RawMessage, k string) string {
	raw, ok := m[k]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// A number that escaped the CAST still arrives as its literal digits.
	return strings.TrimSpace(strings.Trim(string(raw), `"`))
}

// parseCuratedRWARows turns the result set into entries. A malformed
// address is skipped and counted, never repaired. A price with no
// parseable day is dropped and the row kept unpriced — a price with no
// verifiable age is exactly the value the price bound exists to refuse.
func parseCuratedRWARows(rows []map[string]json.RawMessage, counts *curatedRWACounts) []timescale.CuratedRWAEntry {
	out := make([]timescale.CuratedRWAEntry, 0, len(rows))
	for _, r := range rows {
		counts.Rows++
		addr := rawString(r, "address")
		if !curatedRWAContractRe.MatchString(addr) && !curatedRWAClassicRe.MatchString(addr) {
			counts.Malformed++
			continue
		}
		e := timescale.CuratedRWAEntry{
			Address:       addr,
			AssetCode:     rawString(r, "asset_code"),
			AssetIssuer:   rawString(r, "asset_issuer"),
			Company:       rawString(r, "company"),
			AssetSubclass: rawString(r, "asset_subclass"),
		}
		if price, day, ok := curatedRWAPrice(rawString(r, "close_usd"), rawString(r, "priced_day")); ok {
			e.PriceUSD, e.PricedAt = price, day
			counts.Priced++
		} else {
			counts.Unpriced++
		}
		out = append(out, e)
		counts.Kept++
	}
	return out
}

// curatedRWAPrice accepts a decimal literal and a day the curator stamped
// it with. The day may arrive as a date or a timestamp; either way the
// stamp becomes midnight UTC of that day, which is the resolution the
// curator publishes at.
func curatedRWAPrice(price, day string) (string, time.Time, bool) {
	if price == "" || day == "" {
		return "", time.Time{}, false
	}
	if _, err := json.Number(price).Float64(); err != nil {
		return "", time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04:05.000 UTC", "2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, day); err == nil {
			t = t.UTC()
			return price, time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
		}
	}
	return "", time.Time{}, false
}

// ─── textfile ───────────────────────────────────────────────────────

// writeCuratedRWATextfile records the run for node_exporter. Written
// whole to a sibling temp file and renamed, so the collector never reads
// a half-written exposition; every family shares the file's fate.
func writeCuratedRWATextfile(path, curator string, c curatedRWACounts, dryRun bool) error {
	var b strings.Builder
	lbl := fmt.Sprintf(`{curator=%q}`, curator)
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_last_run_unix Unix time the most recent curated-RWA sync finished, pass or fail.\n# TYPE stellarindex_curated_rwa_sync_last_run_unix gauge\nstellarindex_curated_rwa_sync_last_run_unix%s %d\n", lbl, time.Now().Unix())
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_rows Rows the curator served in the most recent sync.\n# TYPE stellarindex_curated_rwa_sync_rows gauge\nstellarindex_curated_rwa_sync_rows%s %d\n", lbl, c.Kept)
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_priced Rows carrying a usable price in the most recent sync.\n# TYPE stellarindex_curated_rwa_sync_priced gauge\nstellarindex_curated_rwa_sync_priced%s %d\n", lbl, c.Priced)
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_execution_cost_credits Credits the curator's platform charged for the most recent sync's execution.\n# TYPE stellarindex_curated_rwa_sync_execution_cost_credits gauge\nstellarindex_curated_rwa_sync_execution_cost_credits%s %g\n", lbl, c.Credits)
	written := 1
	if dryRun {
		written = 0
	}
	fmt.Fprintf(&b, "# HELP stellarindex_curated_rwa_sync_written Whether the most recent sync wrote the cache (0 on a dry run).\n# TYPE stellarindex_curated_rwa_sync_written gauge\nstellarindex_curated_rwa_sync_written%s %d\n", lbl, written)

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
