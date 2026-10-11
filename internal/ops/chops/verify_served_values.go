// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"time"
)

// verify-served-values reconciles values we SERVE against independent ground
// truth, because sound code does not prove a served value right. It emits
// node_exporter textfile gauges so drift alerts within a day:
//   - XLM circulating + total supply vs the SDF lumen API
//     (dashboard.stellar.org/api/v3/lumens).
//   - USDC-on-Stellar total supply vs Stellar Expert's asset API.
//   - The configured SDF reserve-account LIST vs SDF's published list
//     (sdf_reserve_list.go); the 2% value tolerance cannot see one added or
//     retired account.
//
// Price cross-checks belong to the divergence worker, served-vs-lake counts to
// compute-completeness. Every ground truth here is point-in-time state, never
// a windowed counter, so both sides measure the same thing.
//
// Flags: -api, -config, -textfile. Empty -textfile prints to stdout; empty
// -config skips the reserve-list check. Exits 1 when any check failed or every
// VALUE check was skipped (servedValuesExitError).
func verifyServedValues(args []string) error {
	fs := flag.NewFlagSet("verify-served-values", flag.ContinueOnError)
	apiBase := fs.String("api", "http://127.0.0.1:3000", "Base URL of our API (loopback on r1)")
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "stellarindex.toml whose supply.sdf_reserve_accounts is diffed against the reserve list SDF publishes; empty skips that check")
	textfile := fs.String("textfile", "", "node_exporter textfile collector output path; empty = stdout")
	timeout := fs.Duration("timeout", 60*time.Second, "Deadline for each read pass; a drift recheck waits one supply-snapshot interval between passes, outside this deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := &http.Client{Timeout: 20 * time.Second}
	results := runServedValueChecks(context.Background(), client, *apiBase, servedValueChecks(), *timeout, os.Stderr)
	var list *reserveListResult
	if *cfgPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		lr := reconcileReserveList(ctx, client, *cfgPath, sdfReserveListURL)
		list = &lr
	}

	body := renderServedValueProm(results, list, time.Now().UTC())
	if *textfile == "" {
		fmt.Print(body)
	} else if err := writeAtomic(*textfile, body); err != nil {
		return fmt.Errorf("write textfile: %w", err)
	}

	return servedValuesVerdict(os.Stderr, results, list)
}

// servedValuesVerdict writes one status line per check to w and returns
// the run's exit error. The list check can FAIL the run but never counts
// toward the value checks' all-skipped guard (see servedValuesExitError).
func servedValuesVerdict(w io.Writer, results []servedValueResult, list *reserveListResult) error {
	failed, skipped := 0, 0
	if list != nil {
		status := "OK"
		switch {
		case list.skipped:
			status = "SKIP"
		case !list.ok():
			status = "FAIL"
			failed++
		}
		_, _ = fmt.Fprintf(w, "verify-served-values: %-28s %-4s configured=%d published=%d missing=%v extra=%v (%s)\n",
			sdfReserveListCheck, status, len(list.configured), len(list.published), list.drift.missing, list.drift.extra, list.note)
	}
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
		_, _ = fmt.Fprintf(w, "verify-served-values: %-28s %-4s served=%s truth=%s rel_err=%.4f tol=%.4f (%s)\n",
			r.name, status, r.served, r.truth, r.relErr, r.tolerance, r.note)
	}
	return servedValuesExitError(len(results), failed, skipped)
}

// servedValuesExitError decides the run's exit status from its tally. total
// and skipped count the VALUE checks only; failed counts any check, the
// reserve-list check included. Any FAILED check fails the run. A run whose
// value checks ALL skipped also fails CLOSED: every truth source for a served
// number was dark, so the run verified no served value and a one-shot gate
// must not read that as a clean pass (F5 — the
// served_value_persistently_skipped alert catches a SUSTAINED dark source, but
// a single run must fail closed too). The list check is excluded from that
// denominator because it verifies a config list, not a served number: a
// verified list must not turn a run with every value source dark into exit 0.
// A PARTIAL skip stays clean, and a lone third-party outage (the list's
// source included) must not fail the daily cron. Pure — unit-testable.
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
// they may drift. Both sides are exact NATURAL-UNIT rationals (ADR-0003:
// supplies above 2^53 base units do not survive float64).
type servedValueCheck struct {
	name      string
	tolerance float64 // relative, e.g. 0.02 = 2%
	note      string
	served    func(ctx context.Context, c *http.Client, apiBase string) (servedReading, error)
	truth     func(ctx context.Context, c *http.Client) (*big.Rat, error)
	// recheckAfter is how long a drifted check waits before re-reading both
	// sides: the served supply is a periodic snapshot, so a real mint or burn
	// between the snapshot and the truth read is a gap until the next one.
	recheckAfter time.Duration
}

// servedReading is one served supply value and the ledger its snapshot was
// taken at (0 when the API omits supply_as_of_ledger).
type servedReading struct {
	value      *big.Rat
	asOfLedger uint32
}

// supplySnapshotRecheck exceeds the supply writer's 5-minute snapshot
// cadence, so the recheck reads a snapshot taken after the first read.
const supplySnapshotRecheck = 6 * time.Minute

func servedValueChecks() []servedValueCheck {
	const usdcIssuer = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	return []servedValueCheck{
		{
			name: "xlm_total_supply", tolerance: 0.005,
			note:         "vs SDF lumen API totalSupply — the CS-010 canonical source",
			served:       servedSupplyField("native", "total_supply", 7),
			truth:        lumenAPIField("totalSupply"),
			recheckAfter: supplySnapshotRecheck,
		},
		{
			name: "xlm_circulating_supply", tolerance: 0.02,
			note:         "vs SDF lumen API circulatingSupply. sdf_reserve_accounts configured 2026-07-02 (16 accounts from stellar/dashboard common/lumens.js; lake-verified to 4e-13) — residual ≈ the protocol fee pool (~0.03%), which is not an account and is deliberately not excluded.",
			served:       servedSupplyField("native", "circulating_supply", 7),
			truth:        lumenAPIField("circulatingSupply"),
			recheckAfter: supplySnapshotRecheck,
		},
		{
			name: "usdc_total_supply", tolerance: 0.02,
			note:         "vs Stellar Expert asset API supply (point-in-time state both sides)",
			served:       servedSupplyField(usdcIssuer, "total_supply", 7),
			truth:        stellarExpertSupply(usdcIssuer),
			recheckAfter: supplySnapshotRecheck,
		},
	}
}

// supplyObservation is one read of both sides of a check.
type supplyObservation struct {
	served     servedReading
	truth      *big.Rat
	sErr, tErr error
}

func observeCheck(ctx context.Context, c *http.Client, apiBase string, chk servedValueCheck) supplyObservation {
	var o supplyObservation
	o.served, o.sErr = chk.served(ctx, c, apiBase)
	o.truth, o.tErr = chk.truth(ctx, c)
	return o
}

// runServedValueChecks reads every check once, then re-reads only the
// drifted ones after a single shared wait (the longest recheckAfter among
// them). passTimeout bounds each read pass; the wait is outside it and ends
// early, failing the drifted checks, if ctx is done.
func runServedValueChecks(ctx context.Context, c *http.Client, apiBase string, checks []servedValueCheck, passTimeout time.Duration, log io.Writer) []servedValueResult {
	first := make([]supplyObservation, len(checks))
	out := make([]servedValueResult, len(checks))
	var drifted []int
	var wait time.Duration
	func() {
		pctx, cancel := context.WithTimeout(ctx, passTimeout)
		defer cancel()
		for i, chk := range checks {
			first[i] = observeCheck(pctx, c, apiBase, chk)
			out[i] = classifyObservation(chk, first[i])
			if isMeasuredDrift(out[i]) {
				drifted = append(drifted, i)
				wait = max(wait, chk.recheckAfter)
			}
		}
	}()
	if len(drifted) == 0 {
		return out
	}

	_, _ = fmt.Fprintf(log, "verify-served-values: %d check(s) out of tolerance; re-reading both sides in %s\n", len(drifted), wait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		for _, i := range drifted {
			out[i].note += "; recheck aborted before it ran: " + ctx.Err().Error()
		}
		return out
	case <-timer.C:
	}

	pctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	for _, i := range drifted {
		second := observeCheck(pctx, c, apiBase, checks[i])
		out[i] = settleRecheck(checks[i], first[i], out[i], second)
	}
	return out
}

// isMeasuredDrift reports a verdict reached on both sides and out of tolerance.
func isMeasuredDrift(r servedValueResult) bool {
	return !r.ok && !r.skipped && !math.IsNaN(r.relErr)
}

// settleRecheck decides a drifted check from its second read. A gap that
// clears is the snapshot race, not a served-value defect. A gap that
// persists fails, but only names the served value wrong once its snapshot
// ledger has advanced past the first read's; otherwise the served snapshot
// is stale. A failed re-read keeps the first verdict (fail-closed).
func settleRecheck(chk servedValueCheck, first supplyObservation, firstResult servedValueResult, second supplyObservation) servedValueResult {
	if second.sErr != nil || second.tErr != nil {
		firstResult.note += fmt.Sprintf("; recheck read failed, first verdict stands: %v", errors.Join(second.sErr, second.tErr))
		return firstResult
	}
	r := classifyObservation(chk, second)
	from, to := first.served.asOfLedger, second.served.asOfLedger
	switch {
	case r.ok:
		r.note = fmt.Sprintf("%s; first read rel_err=%.4f cleared on recheck (supply_as_of_ledger %d -> %d)", chk.note, firstResult.relErr, from, to)
	case to > from:
		r.note = fmt.Sprintf("%s; gap persisted after the served snapshot advanced (supply_as_of_ledger %d -> %d)", chk.note, from, to)
	default:
		r.note = fmt.Sprintf("served supply snapshot stale: supply_as_of_ledger %d did not advance past %d before the recheck; %s", to, from, chk.note)
	}
	return r
}

// classifyObservation classifies one read: within tolerance (ok),
// drifted (fail), served-side fetch error
// (fail — our own surface must answer), truth-side outage (SKIPPED,
// NaN rel_err — a dark ground-truth source is availability, not
// evidence our value is wrong, so it doesn't fail the run; but it is
// NOT a pass either — skipped never sets ok, so it can't emit
// served_value_ok=1 and hide a real drift behind a green gauge. A
// persistently-dark source shows up as a lingering served_value_skipped=1
// gauge and stale rel_err, which the alerting warns on separately).
func classifyObservation(chk servedValueCheck, o supplyObservation) servedValueResult {
	r := servedValueResult{name: chk.name, tolerance: chk.tolerance, note: chk.note}
	switch {
	case o.sErr != nil:
		// Failed, not measured: a 0 rel_err would read as a perfect match.
		r.note = "served fetch: " + o.sErr.Error()
		r.relErr = math.NaN()
	case o.tErr != nil:
		r.skipped = true
		r.note = "truth fetch failed (skipped): " + o.tErr.Error()
		r.relErr = math.NaN()
	default:
		served, truth := o.served.value, o.truth
		r.served = served.FloatString(7)
		r.truth = truth.FloatString(7)
		rel := new(big.Rat).Sub(served, truth)
		rel.Abs(rel)
		if truth.Sign() != 0 {
			rel.Quo(rel, new(big.Rat).Abs(truth))
		}
		r.relErr, _ = rel.Float64() // i128:ok relative-error ratio for the gauge, not an amount; the verdict below is exact
		r.ok = rel.Cmp(new(big.Rat).SetFloat64(chk.tolerance)) <= 0
	}
	return r
}

// renderServedValueProm renders the textfile body. One gauge family
// per concern so alerts stay one-liner PromQL. list is nil when the
// reserve-list check did not run (no -config); a nil list emits
// nothing for it rather than a verdict.
func renderServedValueProm(results []servedValueResult, list *reserveListResult, now time.Time) string {
	b := &jsonSafeBuilder{}
	b.line("# HELP stellarindex_served_value_rel_err Relative error of a served value vs its independent ground truth.")
	b.line("# TYPE stellarindex_served_value_rel_err gauge")
	b.line("# HELP stellarindex_served_value_ok 1 when the served value is verified within tolerance of ground truth. NOT emitted for a check whose truth source was unavailable (see served_value_skipped) — absence is honest: we did not verify.")
	b.line("# TYPE stellarindex_served_value_ok gauge")
	b.line("# HELP stellarindex_served_value_skipped 1 when a check could not run because its independent truth source was unavailable (availability, not a served-value verdict).")
	b.line("# TYPE stellarindex_served_value_skipped gauge")
	b.line("# HELP stellarindex_served_value_last_run_unix When verify-served-values last completed a run that reached at least one verdict. NOT emitted when every check was skipped.")
	b.line("# TYPE stellarindex_served_value_last_run_unix gauge")
	b.line("# HELP stellarindex_sdf_reserve_list_drift Accounts by which supply.sdf_reserve_accounts differs from the reserve list SDF publishes (stellar/dashboard common/lumens.js): kind=missing are published but not configured, kind=extra are configured but no longer published. NOT emitted when the published list was unreachable (see served_value_skipped{check=sdf_reserve_list}) or the config was unreadable.")
	b.line("# TYPE stellarindex_sdf_reserve_list_drift gauge")
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
	if list != nil {
		// The list check shares served_value_skipped so the
		// persistently_skipped alert covers a dark published list,
		// but carries its verdict in its own family: a list drift is
		// a CONFIG-vs-publication finding with an account-level
		// remedy, not a served number outside a tolerance, so it
		// gets its own alert rather than riding served_value_ok.
		skipped := 0
		if list.skipped {
			skipped = 1
		}
		b.line(fmt.Sprintf(`stellarindex_served_value_skipped{check=%q} %d`, sdfReserveListCheck, skipped))
		// Same absence-is-honest rule as served_value_ok: no drift
		// gauge unless both sides were read and actually diffed.
		if list.verified() {
			b.line(fmt.Sprintf(`stellarindex_sdf_reserve_list_drift{kind=%q} %d`, "missing", len(list.drift.missing)))
			b.line(fmt.Sprintf(`stellarindex_sdf_reserve_list_drift{kind=%q} %d`, "extra", len(list.drift.extra)))
		}
	}
	// The staleness alert keys on this, so a run that reached no verdict
	// must not refresh it.
	if servedValuesReachedVerdict(results, list) {
		b.line(fmt.Sprintf("stellarindex_served_value_last_run_unix %d", now.Unix()))
	}
	return b.String()
}

// servedValuesReachedVerdict reports whether any check produced a pass or
// fail verdict rather than a truth-source skip.
func servedValuesReachedVerdict(results []servedValueResult, list *reserveListResult) bool {
	if list != nil && !list.skipped {
		return true
	}
	for _, r := range results {
		if !r.skipped {
			return true
		}
	}
	return false
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
// empirically — served XLM total was exactly 1e7 × the
// natural-unit truth), so the value is scaled by 10^-decimals before
// comparison against natural-unit ground truth. supply_as_of_ledger is
// read alongside so a drift recheck can tell a new snapshot from a stale one.
func servedSupplyField(assetID, field string, decimals int) func(context.Context, *http.Client, string) (servedReading, error) {
	return func(ctx context.Context, c *http.Client, apiBase string) (servedReading, error) {
		var env struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := getJSON(ctx, c, apiBase+"/v1/assets/"+assetID, &env); err != nil {
			return servedReading{}, err
		}
		raw, present := env.Data[field]
		if !present || string(raw) == "null" {
			return servedReading{}, fmt.Errorf("served %s.%s is null — supply pipeline not populating", assetID, field)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return servedReading{}, fmt.Errorf("served %s.%s is %s, want decimal string (ADR-0003)", assetID, field, raw)
		}
		base, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return servedReading{}, fmt.Errorf("served %s.%s %q is not a base-unit integer", assetID, field, s)
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
		r := servedReading{value: new(big.Rat).SetFrac(base, scale)}
		if l, has := env.Data["supply_as_of_ledger"]; has && string(l) != "null" {
			if err := json.Unmarshal(l, &r.asOfLedger); err != nil {
				return servedReading{}, fmt.Errorf("served %s.supply_as_of_ledger: %w", assetID, err)
			}
		}
		return r, nil
	}
}

// ─── ground-truth readers ───────────────────────────────────────────

// decimalRat parses a JSON decimal (a quoted string or a bare number literal)
// exactly; float64 would round supplies above 2^53.
func decimalRat(raw json.RawMessage) (*big.Rat, error) {
	s := string(raw)
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		s = quoted
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%s is not a decimal", raw)
	}
	return v, nil
}

// lumenAPIField reads a field from the SDF lumen supply API — the
// canonical XLM supply source (what stellar.org itself displays).
func lumenAPIField(field string) func(context.Context, *http.Client) (*big.Rat, error) {
	return func(ctx context.Context, c *http.Client) (*big.Rat, error) {
		var body map[string]json.RawMessage
		if err := getJSON(ctx, c, "https://dashboard.stellar.org/api/v3/lumens", &body); err != nil {
			return nil, err
		}
		raw, ok := body[field]
		if !ok {
			return nil, fmt.Errorf("lumen API has no %q field", field)
		}
		v, err := decimalRat(raw)
		if err != nil {
			return nil, fmt.Errorf("lumen API %s: %w", field, err)
		}
		return v, nil
	}
}

// stellarExpertSupply reads an asset's supply from Stellar Expert.
// SE reports classic-asset supply in STROOPS (1e7 base units).
func stellarExpertSupply(assetID string) func(context.Context, *http.Client) (*big.Rat, error) {
	return func(ctx context.Context, c *http.Client) (*big.Rat, error) {
		// SE serves supply as a number OR a string depending on
		// magnitude — accept both.
		var body struct {
			Supply json.RawMessage `json:"supply"`
		}
		url := "https://api.stellar.expert/explorer/public/asset/" + assetID
		if err := getJSON(ctx, c, url, &body); err != nil {
			return nil, err
		}
		v, err := decimalRat(body.Supply)
		if err != nil {
			return nil, fmt.Errorf("stellar.expert supply for %s: %w", assetID, err)
		}
		if v.Sign() <= 0 {
			return nil, fmt.Errorf("stellar.expert supply for %s is %s", assetID, v.RatString())
		}
		return v.Quo(v, big.NewRat(10_000_000, 1)), nil
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
