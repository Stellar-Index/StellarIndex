package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The slow arm of stellarindex_timescale_job_failures_climbing. The counter
// is per job, so a job scheduled every T accrues at most window/T failures:
// the arm can only fire for T <= window/minFailures. Policies that omit
// schedule_interval take TimescaleDB's derived default (12h for compression
// on r1, per runbooks/compression-lag.md), which is inside that bound.
const (
	jobFailureSlowArm     = "increase(stellarindex_timescale_job_failures_total[3d]) >= 3"
	jobFailureSlowWindow  = 72 * time.Hour
	jobFailureMinFailures = 3
)

var (
	scheduleArgRe     = regexp.MustCompile(`(?i)schedule_interval\s*=>`)
	scheduleLiteralRe = regexp.MustCompile(`(?i)schedule_interval\s*=>\s*INTERVAL\s*'([^']+)'`)
	scheduleColumnRe  = regexp.MustCompile(`(?i)schedule_interval\s*=>\s*\w+\.(\w+)::interval`)
	valuesBlockRe     = regexp.MustCompile(`(?is)\bVALUES\b(.*?)\)\s*AS\s+\w+\s*\(([^)]*)\)`)
	tupleTailRe       = regexp.MustCompile(`'([^']+)'\s*\)\s*,?\s*$`)
	durationRe        = regexp.MustCompile(`^(\d+)\s*(second|minute|hour|day|week|month)s?$`)
)

// TestJobScheduleIntervalsFitJobFailureAlert pins #888's load-bearing
// assumption: every TimescaleDB job a migration schedules runs often enough
// for the job-failure alert's slow arm to be able to fire on it.
func TestJobScheduleIntervalsFitJobFailureAlert(t *testing.T) {
	for _, rules := range []string{
		"../deploy/monitoring/rules/storage.yml",
		"../configs/prometheus/rules.r1/storage.yml",
	} {
		raw, err := os.ReadFile(rules)
		if err != nil {
			t.Fatalf("read %s: %v", rules, err)
		}
		if !strings.Contains(string(raw), jobFailureSlowArm) {
			t.Errorf("%s: stellarindex_timescale_job_failures_climbing lacks the arm %q; "+
				"without it any job scheduled less often than every 36m can fail forever unalerted (#888)",
				rules, jobFailureSlowArm)
		}
	}

	limit := jobFailureSlowWindow / jobFailureMinFailures
	files, err := filepath.Glob("*.sql")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, s := range scheduleIntervals(t, f, stripSQLComments(string(raw))) {
			checked++
			d, ok := parseScheduleInterval(s)
			if !ok {
				t.Errorf("%s: cannot parse schedule_interval %q; extend parseScheduleInterval", f, s)
				continue
			}
			if d > limit {
				t.Errorf("%s: schedule_interval %q exceeds %s, so the job-failure alert's slow arm "+
					"(%d failures in %s) can never fire for this job; widen the arm first (#888)",
					f, s, limit, jobFailureMinFailures, jobFailureSlowWindow)
			}
		}
	}
	t.Logf("checked %d schedule_interval values across %d migration files", checked, len(files))
	if checked == 0 {
		t.Fatal("found no schedule_interval in any migration; the scan is broken")
	}
}

// scheduleIntervals returns every schedule_interval value in sql: inline
// INTERVAL literals, and the VALUES-table column that a
// `schedule_interval => g.<col>::interval` loop reads (0115, 0147). Any other
// form fails the test rather than being skipped.
func scheduleIntervals(t *testing.T, file, sql string) []string {
	t.Helper()
	var out []string
	literals := scheduleLiteralRe.FindAllStringSubmatch(sql, -1)
	columns := scheduleColumnRe.FindAllStringSubmatch(sql, -1)
	if n := len(scheduleArgRe.FindAllStringIndex(sql, -1)); n != len(literals)+len(columns) {
		t.Errorf("%s: %d schedule_interval arguments but only %d are literal or VALUES-column forms",
			file, n, len(literals)+len(columns))
	}
	for _, m := range literals {
		out = append(out, m[1])
	}
	for _, m := range columns {
		vals := valuesColumn(sql, m[1])
		if len(vals) == 0 {
			t.Errorf("%s: schedule_interval reads column %q but no VALUES table ending in it was found", file, m[1])
		}
		out = append(out, vals...)
	}
	return out
}

// valuesColumn returns the trailing literal of each tuple in the VALUES
// table whose column list ends in col.
func valuesColumn(sql, col string) []string {
	var out []string
	for _, b := range valuesBlockRe.FindAllStringSubmatch(sql, -1) {
		cols := strings.Split(b[2], ",")
		if strings.TrimSpace(cols[len(cols)-1]) != col {
			continue
		}
		body := b[1] + ")"
		for _, line := range strings.Split(body, "\n") {
			if m := tupleTailRe.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, l := range lines {
		if j := strings.Index(l, "--"); j >= 0 {
			lines[i] = l[:j]
		}
	}
	return strings.Join(lines, "\n")
}

func parseScheduleInterval(s string) (time.Duration, bool) {
	m := durationRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	unit := map[string]time.Duration{
		"second": time.Second,
		"minute": time.Minute,
		"hour":   time.Hour,
		"day":    24 * time.Hour,
		"week":   7 * 24 * time.Hour,
		"month":  30 * 24 * time.Hour,
	}[m[2]]
	return time.Duration(n) * unit, true
}
