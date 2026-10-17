package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file ledgers which relations carry a retention policy, so widening the set is a red build, not a quiet
// drop. `trades` must have none, and `prices_1m` is the only price aggregate with one. The textual ledger can
// overcount, never undercount; Go pruners are ledgered by TestGoAgePruners_AreExactlyTheDeclaredSet.

var (
	// addRetentionCallRe / removeRetentionCallRe match one call each and capture the whole argument list, so the
	// relation is taken from the ARGUMENTS (0156 mixes positional and named args, and a call may use `relation
	// =>`). Comment lines are dropped first ([stripSQLComments]) so prose mentioning a call never registers.
	addRetentionCallRe    = regexp.MustCompile(`add_retention_policy\s*\(([^)]*)\)`)
	removeRetentionCallRe = regexp.MustCompile(`remove_retention_policy\s*\(([^)]*)\)`)
	// namedRelationArgRe / positionalRelationArgRe extract the relation from a call's args: the named
	// `relation => '<name>'` form first, else the first quoted positional. The class includes `.` so a
	// schema-qualified name is captured whole for normalizeRelationName.
	namedRelationArgRe      = regexp.MustCompile(`relation\s*=>\s*'([a-zA-Z0-9_.]+)'`)
	positionalRelationArgRe = regexp.MustCompile(`^\s*'([a-zA-Z0-9_.]+)'`)
	// A leading `--` marker, removed so a command written inside a
	// header comment reads as the command it is.
	commentMarkerRe = regexp.MustCompile(`(?m)^\s*--\s?`)
	// The start of a refresh CALL: the name followed by a quoted relation. Not a bare name match, because the
	// header also cites the function's SQL signature in prose, which is not a command.
	refreshCallStartRe = regexp.MustCompile(`refresh_continuous_aggregate\s*\(\s*'`)
)

// relationFromRetentionArgs extracts the target relation from one add_retention_policy /
// remove_retention_policy argument list, however spelled. ok=false means no shape matched; callers must fail
// loudly, as an unparsed call reads as never made.
func relationFromRetentionArgs(args string) (string, bool) {
	if m := namedRelationArgRe.FindStringSubmatch(args); m != nil {
		return normalizeRelationName(m[1]), true
	}
	if m := positionalRelationArgRe.FindStringSubmatch(args); m != nil {
		return normalizeRelationName(m[1]), true
	}
	return "", false
}

// normalizeRelationName drops a schema qualifier (`'public.trades'`
// → `trades`) so a schema-qualified call registers under the same
// key the declared set and the pinning tests use.
func normalizeRelationName(raw string) string {
	if i := strings.LastIndexByte(raw, '.'); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

// retentionRelationsFromCalls returns the relation named by each call callRe matches in sql. A matched call
// whose relation cannot be extracted fails the test naming file, so an unparseable spelling cannot vanish
// from the ledger.
func retentionRelationsFromCalls(t *testing.T, callRe *regexp.Regexp, sql, file string) []string {
	t.Helper()
	var relations []string
	for _, call := range callRe.FindAllStringSubmatch(sql, -1) {
		rel, ok := relationFromRetentionArgs(call[1])
		if !ok {
			t.Fatalf("%s: retention call %q — could not extract its relation "+
				"(named `relation =>` or positional first argument)", file, call[0])
		}
		relations = append(relations, rel)
	}
	return relations
}

// retentionLedger replays every up-migration in numeric order and
// returns the relations left holding a retention policy.
func retentionLedger(t *testing.T) map[string]string {
	t.Helper()
	root := findRepoRoot(t)
	ups, err := filepath.Glob(filepath.Join(root, "migrations", "[0-9]*_*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no up-migrations found — this ledger has gone vacuous. Fix the glob, do not delete the test.")
	}
	sort.Strings(ups) // zero-padded four-digit prefixes sort numerically

	held := map[string]string{} // relation → migration that added it
	sawAny := false
	for _, path := range ups {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sql := stripSQLComments(string(b))
		name := filepath.Base(path)
		for _, rel := range retentionRelationsFromCalls(t, removeRetentionCallRe, sql, name) {
			delete(held, rel)
			sawAny = true
		}
		for _, rel := range retentionRelationsFromCalls(t, addRetentionCallRe, sql, name) {
			held[rel] = name
			sawAny = true
		}
	}
	if !sawAny {
		t.Fatal("no retention statements matched in any migration — the pattern no longer " +
			"matches this tree and the ledger has gone vacuous. Fix the regex, do not delete the test.")
	}
	return held
}

// The set of relations carrying a retention policy is exactly this, and every entry is named
// on purpose. A relation added in any migration fails here until it is listed with a reason:
// a policy that arrives silently deletes data nobody decided to delete.
func TestRetentionPolicies_AreExactlyTheDeclaredSet(t *testing.T) {
	want := map[string]string{
		// Request telemetry, 12 months (0027). Never a reconcile
		// target and never served as history.
		"api_usage_events": "0027_platform_v1_schema.up.sql",
		// The minute price aggregate, 90 days (0156): the largest price CAGG and recomputable
		// from `trades`, which the next test pins. 0166 and 0187 rebuild prices_1m and
		// re-attach the same policy (TestPrices1mRetention_RebuildsReattachTheSamePolicy).
		"prices_1m": "0187_price_caggs_priceable_filter.up.sql",
		// Per-account usage rollups, 12 months (0167) — the same
		// horizon as api_usage_events. Served for 30 days only.
		"usage_daily": "0167_usage_daily_retention.up.sql",
		// Per-tick source-contribution breakdowns, 90 days, shipped
		// disabled (0169). Appended once per aggregator tick per
		// (pair, window, source); only the latest bucket is ever read.
		"price_source_contributions": "0169_price_source_contributions_window.up.sql",
		// The aggregator's per-(source, pair) minute sums, 7 days, shipped disabled (0217).
		// Read only for windows of at most 25 hours and recomputable from `trades`.
		"trade_sums_1m": "0217_trade_sums_1m.up.sql",
	}

	held := retentionLedger(t)
	for rel, mig := range want {
		got, ok := held[rel]
		if !ok {
			t.Errorf("%s should carry a retention policy (from %s) and does not", rel, mig)
			continue
		}
		if got != mig {
			t.Errorf("%s's retention policy comes from %s, want %s", rel, got, mig)
		}
	}
	for rel, mig := range held {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s gained a retention policy in %s and is NOT in the declared set. "+
				"A retention policy deletes data on a schedule, forever: add it here with the "+
				"reason it is safe to drop, or remove the policy.", rel, mig)
		}
	}
}

// Migration 0156 argues a dropped minute bucket is recomputable because raw `trades` is kept forever. If
// `trades` regains a retention policy that argument is void and 0156 must go; this pins the pairing.
func TestRetentionPolicies_TradesStaysPermanentWhilePrices1mIsBounded(t *testing.T) {
	held := retentionLedger(t)
	if mig, ok := held["trades"]; ok {
		t.Errorf("raw `trades` carries a retention policy from %s. Migration 0156 bounds "+
			"prices_1m ONLY because `trades` is permanent and every dropped minute bucket is "+
			"recomputable from it (forced refresh_continuous_aggregate). Re-arming retention on "+
			"`trades` makes that false — remove 0156 first, or do not add this.", mig)
	}
	if _, ok := held["prices_1m"]; !ok {
		t.Error("prices_1m has no retention policy — 0156 was removed or neutralised; " +
			"this test and its sibling above must be updated together with it.")
	}
}

// One aggregate, not a family. Every rung a long-window request is
// actually served from — and every TWAP view built on top of the
// minute rung — keeps its full history.
func TestRetentionPolicies_OnlyTheMinuteAggregateIsBounded(t *testing.T) {
	held := retentionLedger(t)
	for _, view := range []string{
		"prices_15m", "prices_1h", "prices_4h", "prices_1d", "prices_1w", "prices_1mo",
		"twap_1h", "twap_1d",
		"oracle_prices_1m", "oracle_prices_15m", "oracle_prices_1h", "oracle_prices_4h",
		"oracle_prices_1d", "oracle_prices_1w", "oracle_prices_1mo",
		"supply_1d", "dex_volume_by_pair_1d", "source_volume_1h", "pools_per_source_1h",
	} {
		if mig, ok := held[view]; ok {
			t.Errorf("%s gained a retention policy in %s — 0156 bounds prices_1m ALONE. "+
				"Every other rung is what a long-window request is served from, and the two "+
				"TWAP views are materialised FROM prices_1m, so bounding them compounds the loss.", view, mig)
		}
	}
}

// The horizon and the relation are read off the migration itself, so a
// later edit that widens `drop_after` or renames the relation has to
// come through this assertion.
func TestPrices1mRetention_HorizonIsNinetyDaysAndNamesOneRelation(t *testing.T) {
	sql := stripSQLComments(readRepoFile(t, "migrations/0156_prices_1m_retention.up.sql"))

	added := retentionRelationsFromCalls(t, addRetentionCallRe, sql, "0156_prices_1m_retention.up.sql")
	if len(added) != 1 {
		t.Fatalf("0156 issues %d add_retention_policy calls, want exactly 1", len(added))
	}
	if added[0] != "prices_1m" {
		t.Errorf("0156 attaches retention to %q, want prices_1m", added[0])
	}
	if !strings.Contains(sql, "INTERVAL '90 days'") {
		t.Errorf("0156 no longer states `INTERVAL '90 days'` — the horizon moved without this "+
			"assertion moving with it:\n%s", sql)
	}
	// The policy must ship DISABLED and the migration must assert that rather than assume it: a deferred first
	// run is no substitute, and the disable is what makes arming a deliberate act.
	if !strings.Contains(sql, "scheduled => false") {
		t.Error("0156 no longer disables the policy it creates — an auto-deployed migration " +
			"would arm a destructive daily job with no operator decision")
	}
	if !strings.Contains(sql, "RAISE EXCEPTION") {
		t.Error("0156 disables the policy but does not RAISE when the disable fails to take. " +
			"`alter_job` over zero rows exits 0 in silence, so an unasserted disable is " +
			"indistinguishable from shipping the policy armed.")
	}
	// A destructive migration's down must detach the policy, or a
	// rollback leaves the drops running.
	removed := retentionRelationsFromCalls(t, removeRetentionCallRe,
		stripSQLComments(readRepoFile(t, "migrations/0156_prices_1m_retention.down.sql")),
		"0156_prices_1m_retention.down.sql")
	if len(removed) != 1 || removed[0] != "prices_1m" {
		t.Errorf("0156 down removes %v, want exactly one removal naming prices_1m", removed)
	}
}

// 0166 and 0187 drop and recreate prices_1m, which drops 0156's policy
// with it. Both directions of each must put back the SAME policy: one
// relation, 90 days, shipped disarmed and asserted so.
func TestPrices1mRetention_RebuildsReattachTheSamePolicy(t *testing.T) {
	for _, path := range []string{
		"migrations/0166_twap_notional_floor.up.sql",
		"migrations/0166_twap_notional_floor.down.sql",
		"migrations/0187_price_caggs_priceable_filter.up.sql",
		"migrations/0187_price_caggs_priceable_filter.down.sql",
	} {
		sql := stripSQLComments(readRepoFile(t, path))
		added := retentionRelationsFromCalls(t, addRetentionCallRe, sql, path)
		if len(added) != 1 || added[0] != "prices_1m" {
			t.Errorf("%s re-attaches %v, want exactly one policy naming prices_1m", path, added)
		}
		for _, want := range []string{"INTERVAL '90 days'", "scheduled => false", "RAISE EXCEPTION"} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s no longer contains %q — the rebuilt prices_1m must carry 0156's "+
					"policy unchanged: same horizon, disarmed, and the disarm asserted", path, want)
			}
		}
	}
}

// Recovering a dropped range needs `force => true`: a retention drop writes no invalidation entry, so a
// plain refresh_continuous_aggregate collects nothing and reports "already up-to-date". An operator copying
// the plain form would conclude history was unrecoverable, so both files must carry the forced form.
func TestPrices1mRetention_DocumentsTheForcedRefreshRecovery(t *testing.T) {
	for _, name := range []string{
		"0156_prices_1m_retention.up.sql",
		"0156_prices_1m_retention.down.sql",
	} {
		// The recovery command lives INSIDE the header comments — it is
		// an operator step, not something a migration executes — so the
		// `--` prefixes come off before the calls are read out.
		text := commentMarkerRe.ReplaceAllString(readRepoFile(t, "migrations/"+name), "")
		calls := refreshCallsIn(text)
		if len(calls) == 0 {
			t.Fatalf("%s shows no refresh_continuous_aggregate recovery call at all", name)
		}
		recipes := 0
		for _, call := range calls {
			// A call the header marks DO NOT RUN is a warning, not a recipe: it is quoted so an operator recognises the
			// destructive form, and adding `force` would make the warning name something nobody would type.
			if strings.Contains(call, doNotRunMarker) {
				continue
			}
			recipes++
			if strings.Contains(call, "force => true") {
				continue
			}
			t.Errorf("%s shows a recovery call without `force => true`:\n\t%s\n"+
				"Without it the call finds no invalidation entries for a retention-dropped "+
				"window, reports the view already up-to-date and writes nothing — so the "+
				"documented recovery silently does not recover.",
				name, strings.Join(strings.Fields(call), " "))
		}
		if recipes == 0 {
			t.Errorf("%s quotes only DO-NOT-RUN forms and no recovery recipe at all", name)
		}
	}
}

// doNotRunMarker flags a command quoted as a WARNING rather than a recipe. It sits in the
// migration text so an operator sees it before the command, and the guards above key on it so
// a forbidden form is neither held to a recipe's rules nor counted as one.
const doNotRunMarker = "DO NOT RUN"

// refreshCallsIn returns each `refresh_continuous_aggregate(...)` call in `sql`, from name to closing `)`.
// Asserting per call, not per file, is load-bearing: the file's prose about `force => true` would pass a
// file-wide substring check on a recovery command that omits it.
func refreshCallsIn(sql string) []string {
	var out []string
	// A CALL is the name followed by a quoted relation; a prose
	// reference to the function's signature is not one.
	for _, loc := range refreshCallStartRe.FindAllStringIndex(sql, -1) {
		// Start from the beginning of the call's own line so a
		// [doNotRunMarker] preceding it on that line is part of the
		// captured text.
		from := strings.LastIndex(sql[:loc[0]], "\n") + 1
		rest := sql[from:]
		end := strings.Index(rest, ");")
		if end < 0 {
			end = len(rest) - 1
		}
		out = append(out, rest[:end+1])
	}
	return out
}

// jobsViewPredicateRe finds every `hypertable_name = <x>` predicate in
// the migration pair — the arm command, the disarm command, the
// migration's own disable step and its assertion all share one.
var jobsViewPredicateRe = regexp.MustCompile(`hypertable_name\s*=\s*([^\n]+)`)

// Every command selecting the retention job must match the CAGG's USER VIEW name `prices_1m`, since the jobs
// view never reports the materialization name. A wrong predicate makes `alter_job` match nothing and exit 0
// silently.
func TestPrices1mRetention_JobPredicatesMatchTheViewName(t *testing.T) {
	for _, name := range []string{
		"0156_prices_1m_retention.up.sql",
		"0156_prices_1m_retention.down.sql",
	} {
		text := readRepoFile(t, "migrations/"+name)
		if strings.Contains(text, "materialization_hypertable_name") {
			t.Errorf("%s selects a job by materialization_hypertable_name. "+
				"timescaledb_information.jobs reports the CAGG's USER VIEW name there, so that "+
				"predicate matches zero rows — and alter_job over zero rows exits 0 in silence.",
				name)
		}
		for _, m := range jobsViewPredicateRe.FindAllStringSubmatch(text, -1) {
			pred := strings.TrimSpace(m[1])
			if strings.HasPrefix(pred, "'prices_1m'") {
				continue
			}
			t.Errorf("%s has a jobs-view predicate `hypertable_name = %s`; it must be "+
				"`hypertable_name = 'prices_1m'` — the user view name is what that column holds "+
				"for a continuous aggregate.", name, pred)
		}
	}
}

// Every command touching the job must be followed by a SELECT showing the outcome, since a zero-row
// `alter_job` is silent success; the up-migration must also ASSERT it (RAISE EXCEPTION), as nobody reads a
// migration's output on auto-deploy.
func TestPrices1mRetention_ArmAndDisarmCarryAVerificationSelect(t *testing.T) {
	text := readRepoFile(t, "migrations/0156_prices_1m_retention.up.sql")
	// The header states the arm and the disarm; each is followed by a
	// SELECT of (job_id, scheduled, next_start) over the same predicate.
	verifications := strings.Count(text, "SELECT job_id, scheduled, next_start")
	if verifications < 2 {
		t.Errorf("0156's header shows %d verification SELECT(s); the arm command and the "+
			"disarm command each need one, or a zero-row alter_job reads as success",
			verifications)
	}
	for _, want := range []string{"scheduled => true", "scheduled => false"} {
		if !strings.Contains(text, want) {
			t.Errorf("0156's header does not state the `%s` command", want)
		}
	}
}

// A retention drop invalidates the TWAP views, and a plain `refresh_continuous_aggregate('twap_1h', NULL,
// now())` would then process those entries against an emptied prices_1m and delete history. The migration
// must say to use the windowed form.
func TestPrices1mRetention_WarnsThatATWAPRefreshMustBeWindowed(t *testing.T) {
	for _, name := range []string{
		"0156_prices_1m_retention.up.sql",
		"0156_prices_1m_retention.down.sql",
	} {
		text := readRepoFile(t, "migrations/"+name)
		if !strings.Contains(text, "twap_1h") {
			t.Errorf("%s does not mention twap_1h at all", name)
			continue
		}
		if !strings.Contains(text, "NULL") {
			t.Errorf("%s does not warn against the NULL-start TWAP refresh form that "+
				"migrations 0081 / 0126 / 0147 document", name)
		}
		if !strings.Contains(text, "WINDOWED") && !strings.Contains(text, "windowed") {
			t.Errorf("%s does not tell the operator the TWAP refresh must be WINDOWED", name)
		}
	}
}

// The up-migration must not claim the 0115 / 0147 distinction is that `trades` is permanent: it was permanent
// for both. The real distinction is that those were one-off drops while this policy drops daily forever, so
// every recovery starts with a disarm.
func TestPrices1mRetention_DoesNotClaimTheFalse0115Distinction(t *testing.T) {
	text := readRepoFile(t, "migrations/0156_prices_1m_retention.up.sql")
	for _, banned := range []string{
		"ONLY thing that makes this case different",
		"only thing that makes this case different",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("0156 still carries the false 0115/0147 distinction (%q). `trades` was "+
				"already permanent when both of those landed.", banned)
		}
	}
	if !strings.Contains(text, "ONE-OFF") {
		t.Error("0156 does not state the real difference from 0115 / 0147: those were one-off " +
			"drops over a static hole, while this policy drops daily forever")
	}
}

// The ledger must see a retention policy on `trades` however the call spells the relation (named, schema-
// qualified, or both), or [TestRetentionPolicies_TradesStaysPermanentWhilePrices1mIsBounded] cannot catch a
// re-armed `trades` policy.
func TestRelationFromRetentionArgs_CatchesEveryTradesSpelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"positional", `'trades'`},
		{"named", `relation => 'trades', drop_after => INTERVAL '180 days'`},
		{"schema-qualified positional", `'public.trades'`},
		{"schema-qualified named", `relation => 'public.trades', drop_after => INTERVAL '180 days'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel, ok := relationFromRetentionArgs(tc.args)
			if !ok {
				t.Fatalf("relationFromRetentionArgs(%q) found no relation", tc.args)
			}
			if rel != "trades" {
				t.Errorf("relationFromRetentionArgs(%q) = %q, want %q", tc.args, rel, "trades")
			}
		})
	}
}

// migrations/README.md rule 4 is what a new CAGG author reads. It must defer to the declared set above
// rather than restate a retention posture, since it once mandated a policy on every new CAGG, the 0031
// data-loss drift.
func TestRetentionPolicies_ReadmeRule4DefersToTheDeclaredSet(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(findRepoRoot(t), "migrations", "README.md"))
	if err != nil {
		t.Fatalf("read migrations/README.md: %v", err)
	}
	raw := string(b)
	start := strings.Index(raw, "4. **Every migration that creates a continuous aggregate**")
	if start == -1 {
		t.Fatal("migrations/README.md rule 4 not found — update this test's anchor, do not delete it")
	}
	end := strings.Index(raw[start:], "5. **Amounts")
	if end == -1 {
		t.Fatal("migrations/README.md rule 5 (Amounts) not found — update this test's anchor")
	}
	rule := strings.Join(strings.Fields(raw[start:start+end]), " ")
	for _, want := range []string{
		"does NOT add a retention policy",
		"TestRetentionPolicies_AreExactlyTheDeclaredSet",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("migrations/README.md rule 4 does not say %q. It must forbid a retention "+
				"policy by default and point at the declared set, not restate a posture:\n%s", want, rule)
		}
	}
	if strings.Contains(rule, "refresh policy + retention policy") {
		t.Error("migrations/README.md rule 4 still mandates a retention policy on every new CAGG")
	}
}

// ageDeleteRe / ageCutoffRe find a Go-side pruner: one SQL string literal that deletes from a relation AND
// compares a timestamp column to a cutoff. It deletes on a schedule like add_retention_policy, so it is
// ledgered too.
var (
	ageDeleteRe = regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+([a-z_][a-z0-9_]*)`)
	ageCutoffRe = regexp.MustCompile(`(?i)\b[a-z_]*(_at|ts|bucket|last_updated)\s*<\s*(\$[0-9]+|now\(\))`)
)

// goAgePrunedRelations returns relation → first file for every non-test Go
// string literal under internal/ and cmd/ that deletes rows by age.
func goAgePrunedRelations(t *testing.T) map[string]string {
	t.Helper()
	root := findRepoRoot(t)
	found := map[string]string{}
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			scanned++
			return collectAgeDeletes(path, root, found)
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if scanned == 0 {
		t.Fatal("no Go sources scanned — the census has gone vacuous")
	}
	return found
}

func collectAgeDeletes(path, root string, found map[string]string) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return err
	}
	rel, _ := filepath.Rel(root, path)
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !ageCutoffRe.MatchString(lit.Value) {
			return true
		}
		for _, m := range ageDeleteRe.FindAllStringSubmatch(lit.Value, -1) {
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = rel
			}
		}
		return true
	})
	return nil
}

// Every relation a Go pruner deletes from by age is named here with why it is safe, as a migration-side
// policy must be. mev_events is deliberately absent: it is served history the detectors cannot re-derive
// (30-minute window).
func TestGoAgePruners_AreExactlyTheDeclaredSet(t *testing.T) {
	const (
		snapshot = "rolling snapshot recomputed every refresh; the delete drops superseded rows"
		resync   = "directory re-synced from its source; the delete drops rows the latest sync did not see"
	)
	want := map[string]string{
		"asset_volume_24h":        snapshot,
		"asset_volume_character":  snapshot,
		"asset_price_snapshot":    snapshot,
		"protocol_events_24h":     snapshot,
		"account_directory":       resync,
		"asset_listing_directory": resync,
		"rwa_curated_directory":   resync,
		"ingestion_cursors":       "cursors of retired sources, never history",
		"sessions":                "expired or revoked auth sessions",
		"magic_link_tokens":       "expired single-use login tokens",
		"login_code_lockouts":     "stale login rate-limit state",
		"accounts":                "lost signup-race orphans with no users or keys",
		"webhook_deliveries":      "finished delivery attempts (internal/retentionreaper)",
	}
	got := goAgePrunedRelations(t)
	for rel, file := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s deletes %s rows by age and %s is NOT in the declared set. A Go pruner "+
				"deletes data on a schedule like a retention policy: add it here with the reason "+
				"it is safe to drop, or remove the delete.", file, rel, rel)
		}
	}
	for rel := range want {
		if _, ok := got[rel]; !ok {
			t.Errorf("%s is declared as Go-pruned but no age-based DELETE for it was found — "+
				"drop it from the declared set", rel)
		}
	}
}
