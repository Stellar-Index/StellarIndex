package timescale

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// createCAGG matches a literal continuous-aggregate CREATE in a migration (comment lines are stripped first).
var createCAGG = regexp.MustCompile(`(?is)CREATE\s+MATERIALIZED\s+VIEW\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z0-9_]+)\s+WITH\s*\(\s*timescaledb\.continuous`)

// retiredCAGGs are views a migration created and a later one dropped for good, each with the migration that dropped it.
var retiredCAGGs = map[string]string{}

// TestEveryMigratedCAGGIsHistoryWatched: a continuous aggregate the migrations create but CAGGHistoryStarts does not
// measure can be emptied by a WITH NO DATA rebuild with nothing noticing.
func TestEveryMigratedCAGGIsHistoryWatched(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	created := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // in-repo migration path
		if err != nil {
			t.Fatal(err)
		}
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				code = append(code, line)
			}
		}
		for _, m := range createCAGG.FindAllStringSubmatch(strings.Join(code, "\n"), -1) {
			if _, seen := created[m[1]]; !seen {
				created[m[1]] = filepath.Base(f)
			}
		}
	}
	watched := map[string]bool{}
	for _, s := range caggHistorySpecs() {
		watched[s.Name] = true
		if _, ok := created[s.Name]; !ok {
			t.Errorf("%s is registered but no migration creates it as a continuous aggregate", s.Name)
		}
	}
	for view, file := range created {
		if _, retired := retiredCAGGs[view]; !retired && !watched[view] {
			t.Errorf("%s (created in %s) is not in TradesCAGGs/OracleCAGGs/SupplyCAGG, so its history is never measured", view, file)
		}
	}
	if len(created) < 21 {
		t.Errorf("found %d continuous aggregates in the migrations, want at least 21; the CREATE pattern drifted", len(created))
	}
}

// TestCAGGHistorySourcesFollowTheMigrations pins each view's source to what its migration selects FROM.
func TestCAGGHistorySourcesFollowTheMigrations(t *testing.T) {
	want := map[string]string{
		"twap_1h": "prices_1m", "twap_1d": "prices_1m", "supply_1d": "asset_supply_history",
		"oracle_prices_1m": "oracle_updates", "oracle_prices_1mo": "oracle_updates",
		"prices_1m": "trades", "pools_per_source_1h": "trades", "trade_sums_1m": "trades",
	}
	specs := caggHistorySpecs()
	for _, s := range specs {
		if w, ok := want[s.Name]; ok && s.source != w {
			t.Errorf("%s source = %s, want %s", s.Name, s.source, w)
		}
	}
	idx := slices.IndexFunc(specs, func(s caggHistorySpec) bool { return s.Name == "prices_1m" })
	for i, s := range specs {
		if s.source == "prices_1m" && i < idx {
			t.Errorf("%s is measured before its parent prices_1m", s.Name)
		}
	}
}

func TestExpectedHistoryStart(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	src := now.AddDate(0, -6, 0)
	spec := func(name, source string, floor time.Duration) caggHistorySpec {
		return caggHistorySpec{CAGGSpec{Name: name, Floor: floor}, source}
	}
	cases := []struct {
		name      string
		spec      caggHistorySpec
		retention map[string]time.Duration
		want      time.Time
	}{
		{"no floor", spec("prices_1h", "trades", 0), nil, src},
		{"documented floor", spec("trade_sums_1m", "trades", 26*time.Hour), nil, now.Add(-26 * time.Hour)},
		{"armed retention on the view", spec("prices_15m", "trades", 0), map[string]time.Duration{"prices_15m": 48 * time.Hour}, now.Add(-48 * time.Hour)},
		{"retention on trades never moves it", spec("prices_1h", "trades", 0), map[string]time.Duration{"trades": time.Hour}, src},
		{"armed retention on a non-trades source", spec("oracle_prices_1h", "oracle_updates", 0), map[string]time.Duration{"oracle_updates": 72 * time.Hour}, now.Add(-72 * time.Hour)},
		{"a floor older than the source is moot", spec("x", "trades", 365*24*time.Hour), nil, src},
	}
	for _, c := range cases {
		if got := expectedHistoryStart(src, now, c.spec, c.retention); !got.Equal(c.want) {
			t.Errorf("%s: expected start %s, want %s", c.name, got, c.want)
		}
	}
}

// TestCAGGHistoryRefusesUnregisteredRelations: identifiers are quoted, but only catalog shapes of registered views reach SQL.
func TestCAGGHistoryRefusesUnregisteredRelations(t *testing.T) {
	s := &Store{} // nil db: a refusal must happen before any query
	for owner, c := range map[string]timeColumn{
		`prices_1m"; DROP TABLE trades; --`: {"_timescaledb_internal", "_materialized_hypertable_1", "bucket"},
		"not_a_view":                        {"_timescaledb_internal", "_materialized_hypertable_1", "bucket"},
		"prices_1m":                         {"public", "trades", "bucket"},
		"trades":                            {"public", "prices_1m", "ts"},
		"oracle_updates":                    {"", "", ""},
	} {
		if _, err := s.minTime(context.Background(), owner, c); err == nil {
			t.Errorf("minTime(%q, %+v) was not refused", owner, c)
		}
	}
	for owner, c := range map[string]timeColumn{
		"prices_1mo": {"_timescaledb_internal", "_materialized_hypertable_42", "bucket"},
		"trades":     {"public", "trades", "ts"},
	} {
		if err := c.validate(owner); err != nil {
			t.Errorf("validate(%q) = %v, want accepted", owner, err)
		}
	}
}
