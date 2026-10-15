package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A failing run must still publish its tiers: the textfile is written before the violation exit.
func TestPublishUSDVolumeRun_WritesBeforeViolationExit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify_usd_volume.prom")
	rollups := map[timescale.USDVolumeTier]*usdVolumeTierRollup{timescale.TierQuotePegged: newTierRollup()}
	end := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 13, 5, 12, 0, 0, time.UTC)

	if err := publishUSDVolumeRun(path, end, rollups, 2, true, now); err == nil {
		t.Fatal("two violations must fail the run")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("textfile not written on the violation path: %v", err)
	}
	if !strings.Contains(string(body), "stellarindex_verify_usd_volume_violations 2") {
		t.Errorf("textfile lacks the violation count:\n%s", body)
	}

	if err := publishUSDVolumeRun(path, end, rollups, 0, true, now); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	none := filepath.Join(dir, "none.prom")
	if err := publishUSDVolumeRun("", end, rollups, 0, true, now); err != nil {
		t.Fatalf("no textfile: %v", err)
	}
	if _, err := os.Stat(none); !os.IsNotExist(err) {
		t.Errorf("an empty textfile path must write nothing")
	}
}
