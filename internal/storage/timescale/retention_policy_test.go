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

// A retention policy is the one migration shape that DELETES data on a
// schedule, forever, without anybody typing a command — and migrations
// auto-deploy. This file is the tree's ledger of which relations carry
// one, so widening the set is a red build rather than a quiet drop.
//
// The history it guards is specific. Migration 0002 gave `prices_1m`
// and `prices_15m` a 30-day retention and 0001 gave raw `trades` 90
// days; 0031 removed all three on 2026-05-14 and its `down` is a
// deliberate no-op, because re-arming retention on `trades` is the
// recurring data-loss drift the repo names by that number. 0156 adds
// exactly ONE policy back — `prices_1m`, 90 days — and the argument
// for it rests entirely on `trades` still being permanent, so the
// invariants that must hold together are:
//
//   - `trades` has NO retention policy. If that ever changes, the
//     dropped minute buckets stop being recomputable and 0156's whole
//     justification is void.
//   - `prices_1m` is the ONLY price aggregate with one. Every coarser
//     rung is small (27 GB at 15m down to 341 MB at 1mo against 69 GB
//     at 1m, r1 2026-09-07) and is what a long-window request is
//     actually served from.
//
// The ledger is TEXTUAL — it replays `add_retention_policy` /
// `remove_retention_policy` over the up-migrations in numeric order.
// That is an approximation in one direction only, and a safe one:
// dropping a continuous aggregate also drops its policies (0115 / 0147
// recreate all seven price views), so the real database can hold FEWER
// policies than this ledger counts, never more. Corroborated against
// r1's live `timescaledb_information.jobs` on 2026-09-07, which held
// exactly one — `api_usage_events` — before 0156.
//
// That one-way claim covers migrations only. A Go pruner deletes on a
// schedule without any policy, so those are ledgered separately by
// TestGoAgePruners_AreExactlyTheDeclaredSet at the end of this file.

var (
	// addRetentionCallRe / removeRetentionCallRe match one call to
	// each function and capture its whole argument list, so the
	// relation is pulled from the ARGUMENTS rather than assumed to be
	// the first quoted string after the paren — 0156 already mixes a
	// positional relation with named `drop_after` / `if_not_exists`,
	// and a future call may name `relation =>` too.
	// Comment lines are dropped first ([stripSQLComments], shared with
	// the compression ledger) so the prose in 0034 / 0115 / 0116 —
	// which all discuss `add_retention_policy` without calling it —
	// never registers as a call.
	addRetentionCallRe    = regexp.MustCompile(`add_retention_policy\s*\(([^)]*)\)`)
	removeRetentionCallRe = regexp.MustCompile(`remove_retention_policy\s*\(([^)]*)\)`)
	// namedRelationArgRe / positionalRelationArgRe pull the relation
	// out of one call's argument list: named `relation => '<name>'`
	// first (order-independent within the arg list), else the first
	// positional quoted argument. The class includes `.` so a
	// schema-qualified relation (`'public.trades'`) is captured whole;
	// [normalizeRelationName] strips the schema before it is used as
	// a ledger key.
	namedRelationArgRe      = regexp.MustCompile(`relation\s*=>\s*'([a-zA-Z0-9_.]+)'`)
	positionalRelationArgRe = regexp.MustCompile(`^\s*'([a-zA-Z0-9_.]+)'`)
	// A leading `--` marker, removed so a command written inside a
	// header comment reads as the command it is.
	commentMarkerRe = regexp.MustCompile(`(?m)^\s*--\s?`)
	// The start of a refresh CALL: the name followed by a quoted
	// relation. Deliberately not a bare name match — the header also
	// cites the function's SQL signature in prose, which is not a
	// command and must not be held to the command's rules.
	refreshCallStartRe = regexp.MustCompile(`refresh_continuous_aggregate\s*\(\s*'`)
)

// relationFromRetentionArgs extracts the target relation from one
// add_retention_policy / remove_retention_policy call's argument
// list, however it is spelled. ok=false means neither the named nor
// the positional shape matched — callers must fail loudly on that,
// never silently skip the call, or a call the ledger cannot parse
// reads as a call that was never made.
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

// retentionRelationsFromCalls finds every call `callRe` matches in
// `sql` and returns the relation each one names. A matched call whose
// relation cannot be extracted fails the test naming `file` — a
// spelling the extractor cannot parse must not silently vanish from
// the ledger, which is exactly how a `relation => 'trades'` policy
// would have gone unnoticed before this function existed.
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

// The set of relations carrying a retention policy is exactly this,
// and every entry is named on purpose. Adding a relation to any
// migration fails here until it is added to this list with a reason,
// which is the whole point: a policy that arrives silently is one that
// deletes data nobody decided to delete.
func TestRetentionPolicies_AreExactlyTheDeclaredSet(t *testing.T) {
	want := map[string]string{
		// Request telemetry, 12 months (0027). Never a reconcile
		// target and never served as history.
		"api_usage_events": "0027_platform_v1_schema.up.sql",
		// The minute price aggregate, 90 days (0156). 69 GB / 82 M
		// rows on r1 2026-09-07, 55 % of all price-CAGG storage, and
		// recomputable from `trades` — which 0156 depends on and the
		// next test pins. 0166 rebuilds prices_1m and re-attaches the
		// same policy (TestPrices1mRetention_0166ReattachesTheSamePolicy).
		"prices_1m": "0166_twap_notional_floor.up.sql",
		// Per-account usage rollups, 12 months (0167) — the same
		// horizon as api_usage_events. Served for 30 days only.
		"usage_daily": "0167_usage_daily_retention.up.sql",
		// Per-tick source-contribution breakdowns, 90 days, shipped
		// disabled (0169). Appended once per aggregator tick per
		// (pair, window, source); only the latest bucket is ever read.
		"price_source_contributions": "0169_price_source_contributions_window.up.sql",
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

// 0156's argument is that a dropped minute bucket is recomputable
// because raw `trades` is kept forever. If `trades` ever regains a
// retention policy that argument is void and 0156 must be removed —
// this pins the pairing so the two can never drift apart silently.
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
	// The policy must ship DISABLED, and the migration must ASSERT that
	// rather than assume it. A deferred first run is not a substitute:
	// a fuse only helps if somebody is watching, and the disable is
	// the thing that makes arming a deliberate act.
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

// 0166 drops and recreates prices_1m, which drops 0156's policy with it.
// Both directions of 0166 must put back the SAME policy: one relation,
// 90 days, shipped disarmed and asserted so.
func TestPrices1mRetention_0166ReattachesTheSamePolicy(t *testing.T) {
	for _, path := range []string{
		"migrations/0166_twap_notional_floor.up.sql",
		"migrations/0166_twap_notional_floor.down.sql",
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

// Recovery of a dropped range needs `force => true`: a retention drop
// writes no materialization-invalidation entry, so a plain
// refresh_continuous_aggregate over that window collects nothing,
// reports the view "already up-to-date" and writes zero rows
// (TimescaleDB 2.26.4, tsl/src/continuous_aggs/refresh.c ->
// invalidation.c collect_and_delete_cagg_invalidations_in_window).
// An operator who copies the plain form out of this migration would
// conclude the history was unrecoverable, so both files have to carry
// the forced form.
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
			// A call the header marks DO NOT RUN is a warning, not a
			// recipe — it is quoted precisely so an operator recognises
			// the destructive form, and adding `force` to it would make
			// the warning name something nobody would ever type.
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

// doNotRunMarker flags a command quoted as a WARNING rather than as a
// recipe. It sits in the migration text so an operator scanning the
// header sees it before the command, and the guards above key on it so
// a forbidden form is never held to a recipe's rules — nor counted as
// one.
const doNotRunMarker = "DO NOT RUN"

// refreshCallsIn returns each `refresh_continuous_aggregate(...)` call
// in `sql`, from the function name to the closing `)` of its argument
// list. Asserting per CALL rather than per FILE is load-bearing: the
// file also has prose ABOUT `force => true`, and a file-wide substring
// check goes green on a recovery command that omits it while the
// paragraph beside it explains why it must not be omitted.
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

// Every command that selects the retention job must match on the CAGG's
// USER VIEW name, `prices_1m`.
//
// This is the test whose absence shipped a disarm command that matched
// ZERO rows. `timescaledb_information.jobs` renders its
// `hypertable_name` column as
// `COALESCE(ca.user_view_name, ht.table_name)`, joined on
// `ca.mat_hypertable_id = j.hypertable_id`, and the retention API
// stores the MATERIALIZATION hypertable id — so for a continuous
// aggregate the view reports `prices_1m` and NEVER
// `_materialized_hypertable_142`. A predicate written against
// `materialization_hypertable_name` therefore selects nothing.
//
// What makes that catastrophic rather than merely wrong is that
// `alter_job` over an empty row set is NOT an error: it prints nothing
// and exits 0, so a disarm that matched nothing reads exactly like a
// disarm that worked — and the policy drops 77 chunks the next day.
// Verified on r1 2026-09-07 against the structurally identical refresh
// job: the materialization-name predicate returns 0 rows, the view-name
// predicate returns job 1088.
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

// Every command that touches the job must be followed by a SELECT that
// shows the outcome, because a zero-row `alter_job` is silent success.
// The up-migration additionally has to ASSERT it (RAISE EXCEPTION),
// since nobody reads a migration's output on an auto-deploy.
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

// A drop invalidates the TWAP views, and the repo's own documented TWAP
// refresh then deletes their history.
//
// `_materialized_hypertable_142` is prices_1m's materialization
// hypertable AND the raw hypertable of both TWAP views (measured on r1
// 2026-09-07: twap_1h and twap_1d each carry raw_hypertable_id = 142
// and parent_mat_hypertable_id = 142, and an invalidation-threshold row
// exists for 142). `ts_chunk_do_drop_chunks` invalidates the raw
// hypertable's dependent aggregates for every chunk it drops, so one
// armed run writes an invalidation entry per dropped chunk against both
// TWAP views. Those entries are dormant under the daily policies — and
// NOT dormant under `refresh_continuous_aggregate('twap_1h', NULL,
// now())`, which migrations 0081, 0126 and 0147 all document and which
// would process them against an emptied prices_1m.
//
// The migration has to point the operator at the windowed form. This
// asserts it says so, because "re-materialise twap afterwards" — which
// is what it used to say — points at the destructive one.
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

// The up-migration must not claim the 0115 / 0147 distinction is that
// `trades` is permanent. Migration 0031 removed the retention on
// `trades` on 2026-05-14; 0115 landed 2026-07-24 and 0147 2026-08-22,
// so it was already permanent for both and the distinction does not
// exist. The real one is that those were one-off drops and this policy
// drops daily forever — which is why every recovery here starts with a
// disarm.
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

// The ledger must see a retention policy on `trades` however the call
// spells the relation — named argument, schema-qualified, or both —
// because [TestRetentionPolicies_TradesStaysPermanentWhilePrices1mIsBounded]
// only catches a re-armed `trades` policy if the ledger extracted it
// in the first place.
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

// migrations/README.md rule 4 is the instruction a new CAGG author
// reads. It must defer to the declared set above rather than restate a
// retention posture of its own: the rule once mandated a retention
// policy on every new CAGG, which is the 0031 data-loss drift.
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

// ageDeleteRe / ageCutoffRe find a Go-side pruner: one SQL string literal
// that deletes from a relation AND compares a timestamp column against a
// cutoff. A Go pruner deletes data on a schedule exactly as
// add_retention_policy does, so it is ledgered the same way.
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

// Every relation a Go pruner deletes from by age is named here with the
// reason it is safe, as a migration-side policy must be. mev_events is
// deliberately absent: it is served history the detectors cannot re-derive
// (they scan a 30-minute trailing window), and launch-plan row D5 contracts
// that the served tier keeps what it indexes (#1168).
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
