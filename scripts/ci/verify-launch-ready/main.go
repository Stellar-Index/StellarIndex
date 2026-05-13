// verify-launch-ready parses
// docs/architecture/launch-readiness-backlog.md and reports
// whether the launch-blocking engineering surface is ready.
//
// Three readiness tiers are tracked separately:
//
//  1. Engineering (L1-L3) — MUST be ✅ or ⚠ (shipped-with-caveat).
//     Any 🟢/🟡/🔴 here means engineering work is still pending
//     and we are NOT ready.
//
//  2. Ops + validation (L4-L5) — MUST be ✅, ⚠, or 🟡
//     (operator-runbook-ready). 🟡 is acceptable here because the
//     code is shipped; the remaining work is operator action that
//     fires before launch day.
//
//  3. Cutover (L6.*) — operator-action-only on launch day. This
//     CLI reports each row's status but does NOT block on them
//     being open. They flip ✅ when the operator pulls the
//     trigger.
//
// Post-launch (L7.*) is reported but ignored from the gating
// computation — those rows are explicitly deferred.
//
// Exit codes:
//
//	0 — Engineering and ops/validation tiers are ready
//	    (regardless of L6 status). Safe-to-launch from the
//	    code side.
//	1 — At least one L1-L5 row is in a non-ready state.
//	2 — The backlog couldn't be parsed (corrupt file).
//
// Usage:
//
//	go run ./scripts/ci/verify-launch-ready
//	go run ./scripts/ci/verify-launch-ready -all   # list every row
//	go run ./scripts/ci/verify-launch-ready -path docs/.../backlog.md
//	go run ./scripts/ci/verify-launch-ready -skip-ids L4.14,L4.15  # skip
//
// `-skip-ids` accepts a comma-separated list of row IDs whose
// status is treated as ✅ for gating purposes (the row is still
// listed in the report with its true status, but does not block
// the engineering-ready verdict). The `make verify-launch-ready-
// single-region` target preset bakes in the multi-region skip
// list — useful for the project's current "live-in-development on
// R1, no consumer traffic yet" posture where the R2/R3 + chaos
// rows are deferred future scope rather than launch blockers.
//
// Wire into Makefile via `make verify-launch-ready` (multi-region
// gate) or `make verify-launch-ready-single-region` (R1-only gate).
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Row is one parsed entry from the backlog table.
type Row struct {
	ID      string
	Status  string // emoji as parsed
	OneLine string // first ~80 chars of the description column
	Surface string // L1, L2, L3, L4, L5, L6, L7
}

// rowRE matches the leading `| <ID> |` of each table row.
// Captures: 1=ID. The full row contents we'll split by `|`.
var rowRE = regexp.MustCompile(`^\|\s*(L\d+\.\w+)\s*\|`)

func main() {
	path := flag.String("path", "docs/architecture/launch-readiness-backlog.md",
		"Path to the launch-readiness backlog markdown file.")
	listAll := flag.Bool("all", false, "List every row regardless of tier.")
	skipIDs := flag.String("skip-ids", "",
		"Comma-separated list of row IDs whose status to ignore for gating (e.g. 'L4.14,L4.15'). The row still prints with its real status; only the verdict changes.")
	flag.Parse()

	rows, err := parseFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-launch-ready: parse %s: %v\n", *path, err)
		os.Exit(2)
	}

	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "verify-launch-ready: %s contained zero L*.* rows — wrong file?\n", *path)
		os.Exit(2)
	}

	skip := parseSkipIDs(*skipIDs)
	report(rows, *listAll, skip)

	if !engineeringReadyWithSkip(rows, skip) {
		os.Exit(1)
	}
	os.Exit(0)
}

// parseSkipIDs turns a comma-separated flag value into a set.
// Whitespace around commas is tolerated. Empty input returns nil
// (no skips), which collapses to the legacy gate behaviour.
func parseSkipIDs(s string) map[string]struct{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]struct{}{}
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// parseFile reads the backlog and returns one Row per matched table line.
func parseFile(path string) ([]Row, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a CLI flag by design
	if err != nil {
		return nil, err
	}
	var rows []Row
	for _, line := range strings.Split(string(data), "\n") {
		m := rowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := splitRow(line)
		if len(fields) < 3 {
			// Malformed row; skip rather than crash the whole run.
			continue
		}
		id := strings.TrimSpace(fields[0])
		status := lastNonEmpty(fields)
		desc := strings.TrimSpace(fields[1])
		surface := surfaceFor(id)
		rows = append(rows, Row{
			ID:      id,
			Status:  normaliseStatus(status),
			OneLine: truncate(desc, 80),
			Surface: surface,
		})
	}
	return rows, nil
}

// splitRow splits a markdown-table row on `|`, dropping the
// leading + trailing empty cells produced by the wrapping pipes.
func splitRow(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) >= 2 {
		parts = parts[1 : len(parts)-1]
	}
	return parts
}

// lastNonEmpty returns the last non-blank-after-trim field.
// The status column is always the last column in the backlog.
func lastNonEmpty(fields []string) string {
	for i := len(fields) - 1; i >= 0; i-- {
		f := strings.TrimSpace(fields[i])
		if f != "" {
			return f
		}
	}
	return ""
}

// normaliseStatus picks the status emoji out of any free-text the
// status column might contain. Returns the first emoji that
// matches; "?" if none of the known emojis appear.
func normaliseStatus(s string) string {
	for _, e := range []string{"✅", "⚠", "🟢", "🟡", "🔴", "⏳"} {
		if strings.Contains(s, e) {
			return e
		}
	}
	return "?"
}

func surfaceFor(id string) string {
	if len(id) < 2 {
		return ""
	}
	return id[:2]
}

// truncate caps `s` to at most `n` bytes plus a trailing "…",
// walking back to the nearest UTF-8 rune boundary at or before
// byte n. Used in launch-readiness CI report rendering — row
// titles and notes routinely contain accented words / unicode
// dashes / em-dashes that a naive byte slice would split.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// readyEngineering returns true iff the row is in a state that
// counts as "engineering ready" for L1-L3.
func readyEngineering(s string) bool {
	return s == "✅" || s == "⚠"
}

// readyOpsValidation returns true iff the row is in a state that
// counts as "ready" for L4-L5: code shipped (✅/⚠) OR operator
// runbook ready (🟡).
func readyOpsValidation(s string) bool {
	return s == "✅" || s == "⚠" || s == "🟡"
}

// engineeringReady returns true iff every L1-L5 row is in a ready
// state. L6 is operator-action-only on launch day; L7 is deferred.
func engineeringReady(rows []Row) bool {
	return engineeringReadyWithSkip(rows, nil)
}

// engineeringReadyWithSkip is the [engineeringReady] variant that
// treats every row whose ID appears in `skip` as ready regardless
// of its true status. Used by the `-skip-ids` flag (and the
// `make verify-launch-ready-single-region` Makefile preset) to
// gate against a subset that matches today's project posture.
func engineeringReadyWithSkip(rows []Row, skip map[string]struct{}) bool {
	for _, r := range rows {
		if _, skipped := skip[r.ID]; skipped {
			continue
		}
		switch r.Surface {
		case "L1", "L2", "L3":
			if !readyEngineering(r.Status) {
				return false
			}
		case "L4", "L5":
			if !readyOpsValidation(r.Status) {
				return false
			}
		}
	}
	return true
}

// surfaceLabel renders a human-readable label for the surface.
func surfaceLabel(s string) string {
	switch s {
	case "L1":
		return "Ingest"
	case "L2":
		return "Aggregator"
	case "L3":
		return "API"
	case "L4":
		return "Operations"
	case "L5":
		return "SLA validation"
	case "L6":
		return "Finalisation (cutover)"
	case "L7":
		return "Post-launch (deferred)"
	}
	return s
}

func report(rows []Row, listAll bool, skip map[string]struct{}) {
	bySurface := groupBySurface(rows)

	fmt.Println(bold("Rates Engine — Launch Readiness Check"))
	fmt.Println(strings.Repeat("=", 40))
	if len(skip) > 0 {
		fmt.Printf("(skipping %d row(s) for gating: %s)\n",
			len(skip), strings.Join(sortedKeys(skip), ", "))
	}
	fmt.Println()

	for _, sf := range []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7"} {
		printSurfaceLine(sf, bySurface[sf], skip)
	}
	fmt.Println()

	if blockers := collectBlockersWithSkip(rows, skip); len(blockers) > 0 {
		printRows(bold("Blocking rows (engineering not ready):"), blockers)
	}
	if listAll {
		printRows(bold("All rows:"), rows)
	}
	printVerdict(rows, skip)
}

// sortedKeys returns the keys of a string set in deterministic order.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// printSurfaceLine emits one summary line for a single surface.
func printSurfaceLine(sf string, group []Row, skip map[string]struct{}) {
	if len(group) == 0 {
		return
	}
	counts := countByStatus(group)
	ready, blockingShape := surfaceReadinessWithSkip(sf, group, skip)
	marker := surfaceMarker(sf, ready)

	fmt.Printf("%s%s %-25s %d/%d %s",
		marker, bold(sf), surfaceLabel(sf),
		counts["✅"]+counts["⚠"]+counts["🟡"]+counts["⏳"],
		len(group),
		compactCounts(counts))
	if !ready && blockingShape != "" {
		fmt.Printf("  %s", red("← "+blockingShape))
	}
	fmt.Println()
}

func surfaceMarker(sf string, ready bool) string {
	switch {
	case sf == "L7":
		return "  "
	case sf == "L6":
		return yellow("ⓘ ")
	case ready:
		return green("✓ ")
	default:
		return red("✗ ")
	}
}

func groupBySurface(rows []Row) map[string][]Row {
	out := map[string][]Row{}
	for _, r := range rows {
		out[r.Surface] = append(out[r.Surface], r)
	}
	return out
}

func collectBlockersWithSkip(rows []Row, skip map[string]struct{}) []Row {
	var blockers []Row
	for _, r := range rows {
		if _, skipped := skip[r.ID]; skipped {
			continue
		}
		switch r.Surface {
		case "L1", "L2", "L3":
			if !readyEngineering(r.Status) {
				blockers = append(blockers, r)
			}
		case "L4", "L5":
			if !readyOpsValidation(r.Status) {
				blockers = append(blockers, r)
			}
		}
	}
	return blockers
}

func printRows(header string, rows []Row) {
	fmt.Println(header)
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, r := range sorted {
		fmt.Printf("  %s %s  %s\n", r.Status, r.ID, r.OneLine)
	}
	fmt.Println()
}

func printVerdict(rows []Row, skip map[string]struct{}) {
	if engineeringReadyWithSkip(rows, skip) {
		if len(skip) > 0 {
			fmt.Println(green(bold("✓ Engineering surface ready (subset gate) — pending operator cutover.")))
		} else {
			fmt.Println(green(bold("✓ Engineering surface ready — pending operator cutover.")))
		}
		return
	}
	fmt.Println(red(bold("✗ Engineering surface NOT ready — see blocking rows above.")))
}

// ─── ANSI helpers ──────────────────────────────────────────────
func bold(s string) string   { return "\033[1m" + s + "\033[0m" }
func green(s string) string  { return "\033[32m" + s + "\033[0m" }
func red(s string) string    { return "\033[31m" + s + "\033[0m" }
func yellow(s string) string { return "\033[33m" + s + "\033[0m" }

func countByStatus(rows []Row) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.Status]++
	}
	return out
}

func compactCounts(c map[string]int) string {
	parts := []string{}
	for _, e := range []string{"✅", "⚠", "🟡", "🟢", "🔴", "⏳"} {
		if n := c[e]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", e, n))
		}
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// surfaceReadiness reports whether all rows in this surface meet
// their tier's readiness bar, plus a short reason if not.
func surfaceReadiness(surface string, rows []Row) (bool, string) {
	return surfaceReadinessWithSkip(surface, rows, nil)
}

// surfaceReadinessWithSkip is the [surfaceReadiness] variant that
// honours `-skip-ids`: skipped rows do not contribute to the
// surface's readiness verdict.
func surfaceReadinessWithSkip(surface string, rows []Row, skip map[string]struct{}) (bool, string) {
	switch surface {
	case "L1", "L2", "L3":
		for _, r := range rows {
			if _, skipped := skip[r.ID]; skipped {
				continue
			}
			if !readyEngineering(r.Status) {
				return false, fmt.Sprintf("%s is %s (must be ✅ or ⚠)", r.ID, r.Status)
			}
		}
	case "L4", "L5":
		for _, r := range rows {
			if _, skipped := skip[r.ID]; skipped {
				continue
			}
			if !readyOpsValidation(r.Status) {
				return false, fmt.Sprintf("%s is %s (must be ✅, ⚠, or 🟡)", r.ID, r.Status)
			}
		}
	}
	return true, ""
}
