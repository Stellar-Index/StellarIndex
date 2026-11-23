package keys

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

func TestLineageRecordHashes_ListsRecordsAcrossCursorsSkippingOwnerSets(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	var want []string
	for i := range 2500 {
		h := strings.Repeat("0", 60) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + "f"
		want = append(want, h)
		if err := mr.Set(cachekeys.APIKeyLineage(h).String(), "{}"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mr.SAdd(cachekeys.APIKeyLineageOwner("acct-x").String(), want[0]); err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(cachekeys.APIKey(want[0]).String(), "{}"); err != nil {
		t.Fatal(err)
	}
	got, err := lineageRecordHashes(context.Background(), rdb)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(slices.Compact(got), want) {
		t.Errorf("listed %d hashes, want the %d lineage records and no owner set or credential", len(got), len(want))
	}
}

func TestLineageBackfillFlags_RequireAStatedMode(t *testing.T) {
	if _, _, _, err := parseLineageBackfillFlags([]string{"-config", "x.toml"}); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("no -write or -dry-run: err = %v, want ErrWriteModeUnstated", err)
	}
	for _, mode := range []string{"-dry-run", "-write"} {
		if _, _, _, err := parseLineageBackfillFlags([]string{"-config", "x.toml", mode}); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	if _, _, _, err := parseLineageBackfillFlags([]string{"-dry-run"}); err == nil {
		t.Error("missing -config accepted")
	}
}

func TestPrintLineageBackfill_NamesKeysByIDAndModeVerb(t *testing.T) {
	rep := auth.LineageBackfillReport{Scanned: 5, Delegated: 3, Complete: 1, Resolved: []string{"k-a"}, Orphaned: []string{"k-b"}, MaxDepth: 2, LineageMissing: []string{"k-c"}}
	out := lineageBackfillSummary(rep, false)
	for _, want := range []string{"scanned 5 records: 3 delegated, 1 already complete, max depth 2", "would record the chain of 1 keys: k-a", "orphaned", "1 keys: k-b", "would create the lineage record of 1 keys: k-c"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	orphans := auth.LineageBackfillReport{LineageOrphans: 3, LineageOrphansDeleted: 2}
	if out := lineageBackfillSummary(orphans, false); !strings.Contains(out, "would delete 3 orphaned lineage records") {
		t.Errorf("dry-run output lacks the orphan count:\n%s", out)
	}
	if out := lineageBackfillSummary(orphans, true); !strings.Contains(out, "deleted 2 of 3 orphaned lineage records") {
		t.Errorf("write output lacks the orphan deletes:\n%s", out)
	}
	denied := lineageBackfillSummary(auth.LineageBackfillReport{LineageDenied: true}, false)
	if !strings.Contains(denied, "not checked, the Redis ACL denies apikey-lineage:*") || strings.Contains(denied, "lineage record of 0 keys") {
		t.Errorf("a denied lineage check must never read as 0 missing:\n%s", denied)
	}
}
