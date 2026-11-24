package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// serveHolders runs GET /v1/assets/native/holders against a pre-seeded cache entry and returns the stale flag the
// handler hands the envelope writer.
func serveHolders(t *testing.T, entry assetHoldersEntry, lakeStale bool) (stale bool) {
	t.Helper()
	h := newProbeHandler(&holdersKeyReader{capReader: &capReader{probe: &deadlineProbe{}}}, nil)
	h.LakeWatermark = func(context.Context) (uint32, bool, bool) { return 0, lakeStale, true }
	entry.holders = []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 7}}
	entry.total = 1
	h.assetHolders.put("native", entry)
	written := false
	h.WriteJSONAt = func(_ http.ResponseWriter, _ any, s, _ bool, _ time.Time) { written, stale = true, s }
	h.WriteJSON = func(http.ResponseWriter, any, bool) { t.Fatal("handler bypassed WriteJSONAt") }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/native/holders", nil)
	req.SetPathValue("asset_id", "native")
	h.AssetHolders(rec, req)
	if !written {
		t.Fatalf("no JSON written: code=%d", rec.Code)
	}
	return stale
}

func TestAssetHolders_StaleBoundPerSource(t *testing.T) {
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }
	cases := []struct {
		name      string
		entry     assetHoldersEntry
		lakeStale bool
		want      bool
	}{
		{"rollup board 10m old is healthy", assetHoldersEntry{computedAt: ago(10 * time.Minute), cachedAt: time.Now()}, false, false},
		{"rollup board 80m old means the rollup is behind", assetHoldersEntry{computedAt: ago(80 * time.Minute), cachedAt: time.Now()}, false, true},
		{"live board 6m old keeps the 300s bound", assetHoldersEntry{cachedAt: ago(6 * time.Minute)}, false, true},
		{"live board 30s old is fresh", assetHoldersEntry{cachedAt: ago(30 * time.Second)}, false, false},
		{"lake watermark stale wins over a fresh rollup", assetHoldersEntry{computedAt: ago(10 * time.Minute), cachedAt: time.Now()}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := serveHolders(t, c.entry, c.lakeStale); got != c.want {
				t.Errorf("flags.stale = %v, want %v", got, c.want)
			}
		})
	}
}

// A rollup bound under two timer cycles would flag a healthy rollup stale between runs.
func TestHoldersRollupStaleBoundCoversTimerCadence(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "configs", "ansible", "roles", "archival-node", "templates", "systemd", "holders-rollup.timer.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^OnUnitInactiveSec=(\d+)min\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no OnUnitInactiveSec=<N>min line", path)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if cadence := time.Duration(n) * time.Minute; assetHoldersRollupStaleAfter < 2*cadence {
		t.Errorf("assetHoldersRollupStaleAfter = %v, want >= 2x the timer cadence %v", assetHoldersRollupStaleAfter, cadence)
	}
}
