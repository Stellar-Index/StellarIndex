package clickhouse

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

var opsBySourceFeederRe = regexp.MustCompile(`(?is)CREATE\s+MATERIALIZED\s+VIEW\s+IF\s+NOT\s+EXISTS\s+stellar\.(\w+)\s+TO\s+stellar\.ops_by_source\s+AS\s+SELECT\s+(\w+).*?FROM\s+stellar\.(\w+)`)

// TestOpsBySourceProofCoversEveryFeedingView: the proof certifies only the streams it counts, so every view the DDL
// points at stellar.ops_by_source must be in the views check, and every tx-sentinel view's account column in the
// proof's tx keys.
func TestOpsBySourceProofCoversEveryFeedingView(t *testing.T) {
	root := lockstepRepoRoot(t)
	accounts := strings.Split(opsProofTxAccounts, ", ")
	for _, rel := range []string{"deploy/clickhouse/ops_by_source.sql", "deploy/clickhouse/tier1_schema.sql"} {
		var views []string
		for _, m := range opsBySourceFeederRe.FindAllStringSubmatch(lockstepReadFile(t, root, rel), -1) {
			views = append(views, m[1])
			switch m[3] {
			case "operations":
			case "transactions":
				if !slices.Contains(accounts, m[2]) {
					t.Errorf("%s: %s writes transactions.%s as a tx-sentinel row, but the proof's tx keys count only %q", rel, m[1], m[2], opsProofTxAccounts)
				}
			default:
				t.Errorf("%s: %s feeds ops_by_source from stellar.%s, which the proof does not count", rel, m[1], m[3])
			}
		}
		slices.Sort(views)
		want := slices.Sorted(slices.Values(opsBySourceViews))
		if !slices.Equal(views, want) {
			t.Errorf("%s: views feeding ops_by_source = %v, OpsBySourceMVsExist checks %v", rel, views, want)
		}
	}
	if !strings.Contains(opsProofTxQuery, "["+opsProofTxAccounts+"]") {
		t.Errorf("opsProofTxQuery does not key on %s", opsProofTxAccounts)
	}
}
