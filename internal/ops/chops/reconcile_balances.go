// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// reconcileBalances is the stellarindex-ops `reconcile-balances`
// subcommand — an ADR-0033-style acceptance test for the "verified
// explorer" claim: it proves stellar.ledger_entry_changes (the
// substrate every completeness claim in this repo is built on)
// reflects TRUE on-chain state, by comparing our latest recorded
// NATIVE (XLM) balance for an account against an INDEPENDENT external
// source.
//
// Horizon is that external source. This is deliberate and does NOT
// violate ADR-0001 ("Horizon is not in our architecture"): ADR-0001
// scopes the ban to the PRODUCTION INGEST PIPELINE — we don't run
// Horizon, ingest from it, or proxy to it. reconcile-balances is
// none of those things; it's a one-off, read-only, operator-invoked
// VERIFIER that by construction needs a source of truth independent
// of our own pipeline, and public Horizon is the obvious one. No
// production code path depends on it.
//
// Usage: reconcile-balances (-account G... | -sample N) [-ch-addr H:P]
// [-horizon URL] [-tolerance-stroops N] [-min-recent-ledger N]
// [-sleep-ms N] [-timeout DUR]. Exactly one of -account/-sample is
// required. Exit code is the number of MISMATCHes (capped at 255),
// mirroring scripts/dev/r1-smoke.sh's "exit code = number of failed
// checks" convention so cron/Healthchecks.io can consume it directly
// — see opsutil.ExitCodeError's doc comment for how a Go subcommand
// reports a non-1 exit code without breaking realMain's flush-on-exit
// discipline.
func reconcileBalances(args []string) error { //nolint:funlen // linear: flag parse+validate, resolve account set, per-account loop, report.
	fs := flag.NewFlagSet("reconcile-balances", flag.ContinueOnError)
	account := fs.String("account", "", "reconcile exactly this account (G...); mutually exclusive with -sample")
	sample := fs.Int("sample", 0, "reconcile N accounts sampled from those active since -min-recent-ledger; mutually exclusive with -account")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address to read stellar.ledger_entry_changes from")
	horizonBase := fs.String("horizon", "https://horizon.stellar.org", "Horizon base URL — the independent external reference balance source for this verifier (see doc comment: ADR-0001 scopes its Horizon ban to production ingest, not a one-off verifier)")
	toleranceStroops := fs.Int64("tolerance-stroops", 0, "max |our_stroops - ref_stroops| still counted as MATCH")
	minRecentLedger := fs.Uint("min-recent-ledger", 60_000_000, "for -sample: only consider accounts with a ledger_entry_changes 'account' row above this ledger, so their latest snapshot approximates current chain state")
	sleepMS := fs.Int("sleep-ms", 250, "polite delay between Horizon requests (serial, not concurrent)")
	timeout := fs.Duration("timeout", 15*time.Second, "per-request Horizon HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	haveAccount := *account != ""
	haveSample := *sample > 0
	if haveAccount == haveSample {
		return fmt.Errorf("reconcile-balances: exactly one of -account or -sample is required")
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	accounts, err := reconcileResolveAccounts(ctx, *chAddr, *account, *sample, uint32(*minRecentLedger)) //nolint:gosec // ledger sequence, always in uint32 range for real usage.
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: *timeout}
	sleep := time.Duration(*sleepMS) * time.Millisecond
	fmt.Fprintf(os.Stderr, "reconcile-balances: checking %d account(s) against %s (ch=%s tolerance=%d stroops)\n",
		len(accounts), *horizonBase, *chAddr, *toleranceStroops)

	results := make([]reconcileResult, 0, len(accounts))
	for i, acct := range accounts {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "reconcile-balances: cancelled after %d/%d accounts\n", i, len(accounts))
			break
		}
		results = append(results, reconcileOneAccount(ctx, client, *chAddr, *horizonBase, acct, *toleranceStroops))
		if i < len(accounts)-1 && sleep > 0 {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
			}
		}
	}

	mismatches := printReconcileReport(os.Stdout, results)
	if mismatches == 0 {
		return nil
	}
	code := mismatches
	if code > 255 {
		fmt.Fprintf(os.Stderr, "reconcile-balances: %d mismatches exceeds the max process exit code (255) — reporting 255\n", mismatches)
		code = 255
	}
	return &opsutil.ExitCodeError{Code: code}
}

// reconcileResolveAccounts turns the parsed -account/-sample flags
// into the concrete account list to check, doing the -sample
// ClickHouse lookup when needed. Split out of reconcileBalances to
// keep that function's flag-handling linear and this file's per-piece
// testability high.
func reconcileResolveAccounts(ctx context.Context, chAddr, account string, sample int, minRecentLedger uint32) ([]string, error) {
	if account != "" {
		return []string{account}, nil
	}
	ids, err := clickhouse.SampleAccountIDs(ctx, chAddr, minRecentLedger, sample)
	if err != nil {
		return nil, fmt.Errorf("reconcile-balances: sample accounts: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("reconcile-balances: -sample found no accounts with a ledger_entry_changes row above ledger %d — lower -min-recent-ledger or check -ch-addr", minRecentLedger)
	}
	if len(ids) < sample {
		fmt.Fprintf(os.Stderr, "reconcile-balances: WARNING requested -sample %d but only found %d eligible account(s)\n", sample, len(ids))
	}
	return ids, nil
}

// reconcileOutcome classifies one account's reconciliation attempt.
type reconcileOutcome string

const (
	// outcomeMatch: our latest-recorded balance equals the current
	// chain balance within tolerance — holds when the account has had
	// no change since our last snapshot, since live ingest captures
	// every account-balance change. This is the invariant this whole
	// tool exists to check.
	outcomeMatch reconcileOutcome = "MATCH"
	// outcomeMismatch: balances disagree by more than -tolerance-stroops.
	outcomeMismatch reconcileOutcome = "MISMATCH"
	// outcomeNoData: zero rows in our lake for this account — outside
	// our coverage. Reported, not counted as a mismatch.
	outcomeNoData reconcileOutcome = "NO_DATA"
	// outcomeMergedOrAbsent: Horizon 404'd. If we hold a balance>0 for
	// this account, that's itself a signal worth an operator's eye
	// (the account existed in our data but is gone from the chain now)
	// — reported separately, never a hard failure.
	outcomeMergedOrAbsent reconcileOutcome = "MERGED_OR_ABSENT"
	// outcomeError: something other than the above went wrong (CH
	// query failure, Horizon transport error, unparseable response).
	outcomeError reconcileOutcome = "ERROR"
)

// reconcileResult is one account's full reconciliation outcome —
// everything printReconcileReport needs to render its report.
type reconcileResult struct {
	Account    string
	Outcome    reconcileOutcome
	OurStroops int64
	AtLedger   uint32
	Snapshots  uint64
	RefStroops int64
	Delta      int64
	Err        error
}

// reconcileOneAccount runs the full three-step reconciliation
// (CLAUDE.md task doc "What it does" #1-3) for a single account:
// read our balance from ClickHouse, fetch Horizon's current native
// balance, classify. Never returns an error itself — every failure
// mode is folded into the returned reconcileResult's Outcome/Err so
// the caller's loop stays uniform across N accounts.
func reconcileOneAccount(ctx context.Context, client *http.Client, chAddr, horizonBase, account string, toleranceStroops int64) reconcileResult {
	res := reconcileResult{Account: account}

	snap, found, err := clickhouse.QueryAccountBalance(ctx, chAddr, account)
	if err != nil {
		res.Outcome = outcomeError
		res.Err = err
		return res
	}
	if !found {
		res.Outcome = outcomeNoData
		return res
	}
	res.OurStroops = snap.Stroops
	res.AtLedger = snap.AtLedger
	res.Snapshots = snap.Snapshots

	refBig, status, err := fetchHorizonNativeBalance(ctx, client, horizonBase, account)
	if status == http.StatusNotFound {
		res.Outcome = outcomeMergedOrAbsent
		return res
	}
	if err != nil {
		res.Outcome = outcomeError
		res.Err = err
		return res
	}
	refStroops, err := bigStroopsToInt64(refBig)
	if err != nil {
		res.Outcome = outcomeError
		res.Err = err
		return res
	}
	res.RefStroops = refStroops

	res.Outcome, res.Delta = classifyBalances(res.OurStroops, res.RefStroops, toleranceStroops)
	return res
}

// classifyBalances compares our latest-recorded stroop balance
// against Horizon's current stroop balance and returns MATCH or
// MISMATCH plus the absolute delta.
func classifyBalances(ourStroops, refStroops, toleranceStroops int64) (reconcileOutcome, int64) {
	delta := ourStroops - refStroops
	if delta < 0 {
		delta = -delta
	}
	if delta <= toleranceStroops {
		return outcomeMatch, delta
	}
	return outcomeMismatch, delta
}

// bigStroopsToInt64 downcasts a stroop amount parsed via math/big
// into int64, the type stellar.ledger_entry_changes.balance and this
// tool's comparison arithmetic use. XLM's total supply is bounded
// well under int64 range (unlike arbitrary Soroban i128 amounts,
// which is exactly why that column is Int64 not NUMERIC — see
// deploy/clickhouse/tier1_schema.sql), so this should never fail in
// practice; it's a defensive guard, not an expected path.
func bigStroopsToInt64(v *big.Int) (int64, error) {
	if !v.IsInt64() {
		return 0, fmt.Errorf("reconcile-balances: stroop amount %s does not fit int64 (unexpected for XLM)", v.String())
	}
	return v.Int64(), nil
}

// ─── Horizon fetch ──────────────────────────────────────────────────

// horizonAccountResponse is the subset of Horizon's
// GET /accounts/{id} response this verifier needs.
type horizonAccountResponse struct {
	Balances []struct {
		AssetType string `json:"asset_type"`
		Balance   string `json:"balance"`
	} `json:"balances"`
}

// horizonNativeAssetType is the asset_type Horizon stamps on the
// native-XLM entry of an account's balances[] array.
const horizonNativeAssetType = "native"

// errNoNativeBalance signals a well-formed Horizon response with no
// native-asset entry — shouldn't happen for a real account (every
// Stellar account holds a native-XLM balance by construction) but
// guarded rather than assumed.
var errNoNativeBalance = errors.New("reconcile-balances: horizon account response has no native balance entry")

// horizonMaxRetriesOn429 bounds how many times fetchHorizonNativeBalance
// retries a single account after a 429 before giving up and reporting
// it as an error — keeps one rate-limited account from stalling a
// -sample run indefinitely.
const horizonMaxRetriesOn429 = 3

// horizonMaxResponseBytes caps how much of a Horizon response body
// this tool will read — a public HTTP endpoint's response should
// never legitimately be large for a single-account lookup; this is a
// DoS guard, not an expected limit.
const horizonMaxResponseBytes = 1 << 20 // 1 MiB

// fetchHorizonNativeBalance GETs Horizon's /accounts/{account} and
// returns the native-XLM balance in exact stroops (never float64 —
// see xlmDecimalToStroops). status is the final HTTP status observed
// (0 on a pure transport failure); callers treat 404 as
// MERGED_OR_ABSENT rather than an error (see reconcileOneAccount).
// Retries once per 429 up to horizonMaxRetriesOn429, honoring
// Retry-After when present — "rate-limit Horizon politely."
func fetchHorizonNativeBalance(ctx context.Context, client *http.Client, base, account string) (*big.Int, int, error) {
	url := strings.TrimRight(base, "/") + "/accounts/" + account
	for attempt := 0; ; attempt++ {
		bal, status, retryAfter, err := fetchHorizonNativeBalanceOnce(ctx, client, url)
		if status != http.StatusTooManyRequests {
			return bal, status, err
		}
		if attempt >= horizonMaxRetriesOn429 {
			return nil, status, fmt.Errorf("reconcile-balances: horizon GET %s: rate-limited after %d attempt(s)", url, attempt+1)
		}
		fmt.Fprintf(os.Stderr, "reconcile-balances: horizon 429 for %s — backing off %s (attempt %d/%d)\n",
			account, retryAfter, attempt+1, horizonMaxRetriesOn429)
		select {
		case <-time.After(retryAfter):
		case <-ctx.Done():
			return nil, status, ctx.Err()
		}
	}
}

// fetchHorizonNativeBalanceOnce performs a single Horizon request
// attempt. Split out of fetchHorizonNativeBalance so the 429-retry
// loop above stays a plain, easy-to-audit for loop.
func fetchHorizonNativeBalanceOnce(ctx context.Context, client *http.Client, url string) (bal *big.Int, status int, retryAfter time.Duration, err error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if rerr != nil {
		return nil, 0, 0, rerr
	}
	req.Header.Set("Accept", "application/json")

	resp, rerr := client.Do(req)
	if rerr != nil {
		return nil, 0, 0, fmt.Errorf("reconcile-balances: horizon GET %s: %w", url, rerr)
	}
	defer func() { _ = resp.Body.Close() }()

	body, rerr := io.ReadAll(io.LimitReader(resp.Body, horizonMaxResponseBytes))
	if rerr != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("reconcile-balances: horizon GET %s: read body: %w", url, rerr)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		parsed, perr := parseHorizonNativeBalance(body)
		return parsed, resp.StatusCode, 0, perr
	case http.StatusNotFound:
		return nil, resp.StatusCode, 0, nil
	case http.StatusTooManyRequests:
		return nil, resp.StatusCode, horizonRetryAfter(resp.Header.Get("Retry-After")), nil
	default:
		return nil, resp.StatusCode, 0, fmt.Errorf("reconcile-balances: horizon GET %s: unexpected status %d: %s",
			url, resp.StatusCode, opsutil.Truncate(string(body), 200))
	}
}

// horizonRetryAfter parses an RFC 7231 Retry-After (seconds form —
// Horizon does not use the HTTP-date form) into a bounded backoff
// duration. Falls back to a fixed 3s when the header is absent or
// unparseable; caps at 30s so a misbehaving/huge value can't stall a
// -sample run for an unreasonable time.
func horizonRetryAfter(v string) time.Duration {
	if v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			d := time.Duration(secs) * time.Second
			if d > 30*time.Second {
				d = 30 * time.Second
			}
			return d
		}
	}
	return 3 * time.Second
}

// parseHorizonNativeBalance extracts + converts the native-XLM
// balance out of a Horizon GET /accounts/{id} JSON body.
func parseHorizonNativeBalance(body []byte) (*big.Int, error) {
	var parsed horizonAccountResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("reconcile-balances: parse horizon account response: %w", err)
	}
	for _, b := range parsed.Balances {
		if b.AssetType == horizonNativeAssetType {
			return xlmDecimalToStroops(b.Balance)
		}
	}
	return nil, errNoNativeBalance
}

// xlmStroopDecimals is XLM's fixed 7 decimal places: 1 XLM = 10^7 stroops.
const xlmStroopDecimals = 7

// xlmDecimalToStroops converts Horizon's decimal-string XLM balance
// (e.g. "1234.5670000") into an EXACT stroop count. Reuses
// internal/sources/external/scale's exact decimal-string parser — the
// same helper every off-chain source in this repo already uses to
// avoid the float64-precision trap ADR-0003 warns about for money.
// Never routes through float64.
func xlmDecimalToStroops(s string) (*big.Int, error) {
	v, err := scale.DecimalStringToScaledInt(s, xlmStroopDecimals)
	if err != nil {
		return nil, fmt.Errorf("reconcile-balances: parse horizon native balance %q: %w", s, err)
	}
	return v, nil
}

// ─── report ─────────────────────────────────────────────────────────

// printReconcileReport writes the full per-account + summary report
// to w and returns the MISMATCH count (the caller's exit code).
func printReconcileReport(w io.Writer, results []reconcileResult) int {
	var matched, mismatched, noData, mergedAbsent, errored int
	var mismatchRows, errorRows []reconcileResult
	for _, r := range results {
		switch r.Outcome {
		case outcomeMatch:
			matched++
		case outcomeMismatch:
			mismatched++
			mismatchRows = append(mismatchRows, r)
		case outcomeNoData:
			noData++
		case outcomeMergedOrAbsent:
			mergedAbsent++
		case outcomeError:
			errored++
			errorRows = append(errorRows, r)
		}
	}

	_, _ = fmt.Fprintf(w, "\n=== reconcile-balances: %d account(s) checked ===\n", len(results))
	_, _ = fmt.Fprintf(w, "%-18s %8d\n", "MATCHED", matched)
	_, _ = fmt.Fprintf(w, "%-18s %8d\n", "MISMATCH", mismatched)
	_, _ = fmt.Fprintf(w, "%-18s %8d\n", "NO_DATA", noData)
	_, _ = fmt.Fprintf(w, "%-18s %8d\n", "MERGED_OR_ABSENT", mergedAbsent)
	if errored > 0 {
		_, _ = fmt.Fprintf(w, "%-18s %8d\n", "ERROR", errored)
	}

	if len(mismatchRows) > 0 {
		_, _ = fmt.Fprintln(w, "\nMismatches:")
		for _, r := range mismatchRows {
			_, _ = fmt.Fprintf(w, "  %s  our=%d@ledger_%d  ref=%d  delta=%d stroops\n",
				r.Account, r.OurStroops, r.AtLedger, r.RefStroops, r.Delta)
		}
	}
	if len(errorRows) > 0 {
		_, _ = fmt.Fprintln(w, "\nErrors:")
		for _, r := range errorRows {
			_, _ = fmt.Fprintf(w, "  %s  %v\n", r.Account, r.Err)
		}
	}

	_, _ = fmt.Fprintf(w, "\nreconcile-balances: %d checked, %d matched, %d mismatch, %d no_data, %d merged_or_absent, %d error\n",
		len(results), matched, mismatched, noData, mergedAbsent, errored)

	return mismatched
}
