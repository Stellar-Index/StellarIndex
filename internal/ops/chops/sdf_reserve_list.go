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

// SDF reserve-list drift diffs the configured supply.sdf_reserve_accounts set against the one
// SDF publishes, because the value check's 2% bound hides one small account being added or
// retired. The list is read from stellar/dashboard common/lumens.js: the accounts table plus
// networkUpgradeReserveAccount, but not voidAccount, which is the burn address.
//
// The parser fails closed into skipped (no verdict) on a missing or malformed block, any row
// outside the key: "G..." grammar, a missing upgrade-reserve constant, or more than
// maxPlausibleReserveRetirements retirements at once.

// sdfReserveListURL is the reserve list SDF publishes: the source of
// dashboard.stellar.org's circulatingSupply computation.
const sdfReserveListURL = "https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js"

// sdfReserveListCheck is the check name this verdict is emitted under
// in the served_value_* families (skipped/last-run share the harness's
// existing alerting; the drift verdict has its own gauge family).
const sdfReserveListCheck = "sdf_reserve_list"

// reserveListDrift is the set difference. missing is published but not configured (we overstate
// circulating supply); extra is configured but no longer published (we understate it). Both
// are sorted and duplicate-free.
type reserveListDrift struct {
	missing []string
	extra   []string
}

func (d reserveListDrift) empty() bool { return len(d.missing) == 0 && len(d.extra) == 0 }

// reserveListResult is the list check's verdict. skipped means the published list was
// unreachable, unparseable or implausibly far off (availability, not a verdict; drift is then
// set only for the log line, never a gauge). err means our own config could not be read, which
// is a failure, not a skip.
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

// maxPlausibleReserveRetirements bounds how many accounts one run may report as no longer
// published. SDF retires rows one at a time, so a larger set more likely means a reshaped source
// than real retirements, and the diff is refused. It is a conservative bound, not an SDF figure.
const maxPlausibleReserveRetirements = 3

var (
	// reserveAccountsOpen locates the `accounts` table; its extent is
	// found by accountsTableBody's brace-depth scan, never a regex, so
	// a brace inside a row or a comment cannot end it early.
	reserveAccountsOpen = regexp.MustCompile(`\bconst\s+accounts\s*=\s*\{`)
	// upgradeReserveConst captures the network-upgrade reserve account,
	// which lives OUTSIDE the accounts table but inside
	// noncirculatingSupply(). `\s*` spans the line break the file's
	// formatter puts after the `=`.
	upgradeReserveConst = regexp.MustCompile(`\bnetworkUpgradeReserveAccount\s*=\s*(?:"(G[A-Z2-7]{55})"|'(G[A-Z2-7]{55})')`)
	// reserveEntry is the ONLY row grammar the table is accepted in:
	// `key: "G…"` (either JS quote style, bare or quoted key), followed
	// by a comma or the end of the table. A row in any other shape fails
	// the whole parse rather than being skipped.
	reserveEntry = regexp.MustCompile(`^\s*(?:[A-Za-z_$][A-Za-z0-9_$]*|"[^"\n]*"|'[^'\n]*')\s*:\s*(?:"(G[A-Z2-7]{55})"|'(G[A-Z2-7]{55})')\s*(?:,|$)`)
)

// parsePublishedReserveList extracts the accounts table rows plus networkUpgradeReserveAccount
// from common/lumens.js. Commented-out rows are excluded, which is how a retirement shows as
// extra. Any unrecognised shape, even one row, is an error and never a partial list, so the
// caller skips rather than reporting phantom extra accounts.
func parsePublishedReserveList(src string) ([]string, error) {
	body, err := accountsTableBody(src)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var out []string
	for rest := body; strings.TrimSpace(rest) != ""; {
		m := reserveEntry.FindStringSubmatchIndex(rest)
		if m == nil {
			return nil, fmt.Errorf("unrecognised row %q in the `accounts` table of %s — source shape changed", firstLine(rest), sdfReserveListURL)
		}
		a := strkeyGroup(rest, m)
		rest = rest[m[1]:]
		if !isSeen(seen, a) {
			seen[a] = struct{}{}
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the `accounts` table in %s has no G-strkey entries — source shape changed", sdfReserveListURL)
	}
	um := upgradeReserveConst.FindStringSubmatchIndex(src)
	if um == nil {
		return nil, fmt.Errorf("no `networkUpgradeReserveAccount` constant in %s — source shape changed", sdfReserveListURL)
	}
	if a := strkeyGroup(src, um); !isSeen(seen, a) {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// strkeyGroup returns whichever of the two quote-style capture groups
// (1: double, 2: single) matched in a FindStringSubmatchIndex result.
func strkeyGroup(s string, m []int) string {
	if m[2] >= 0 {
		return s[m[2]:m[3]]
	}
	return s[m[4]:m[5]]
}

func isSeen(set map[string]struct{}, k string) bool {
	_, ok := set[k]
	return ok
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// accountsTableBody returns the text between `const accounts = {` and
// its MATCHING close brace, comments removed. String literals are copied
// through whole so a brace or `//` inside one is not structure.
func accountsTableBody(src string) (string, error) {
	loc := reserveAccountsOpen.FindStringIndex(src)
	if loc == nil {
		return "", fmt.Errorf("no `const accounts = { … }` table in %s — source shape changed", sdfReserveListURL)
	}
	var b strings.Builder
	depth := 1
	for i := loc[1]; i < len(src); {
		n := jsTokenLen(src[i:])
		if n < 0 {
			break
		}
		tok := src[i : i+n]
		i += n
		switch {
		case strings.HasPrefix(tok, "//"), strings.HasPrefix(tok, "/*"):
			b.WriteByte(' ')
			continue
		case tok == "{":
			depth++
		case tok == "}":
			depth--
		}
		if depth == 0 {
			return b.String(), nil
		}
		b.WriteString(tok)
	}
	return "", fmt.Errorf("the `accounts` table in %s is not closed — source shape changed", sdfReserveListURL)
}

// jsTokenLen is the length of the lexical unit at the start of s: a
// whole comment (a line comment stops before its newline), a whole
// quoted string, or one byte. -1 means an unterminated comment or string.
func jsTokenLen(s string) int {
	switch {
	case strings.HasPrefix(s, "//"):
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return i
		}
		return len(s)
	case strings.HasPrefix(s, "/*"):
		if i := strings.Index(s[2:], "*/"); i >= 0 {
			return i + 4
		}
		return -1
	case s[0] == '"' || s[0] == '\'' || s[0] == '`':
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case s[0]:
				return i + 1
			}
		}
		return -1
	}
	return 1
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

// loadConfiguredReserveAccounts reads [supply] sdf_reserve_accounts from the toml, the only
// source of the configured set. It is a partial decode rather than config.Load so an unrelated
// validation failure cannot turn the check into a daily failure. The result is deduplicated
// and sorted.
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

// reconcileReserveList runs the list check end to end. An unreadable config is err (fails the
// run), an unreachable or unparseable published list is skipped, otherwise the diff is the verdict.
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
	if n := len(r.drift.extra); n > maxPlausibleReserveRetirements {
		r.skipped = true
		r.note = fmt.Sprintf("published list retires %d configured accounts at once, above the %d a real SDF edit plausibly does — implausible diff refused (skipped); confirm by hand before editing config", n, maxPlausibleReserveRetirements)
	}
	return r
}
