// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// SDF reserve-list drift — the LIST-level companion to the
// xlm_circulating_supply VALUE cross-check (tail-triage C4-069).
//
// The value check reconciles our served circulating supply against
// dashboard.stellar.org within 2% of circulating (~700M XLM). That
// bound is what the check is FOR (methodology residuals such as the
// fee pool), but it is also a blind spot: SDF adding or retiring one
// reserve account moves the total by whatever that account holds, and
// the smaller program hot wallets hold well under 2% of circulating,
// so a stale `supply.sdf_reserve_accounts` would over- or under-state
// circulating supply indefinitely behind a green value check.
//
// This check closes that gap by diffing the configured account SET
// against the set SDF publishes. What SDF publishes machine-readably
// is NOT the dashboard API — every version of it (/api/lumens,
// /api/v2/lumens, /api/v3/lumens, /api/v3/lumens/all; probed
// 2026-09-18) returns program-level sums (`sdfMandate`,
// `upgradeReserve`, `programs.*`) and never an account id. The list
// itself lives in the source of that dashboard, stellar/dashboard
// `common/lumens.js`: an `accounts` table plus the
// `networkUpgradeReserveAccount` constant, which together are exactly
// what `noncirculatingSupply()` subtracts (the fee pool aside) to
// produce the `circulatingSupply` figure the value check reads. That
// is the same source the configured list was transcribed from on
// 2026-07-02 (see configs/ansible/roles/archival-node/defaults/main.yml),
// so a diff against it is a diff against the operator's own citation.
//
// The `voidAccount` in the same file is the BURN address: it is
// subtracted from total supply, not from circulating, and is
// deliberately not part of the published set here.
//
// Parsing JavaScript source is the honest option, not the elegant one,
// so the parser is narrow and fails CLOSED into "skipped": a missing
// accounts block, an empty one, or a missing upgrade-reserve constant
// is a truth-source outage (served_value_skipped=1, no drift verdict),
// never a spurious drift and never a spurious pass. The
// stellarindex_served_value_persistently_skipped alert then names the
// source once it stays dark for two runs, the same as for the value
// checks.

// sdfReserveListURL is the reserve list SDF publishes: the source of
// dashboard.stellar.org's circulatingSupply computation.
const sdfReserveListURL = "https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js"

// sdfReserveListCheck is the check name this verdict is emitted under
// in the served_value_* families (skipped/last-run share the harness's
// existing alerting; the drift verdict has its own gauge family).
const sdfReserveListCheck = "sdf_reserve_list"

// reserveListDrift is the pure set difference: missing = published by
// SDF but absent from our config (we over-state circulating supply by
// that account's balance); extra = configured by us but no longer
// published (we under-state it). Both slices are sorted and
// duplicate-free so the verdict is deterministic.
type reserveListDrift struct {
	missing []string
	extra   []string
}

func (d reserveListDrift) empty() bool { return len(d.missing) == 0 && len(d.extra) == 0 }

// reserveListResult is the list check's verdict. skipped and err are
// mutually exclusive with a diff: skipped means the PUBLISHED list was
// unreachable or unparseable (availability, not a verdict — the same
// F5 semantics as servedValueResult.skipped); err means OUR side could
// not be read (the config file), which is a failure, not a skip — our
// own surface must answer, as for a served-side fetch error.
type reserveListResult struct {
	configured []string
	published  []string
	drift      reserveListDrift
	skipped    bool
	err        error
	note       string
}

// verified reports whether both sides were read and the diff was
// actually computed. Only a verified result carries a drift verdict.
func (r reserveListResult) verified() bool { return !r.skipped && r.err == nil }

// ok is true only for a verified, drift-free result — never for a
// skipped one (a dark published list must not read as "in step").
func (r reserveListResult) ok() bool { return r.verified() && r.drift.empty() }

// diffReserveList computes the symmetric difference of two account
// lists. Inputs are deduplicated and the outputs sorted; comparison is
// exact (G-strkeys are case-sensitive base32, so no normalisation is
// applied — a mis-cased key is a wrong key).
func diffReserveList(configured, published []string) reserveListDrift {
	have := make(map[string]struct{}, len(configured))
	for _, a := range configured {
		have[a] = struct{}{}
	}
	want := make(map[string]struct{}, len(published))
	for _, a := range published {
		want[a] = struct{}{}
	}
	var d reserveListDrift
	for a := range want {
		if _, ok := have[a]; !ok {
			d.missing = append(d.missing, a)
		}
	}
	for a := range have {
		if _, ok := want[a]; !ok {
			d.extra = append(d.extra, a)
		}
	}
	sort.Strings(d.missing)
	sort.Strings(d.extra)
	return d
}

var (
	// reserveAccountsBlock captures the body of the `accounts` table in
	// common/lumens.js. `[^}]*` is enough because the table is flat —
	// string values only, no nested braces.
	reserveAccountsBlock = regexp.MustCompile(`(?s)\bconst\s+accounts\s*=\s*\{([^}]*)\}`)
	// upgradeReserveConst captures the network-upgrade reserve account,
	// which lives OUTSIDE the accounts table but inside
	// noncirculatingSupply(). `\s*` spans the line break the file's
	// formatter puts after the `=`.
	upgradeReserveConst = regexp.MustCompile(`\bnetworkUpgradeReserveAccount\s*=\s*"(G[A-Z2-7]{55})"`)
	// quotedStrkey matches a quoted G-strkey value. The quotes are part
	// of the match on purpose: a bare 56-char token inside a comment or
	// URL is not an account entry.
	quotedStrkey = regexp.MustCompile(`"(G[A-Z2-7]{55})"`)
)

// parsePublishedReserveList extracts the reserve set from the
// common/lumens.js source: every quoted G-strkey on a non-comment line
// of the `accounts` table, plus the `networkUpgradeReserveAccount`
// constant. Commented-out rows (SDF retires escrows by commenting them
// out — `escrowJan2021` is one) are excluded, which is the behaviour
// that makes a retirement visible as `extra` on our side.
//
// Any shape the parser does not recognise is an error, never an empty
// list: the caller turns it into a SKIP so a format change upstream
// reads as "unverifiable" rather than "every configured account is
// extra".
func parsePublishedReserveList(src string) ([]string, error) {
	m := reserveAccountsBlock.FindStringSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("no `const accounts = { … }` table in %s — source shape changed", sdfReserveListURL)
	}
	seen := make(map[string]struct{})
	var out []string
	for _, line := range strings.Split(m[1], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, sm := range quotedStrkey.FindAllStringSubmatch(line, -1) {
			if _, dup := seen[sm[1]]; dup {
				continue
			}
			seen[sm[1]] = struct{}{}
			out = append(out, sm[1])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the `accounts` table in %s has no G-strkey entries — source shape changed", sdfReserveListURL)
	}
	um := upgradeReserveConst.FindStringSubmatch(src)
	if um == nil {
		return nil, fmt.Errorf("no `networkUpgradeReserveAccount` constant in %s — source shape changed", sdfReserveListURL)
	}
	if _, dup := seen[um[1]]; !dup {
		out = append(out, um[1])
	}
	sort.Strings(out)
	return out, nil
}

// fetchPublishedReserveList reads and parses the published source at
// url (sdfReserveListURL in production; a stub in tests).
func fetchPublishedReserveList(ctx context.Context, c *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stellarindex-ops/verify-served-values")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, snippet)
	}
	src, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parsePublishedReserveList(string(src))
}

// loadConfiguredReserveAccounts reads `[supply] sdf_reserve_accounts`
// from the node's stellarindex.toml — the ONLY writer of the configured
// set (SupplyConfig.SDFReserveAccounts carries no `env:` override).
//
// Deliberately a partial decode rather than config.Load: Load
// validates the WHOLE file, and an unrelated validation failure (or
// an unknown key from a newer binary's config) must not turn the list
// check into a daily failure. This reads one key and ignores the rest.
// The result is deduplicated and sorted; a duplicate entry in the file
// is one account, not two.
func loadConfiguredReserveAccounts(path string) ([]string, error) {
	var cfg struct {
		Supply struct {
			SDFReserveAccounts []string `toml:"sdf_reserve_accounts"`
		} `toml:"supply"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	seen := make(map[string]struct{}, len(cfg.Supply.SDFReserveAccounts))
	out := make([]string, 0, len(cfg.Supply.SDFReserveAccounts))
	for _, a := range cfg.Supply.SDFReserveAccounts {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// reconcileReserveList runs the list check end to end: our configured
// set from cfgPath, the published set from sourceURL, and the diff. The
// three outcomes mirror reconcileOneCheck: config unreadable → err
// (our side; fails the run), published list unreachable/unparseable →
// skipped (their side; never a verdict), otherwise a verified diff
// whose emptiness is the verdict.
func reconcileReserveList(ctx context.Context, c *http.Client, cfgPath, sourceURL string) reserveListResult {
	r := reserveListResult{note: "vs stellar/dashboard common/lumens.js — the accounts dashboard.stellar.org subtracts for circulatingSupply"}
	configured, cErr := loadConfiguredReserveAccounts(cfgPath)
	if cErr != nil {
		r.err = cErr
		r.note = "configured list: " + cErr.Error()
		return r
	}
	r.configured = configured
	published, pErr := fetchPublishedReserveList(ctx, c, sourceURL)
	if pErr != nil {
		r.skipped = true
		r.note = "published list fetch failed (skipped): " + pErr.Error()
		return r
	}
	r.published = published
	r.drift = diffReserveList(configured, published)
	return r
}
