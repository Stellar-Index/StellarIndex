package supply

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWriteSnapshotMetrics_PassRun(t *testing.T) {
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(50_001_806_812 * 10_000_000), // 50B XLM in stroops
		CirculatingSupply: big.NewInt(30_000_000_000 * 10_000_000),
		MaxSupply:         big.NewInt(50_001_806_812 * 10_000_000),
		Basis:             BasisXLMSDFReserveExclusion,
		LedgerSequence:    50_000_000,
		ObservedAt:        time.Unix(1_770_000_000, 0).UTC(),
	}
	var buf bytes.Buffer
	if err := writeSnapshotMetrics(&buf, snap, 1.234, true); err != nil {
		t.Fatalf("writeSnapshotMetrics: %v", err)
	}
	out := buf.String()
	wants := []string{
		`stellarindex_supply_snapshot_total_xlm{asset_key="XLM"} 50001806812.000`,
		`stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"} 30000000000.000`,
		`stellarindex_supply_snapshot_max_xlm{asset_key="XLM"} 50001806812.000`,
		`stellarindex_supply_snapshot_ledger{asset_key="XLM"} 50000000`,
		`stellarindex_supply_snapshot_observed_at_seconds{asset_key="XLM"} 1770000000`,
		`stellarindex_supply_snapshot_run_duration_seconds 1.234`,
		`stellarindex_supply_snapshot_unit_failed{asset_key="XLM"} 0`,
		`stellarindex_supply_snapshot_last_success_timestamp{asset_key="XLM"}`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

// TestWriteSnapshotMetrics_OmitsMaxWhenNil — max_supply is null
// for uncapped assets per ADR-0011 ("we don't fabricate"). The
// metric should be absent rather than emitting a zero or NaN.
func TestWriteSnapshotMetrics_OmitsMaxWhenNil(t *testing.T) {
	snap := Supply{
		AssetKey:          "USDC:GA5...",
		TotalSupply:       big.NewInt(1_000_000),
		CirculatingSupply: big.NewInt(900_000),
		MaxSupply:         nil, // uncapped
		Basis:             BasisIssuerExclusion,
		LedgerSequence:    100,
		ObservedAt:        time.Now().UTC(),
	}
	var buf bytes.Buffer
	if err := writeSnapshotMetrics(&buf, snap, 0.5, true); err != nil {
		t.Fatalf("writeSnapshotMetrics: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "stellarindex_supply_snapshot_max_xlm") {
		t.Errorf("nil MaxSupply should omit max_xlm metric:\n%s", out)
	}
	if !strings.Contains(out, `stellarindex_supply_snapshot_total_xlm{asset_key="USDC:GA5..."}`) {
		t.Errorf("total_xlm should still emit:\n%s", out)
	}
}

// TestWriteSnapshotMetrics_FailRun — failed runs (pass=false) emit
// unit_failed=1 and OMIT last_success_timestamp. The staleness alert
// keys on time-since-last-success, so omitting on failure preserves
// the previous-scrape value.
func TestWriteSnapshotMetrics_FailRun(t *testing.T) {
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(0),
		CirculatingSupply: big.NewInt(0),
		ObservedAt:        time.Now().UTC(),
	}
	var buf bytes.Buffer
	if err := writeSnapshotMetrics(&buf, snap, 0.1, false); err != nil {
		t.Fatalf("writeSnapshotMetrics: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `stellarindex_supply_snapshot_unit_failed{asset_key="XLM"} 1`) {
		t.Errorf("fail run should emit unit_failed=1:\n%s", out)
	}
	if strings.Contains(out, "stellarindex_supply_snapshot_last_success_timestamp") {
		t.Errorf("fail run should NOT emit last_success_timestamp:\n%s", out)
	}
}

func TestWriteFailureMetrics_NoValueGauges(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFailureMetrics(&buf, "native", 0.5); err != nil {
		t.Fatalf("writeFailureMetrics: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `stellarindex_supply_snapshot_unit_failed{asset_key="native"} 1`) {
		t.Errorf("failure path should emit unit_failed=1:\n%s", out)
	}
	if strings.Contains(out, "stellarindex_supply_snapshot_total_xlm") {
		t.Errorf("failure path has no Supply, should not emit value gauges:\n%s", out)
	}
	if strings.Contains(out, "stellarindex_supply_snapshot_last_success_timestamp") {
		t.Errorf("failure path should NOT emit last_success_timestamp:\n%s", out)
	}
}

func TestWriteSnapshotTextfile_AtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supply_snapshot.prom")
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(1_000_000),
		CirculatingSupply: big.NewInt(900_000),
		MaxSupply:         big.NewInt(1_000_000),
		LedgerSequence:    1,
		ObservedAt:        time.Now().UTC(),
	}
	if err := WriteSnapshotTextfile(path, snap, 1.0, true); err != nil {
		t.Fatalf("WriteSnapshotTextfile: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), "stellarindex_supply_snapshot_unit_failed") {
		t.Errorf("round-trip missing unit_failed:\n%s", body)
	}
	// `<path>.tmp` should be gone after a clean rename.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("temp file lingered after atomic write")
	}
}

func TestStroopsToXLM(t *testing.T) {
	cases := []struct {
		stroops string
		want    float64
	}{
		{"10000000", 1.0},                     // 1 XLM = 10^7 stroops
		{"50001806812000000", 5.0001806812e9}, // network total
		{"0", 0.0},
	}
	for _, tc := range cases {
		s, _ := new(big.Int).SetString(tc.stroops, 10)
		got := stroopsToXLM(s)
		if abs(got-tc.want) > 1e-3 {
			t.Errorf("stroopsToXLM(%s) = %g, want %g", tc.stroops, got, tc.want)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// grepLines returns every line of body that starts with prefix.
func grepLines(t *testing.T, body, prefix string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// TestWriteSnapshotFailureTextfile_CarriesLastSuccessTimestamp is the
// regression guard for the 36 h ticket / 72 h page escalation. The
// textfile collector serves exactly what the `.prom` file holds on
// each scrape, so a failure-path rewrite that omits
// last_success_timestamp RETIRES the series and both alerts — which
// evaluate `time() - <metric>` — go permanently no-data. Days 2..n of
// an outage must still carry the day-0 success timestamp verbatim.
func TestWriteSnapshotFailureTextfile_CarriesLastSuccessTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supply_snapshot.prom")
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(50_001_806_812 * 10_000_000),
		CirculatingSupply: big.NewInt(30_000_000_000 * 10_000_000),
		LedgerSequence:    50_000_000,
		ObservedAt:        time.Unix(1_770_000_000, 0).UTC(),
	}
	if err := WriteSnapshotTextfile(path, snap, 1.0, true); err != nil {
		t.Fatalf("seed success write: %v", err)
	}
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	want := grepLines(t, string(seeded), `stellarindex_supply_snapshot_last_success_timestamp{`)
	if len(want) != 1 {
		t.Fatalf("seed should hold exactly one last_success sample, got %d:\n%s", len(want), seeded)
	}

	// Three consecutive failed daily runs.
	for day := 1; day <= 3; day++ {
		if err := WriteSnapshotFailureTextfile(path, "native", 0.2); err != nil {
			t.Fatalf("failure write day %d: %v", day, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back day %d: %v", day, err)
		}
		body := string(raw)

		got := grepLines(t, body, `stellarindex_supply_snapshot_last_success_timestamp{`)
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("day %d: last_success_timestamp must survive verbatim as %q, got %q\nfile:\n%s",
				day, want[0], got, body)
		}
		// The family must be declared exactly once or the textfile
		// collector rejects the whole file.
		if n := len(grepLines(t, body, "# HELP stellarindex_supply_snapshot_last_success_timestamp ")); n != 1 {
			t.Errorf("day %d: want exactly 1 HELP line for the family, got %d:\n%s", day, n, body)
		}
		if n := len(grepLines(t, body, "# TYPE stellarindex_supply_snapshot_last_success_timestamp ")); n != 1 {
			t.Errorf("day %d: want exactly 1 TYPE line for the family, got %d:\n%s", day, n, body)
		}
		// …and the failure itself must still be reported, under the
		// same asset_key as the carried last_success sample so the two
		// alert tiers that read them fire on one series identity.
		if !strings.Contains(body, `stellarindex_supply_snapshot_unit_failed{asset_key="XLM"} 1`) {
			t.Errorf("day %d: failure write should emit unit_failed=1 for XLM:\n%s", day, body)
		}
		if keys := assetKeyLabels(body); len(keys) != 1 || keys[0] != "XLM" {
			t.Errorf("day %d: want one shared asset_key XLM in the file, got %q:\n%s", day, keys, body)
		}
		// No stale value gauges from the seeded success run — they
		// would read as a supply figure the failed run never computed.
		if strings.Contains(body, "stellarindex_supply_snapshot_total_xlm") {
			t.Errorf("day %d: value gauges must not be carried forward:\n%s", day, body)
		}
	}
}

// TestWriteSnapshotFailureTextfile_NoPriorFileEmitsNothing — a first-ever
// run that fails has no success to carry. The series must stay absent so
// `_never_initialized` (absent_over_time … == 1) still fires, rather than
// a fabricated timestamp making an uninitialized deployment look healthy.
func TestWriteSnapshotFailureTextfile_NoPriorFileEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supply_snapshot.prom")
	if err := WriteSnapshotFailureTextfile(path, "native", 0.2); err != nil {
		t.Fatalf("WriteSnapshotFailureTextfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(raw), "stellarindex_supply_snapshot_last_success_timestamp") {
		t.Errorf("no prior success to carry, must not fabricate one:\n%s", raw)
	}
	if !strings.Contains(string(raw), `stellarindex_supply_snapshot_unit_failed{asset_key="XLM"} 1`) {
		t.Errorf("first-run failure should still emit unit_failed=1 for XLM:\n%s", raw)
	}
}

// assetKeyLabels returns the distinct asset_key label values in a
// textfile body, in first-seen order.
func assetKeyLabels(body string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`asset_key="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	return keys
}

// TestWriteSnapshotFailureTextfile_UnreadablePriorFileIsNotErased pins the
// fail-safe choice: when the previous exposition exists but cannot be read,
// leave it in place (the staleness alerts keep escalating on the surviving
// series) rather than truncate it to publish unit_failed.
func TestWriteSnapshotFailureTextfile_UnreadablePriorFileIsNotErased(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the read permission bit")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "supply_snapshot.prom")
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(10_000_000),
		CirculatingSupply: big.NewInt(9_000_000),
		LedgerSequence:    7,
		ObservedAt:        time.Unix(1_770_000_000, 0).UTC(),
	}
	if err := WriteSnapshotTextfile(path, snap, 1.0, true); err != nil {
		t.Fatalf("seed success write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if err := WriteSnapshotFailureTextfile(path, "native", 0.2); err == nil {
		t.Fatal("unreadable prior exposition should surface an error, not a silent erase")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("prior exposition must be left intact:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestWriteSnapshotTextfile_FailFlagCarriesLastSuccess — the same invariant
// through the other exported writer. pass=false emits no fresh timestamp,
// so the previous one must survive here too.
func TestWriteSnapshotTextfile_FailFlagCarriesLastSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supply_snapshot.prom")
	snap := Supply{
		AssetKey:          "XLM",
		TotalSupply:       big.NewInt(10_000_000),
		CirculatingSupply: big.NewInt(9_000_000),
		LedgerSequence:    7,
		ObservedAt:        time.Unix(1_770_000_000, 0).UTC(),
	}
	if err := WriteSnapshotTextfile(path, snap, 1.0, true); err != nil {
		t.Fatalf("seed success write: %v", err)
	}
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	want := grepLines(t, string(seeded), `stellarindex_supply_snapshot_last_success_timestamp{`)
	if len(want) != 1 {
		t.Fatalf("seed should hold exactly one last_success sample, got %d", len(want))
	}
	if err := WriteSnapshotTextfile(path, snap, 1.0, false); err != nil {
		t.Fatalf("fail-flag write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := grepLines(t, string(raw), `stellarindex_supply_snapshot_last_success_timestamp{`)
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("want last_success carried verbatim as %q, got %q\nfile:\n%s", want[0], got, raw)
	}
}
