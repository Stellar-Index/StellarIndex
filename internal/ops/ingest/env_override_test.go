package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIngestConfig drops a minimal, otherwise-valid stellarindex.toml
// carrying a KNOWN-GOOD postgres DSN, so a bare config.Load() accepts the
// file and only an APPLIED env override can make config load fail.
func writeIngestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stellarindex.toml")
	body := `
[storage]
postgres_dsn = "postgres://good:good@localhost/stellarindex?sslmode=disable"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBackfillIndex_HonorsEnvOverride proves backfill-index loads config
// via LoadWithEnv (Load + ApplyEnvOverrides + re-Validate) rather than
// bare config.Load — the C3-14 class the archive commands already pin.
//
// It reproduces a real production failure: on r1 the postgres credentials
// live in STELLARINDEX_POSTGRES_DSN, so a bare Load fell back to the
// file's DSN and the run died with "failed SASL auth" after the API work
// was already done. A DELIBERATELY INVALID override is the signal: only a
// path that applies it and re-validates surfaces a postgres_dsn error.
func TestBackfillIndex_HonorsEnvOverride(t *testing.T) {
	cfgPath := writeIngestConfig(t)
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://injected-but-invalid")

	err := Run([]string{
		"backfill-index",
		"-config", cfgPath,
		"-from", "2015-09-30T00:00:00Z",
		"-to", "2017-01-17T00:00:00Z",
		// Wide enough to clear the pre-2018 granularity guard, so this
		// test fails on the DSN override and nothing else.
		"-chunk-days", "180",
	})
	if err == nil {
		t.Fatal("expected an error (invalid env-injected DSN should be rejected once overrides are applied)")
	}
	if !strings.Contains(err.Error(), "postgres_dsn") {
		t.Fatalf("env override not honored — err=%q does not mention postgres_dsn "+
			"(bare Load ignores STELLARINDEX_POSTGRES_DSN and fails later, at connect time)", err.Error())
	}
}

// TestBackfillIndex_RefusesHourlyWindowBefore2018 pins the granularity
// trap. CoinGecko chooses hourly vs daily from the window WIDTH, and has
// no hourly history before 2018 — so a pre-2018 range walked in <=90-day
// chunks returns an EMPTY series with no error. That looked exactly like
// "the source has no data for 2015", which is false: the same window at
// -chunk-days 180 returns one point per day.
//
// The default -chunk-days is 80, so the DEFAULT invocation hit this.
func TestBackfillIndex_RefusesHourlyWindowBefore2018(t *testing.T) {
	cfgPath := writeIngestConfig(t)

	err := Run([]string{
		"backfill-index",
		"-config", cfgPath,
		"-from", "2015-09-30T00:00:00Z",
		"-to", "2017-01-17T00:00:00Z",
		"-chunk-days", "90",
	})
	if err == nil {
		t.Fatal("expected a refusal: a pre-2018 window in <=90-day chunks silently returns zero rows")
	}
	if !strings.Contains(err.Error(), "HOURLY") {
		t.Fatalf("error does not explain the granularity trap: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "-chunk-days greater than 90") {
		t.Fatalf("error does not carry the remedy: %q", err.Error())
	}
}

// TestBackfillIndex_AllowsWideWindowBefore2018 is the twin: the same
// pre-2018 window with a wider chunk must pass the guard (it then fails
// later, at config/DSN validation, which is a different error).
func TestBackfillIndex_AllowsWideWindowBefore2018(t *testing.T) {
	cfgPath := writeIngestConfig(t)
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://injected-but-invalid")

	err := Run([]string{
		"backfill-index",
		"-config", cfgPath,
		"-from", "2015-09-30T00:00:00Z",
		"-to", "2017-01-17T00:00:00Z",
		"-chunk-days", "180",
	})
	if err == nil {
		t.Fatal("expected the DSN override to fail the run")
	}
	if strings.Contains(err.Error(), "HOURLY") {
		t.Fatalf("granularity guard fired on a wide chunk: %q", err.Error())
	}
}
