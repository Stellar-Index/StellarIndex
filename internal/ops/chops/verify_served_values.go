// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
)

// verify-served-values — the "data-correct, not just code-correct"
// harness (board #14; audit theme: prior passes proved the CODE sound
// while the flagship served VALUE was wrong — CS-010's XLM market cap
// read +58% until sampled by hand).
//
// It fetches a curated set of values we SERVE and reconciles each
// against an INDEPENDENT ground truth, emitting node_exporter
// textfile gauges (same collector pattern as data-freshness.sh) so a
// drifting served value alerts within a day instead of at the next
// hand audit:
//
//   - XLM circulating + total supply vs the SDF lumen API
//     (https://developers.stellar.org/docs — dashboard.stellar.org/api/v3/lumens),
//     the canonical source CS-010's fix is measured against.
//   - USDC-on-Stellar total supply vs Stellar Expert's asset API.
//
// Deliberately NOT here: price cross-checks (the divergence worker
// compares served prices against CoinGecko/Chainlink continuously —
// duplicating it would double-count the same references) and lake
// count reconciliation (compute-completeness owns served↔lake).
//
// The check table is designed to GROW — adding a check is one entry.
// Mind the window trap (feedback_metric_window_apples_oranges):
// every ground truth here is point-in-time state, never a windowed
// counter, so both sides measure the same thing.
//
// Usage:
//
//	stellarindex-ops verify-served-values \
//	    -api http://127.0.0.1:3000 \
//	    -textfile /var/lib/node_exporter/textfile_collector/served_values.prom
//
// Empty -textfile prints the gauges to stdout (operator spot-run).
// Exit code = number of failed checks (cron/Healthchecks-friendly).
func verifyServedValues(args []string) error {
	fs := flag.NewFlagSet("verify-served-values", flag.ContinueOnError)
	apiBase := fs.String("api", "http://127.0.0.1:3000", "Base URL of our API (loopback on r1)")
	textfile := fs.String("textfile", "", "node_exporter textfile collector output path; empty = stdout")
	timeout := fs.Duration("timeout", 60*time.Second, "Overall run deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: 20 * time.Second}
	results := runServedValueChecks(ctx, client, *apiBase)

	body := renderServedValueProm(results, time.Now().UTC())
	if *textfile == "" {
		fmt.Print(body)
	} else if err := writeAtomic(*textfile, body); err != nil {
		return fmt.Errorf("write textfile: %w", err)
	}

	failed, skipped := 0, 0
	for _, r := range results {
		status := "OK"
		switch {
		case r.skipped:
			// Truth-source outage: availability, not a served-value
			// verdict — reported, never counted as a failure.
			status = "SKIP"
			skipped++
		case !r.ok:
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(os.Stderr, "verify-served-values: %-28s %-4s served=%s truth=%s rel_err=%.4f tol=%.4f (%s)\n",
			r.name, status, r.served, r.truth, r.relErr, r.tolerance, r.note)
	}
	return servedValuesExitError(len(results), failed, skipped)
}

// servedValuesExitError decides the run's exit status from its tally. Any
// FAILED (drifted) check fails the run. An ALL-SKIPPED run also fails CLOSED:
// every independent truth source was dark, so the run verified NOTHING and a
// one-shot gate must not read that as a clean pass (F5 — the
// served_value_persistently_skipped alert catches a SUSTAINED dark source, but
// a single run must fail closed too). A PARTIAL skip stays clean: some checks
// were verified, and a lone third-party outage must not fail the daily cron.
// Pure — unit-testable.
func servedValuesExitError(total, failed, skipped int) error {
	if failed > 0 {
		return fmt.Errorf("%d served-value check(s) failed", failed)
	}
	if total > 0 && skipped == total {
		return fmt.Errorf("all %d served-value check(s) SKIPPED — every independent truth source was unreachable; verified nothing (not a clean pass)", skipped)
	}
	return nil
}

// servedValueResult is one reconciled check. ok and skipped are
// mutually exclusive and jointly encode three states: verified-pass
// (ok), verified-drift (neither), and not-verified (skipped — the
// truth source was dark). skipped is NOT a pass: it must never assert
// served_value_ok=1, or a persistently-dark ground truth would mask a
// real drift behind a green gauge (the F5 fail-open).
type servedValueResult struct {
	name      string
	served    string // decimal string as served
	truth     string // decimal string from the independent source
	relErr    float64
	tolerance float64
	ok        bool
	skipped   bool
	note      string
}

// servedValueCheck declares one reconciliation: how to read our
// served value, how to read the independent truth, and how far apart
// they may drift. Both sides return NATURAL-UNIT floats (float is
// fine here: we compare at percent-level tolerances, not serve the
// value).
type servedValueCheck struct {
	name      string
	tolerance float64 // relative, e.g. 0.02 = 2%
	note      string
	served    func(ctx context.Context, c *http.Client, apiBase string) (float64, error)
	truth     func(ctx context.Context, c *http.Client) (float64, error)
}

func servedValueChecks() []servedValueCheck {
	const usdcIssuer = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	return []servedValueCheck{
		{
			name: "xlm_total_supply", tolerance: 0.005,
			note:   "vs SDF lumen API totalSupply — the CS-010 canonical source",
			served: servedSupplyField("native", "total_supply", 7),
			truth:  lumenAPIField("totalSupply"),
		},
		{
			name: "xlm_circulating_supply", tolerance: 0.02,
			note:   "vs SDF lumen API circulatingSupply. sdf_reserve_accounts configured 2026-07-02 (16 accounts from stellar/dashboard common/lumens.js; lake-verified to 4e-13) — residual ≈ the protocol fee pool (~0.03%), which is not an account and is deliberately not excluded.",
			served: servedSupplyField("native", "circulating_supply", 7),
			truth:  lumenAPIField("circulatingSupply"),
		},
		{
			name: "usdc_total_supply", tolerance: 0.02,
			note:   "vs Stellar Expert asset API supply (point-in-time state both sides)",
			served: servedSupplyField(usdcIssuer, "total_supply", 7),
			truth:  stellarExpertSupply(usdcIssuer),
		},
	}
}

func runServedValueChecks(ctx context.Context, c *http.Client, apiBase string) []servedValueResult {
	checks := servedValueChecks()
	out := make([]servedValueResult, 0, len(checks))
	for _, chk := range checks {
		out = append(out, reconcileOneCheck(ctx, c, apiBase, chk))
	}
	return out
}

// reconcileOneCheck fetches both sides of one check and classifies:
// within tolerance (ok), drifted (fail), served-side fetch error
// (fail — our own surface must answer), truth-side outage (SKIPPED,
// NaN rel_err — a dark ground-truth source is availability, not
// evidence our value is wrong, so it doesn't fail the run; but it is
// NOT a pass either — skipped never sets ok, so it can't emit
// served_value_ok=1 and hide a real drift behind a green gauge. A
// persistently-dark source shows up as a lingering served_value_skipped=1
// gauge and stale rel_err, which the alerting warns on separately).
func reconcileOneCheck(ctx context.Context, c *http.Client, apiBase string, chk servedValueCheck) servedValueResult {
	r := servedValueResult{name: chk.name, tolerance: chk.tolerance, note: chk.note}
	served, sErr := chk.served(ctx, c, apiBase)
	truth, tErr := chk.truth(ctx, c)
	switch {
	case sErr != nil:
		r.note = "served fetch: " + sErr.Error()
	case tErr != nil:
		r.skipped = true
		r.note = "truth fetch failed (skipped): " + tErr.Error()
		r.relErr = math.NaN()
	default:
		r.served = strconv.FormatFloat(served, 'f', -1, 64)
		r.truth = strconv.FormatFloat(truth, 'f', -1, 64)
		if truth != 0 {
			r.relErr = math.Abs(served-truth) / math.Abs(truth)
		} else {
			r.relErr = math.Abs(served - truth)
		}
		r.ok = r.relErr <= chk.tolerance
	}
	return r
}

// renderServedValueProm renders the textfile body. One gauge family
// per concern so alerts stay one-liner PromQL.
func renderServedValueProm(results []servedValueResult, now time.Time) string {
	b := &jsonSafeBuilder{}
	b.line("# HELP stellarindex_served_value_rel_err Relative error of a served value vs its independent ground truth.")
	b.line("# TYPE stellarindex_served_value_rel_err gauge")
	b.line("# HELP stellarindex_served_value_ok 1 when the served value is verified within tolerance of ground truth. NOT emitted for a check whose truth source was unavailable (see served_value_skipped) — absence is honest: we did not verify.")
	b.line("# TYPE stellarindex_served_value_ok gauge")
	b.line("# HELP stellarindex_served_value_skipped 1 when a check could not run because its independent truth source was unavailable (availability, not a served-value verdict).")
	b.line("# TYPE stellarindex_served_value_skipped gauge")
	b.line("# HELP stellarindex_served_value_last_run_unix When verify-served-values last completed.")
	b.line("# TYPE stellarindex_served_value_last_run_unix gauge")
	for _, r := range results {
		if !math.IsNaN(r.relErr) {
			b.line(fmt.Sprintf(`stellarindex_served_value_rel_err{check=%q} %g`, r.name, r.relErr))
		}
		skipped := 0
		if r.skipped {
			skipped = 1
		}
		b.line(fmt.Sprintf(`stellarindex_served_value_skipped{check=%q} %d`, r.name, skipped))
		// A skipped check asserts NO ok verdict: emitting ok=1 would
		// hide a real drift behind a dark truth source (F5 fail-open),
		// and ok=0 would page for a third-party outage. Absence is the
		// honest state — the skipped gauge above carries the signal.
		if r.skipped {
			continue
		}
		ok := 0
		if r.ok {
			ok = 1
		}
		b.line(fmt.Sprintf(`stellarindex_served_value_ok{check=%q} %d`, r.name, ok))
	}
	b.line(fmt.Sprintf("stellarindex_served_value_last_run_unix %d", now.Unix()))
	return b.String()
}

type jsonSafeBuilder struct{ s string }

func (b *jsonSafeBuilder) line(l string) { b.s += l + "\n" }
func (b *jsonSafeBuilder) String() string {
	return b.s
}

// writeAtomic writes via temp+rename so node_exporter never reads a
// half-written collector file (same contract as data-freshness.sh).
func writeAtomic(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil { //nolint:gosec // world-readable metrics file by design
		return err
	}
	return os.Rename(tmp, path)
}

// ─── served-side readers ────────────────────────────────────────────

// servedSupplyField reads a supply field from our own
// GET /v1/assets/{id} F2 block. The F2 supply fields are served as
// decimal strings in BASE UNITS (stroops for classic; verified
// empirically 2026-07-02 — served XLM total was exactly 1e7 × the
// natural-unit truth), so the value is scaled by 10^-decimals before
// comparison against natural-unit ground truth.
func servedSupplyField(assetID, field string, decimals int) func(context.Context, *http.Client, string) (float64, error) {
	return func(ctx context.Context, c *http.Client, apiBase string) (float64, error) {
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := getJSON(ctx, c, apiBase+"/v1/assets/"+assetID, &env); err != nil {
			return 0, err
		}
		raw, present := env.Data[field]
		if !present || raw == nil {
			return 0, fmt.Errorf("served %s.%s is null — supply pipeline not populating", assetID, field)
		}
		s, isStr := raw.(string)
		if !isStr {
			return 0, fmt.Errorf("served %s.%s is %T, want decimal string (ADR-0003)", assetID, field, raw)
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, err
		}
		return v / math.Pow10(decimals), nil
	}
}

// ─── ground-truth readers ───────────────────────────────────────────

// lumenAPIField reads a field from the SDF lumen supply API — the
// canonical XLM supply source (what stellar.org itself displays).
func lumenAPIField(field string) func(context.Context, *http.Client) (float64, error) {
	return func(ctx context.Context, c *http.Client) (float64, error) {
		var body map[string]any
		if err := getJSON(ctx, c, "https://dashboard.stellar.org/api/v3/lumens", &body); err != nil {
			return 0, err
		}
		raw, ok := body[field]
		if !ok {
			return 0, fmt.Errorf("lumen API has no %q field", field)
		}
		switch v := raw.(type) {
		case string:
			return strconv.ParseFloat(v, 64)
		case float64:
			return v, nil
		default:
			return 0, fmt.Errorf("lumen API %s is %T", field, raw)
		}
	}
}

// stellarExpertSupply reads an asset's supply from Stellar Expert.
// SE reports classic-asset supply in STROOPS (1e7 base units).
func stellarExpertSupply(assetID string) func(context.Context, *http.Client) (float64, error) {
	return func(ctx context.Context, c *http.Client) (float64, error) {
		// SE serves supply as a number OR a string depending on
		// magnitude — accept both.
		var body struct {
			Supply json.Number `json:"supply"`
		}
		url := "https://api.stellar.expert/explorer/public/asset/" + assetID
		if err := getJSON(ctx, c, url, &body); err != nil {
			return 0, err
		}
		v, err := body.Supply.Float64()
		if err != nil {
			return 0, fmt.Errorf("stellar.expert supply for %s: %w", assetID, err)
		}
		if v <= 0 {
			return 0, fmt.Errorf("stellar.expert supply for %s is %v", assetID, v)
		}
		return v / 1e7, nil
	}
}

func getJSON(ctx context.Context, c *http.Client, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stellarindex-ops/verify-served-values")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, snippet)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}
