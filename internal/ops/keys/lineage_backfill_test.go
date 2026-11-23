package keys

import (
	"context"
	"errors"
	"fmt"
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
	got, owners, err := lineageRecordHashes(context.Background(), rdb)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(slices.Compact(got), want) {
		t.Errorf("listed %d hashes, want the %d lineage records and no owner set or credential", len(got), len(want))
	}
	if owner := cachekeys.APIKeyLineageOwner("acct-x").String(); !slices.Equal(slices.Compact(owners), []string{owner}) {
		t.Errorf("listed owner sets %v, want [%s]", owners, owner)
	}
}

// The sweep deletes an owner set none of whose members has a lineage record or a credential, across SSCAN pages;
// keeps one with a member that has either; deletes nothing on a dry run; and finds nothing on a rerun.
func TestSweepOwnerSets_DeletesSetsWithNoStandingMember(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	add := func(acct string, members ...string) string {
		owner := cachekeys.APIKeyLineageOwner(acct).String()
		if _, err := mr.SAdd(owner, members...); err != nil {
			t.Fatal(err)
		}
		return owner
	}
	var big []string
	for i := range 2500 {
		big = append(big, fmt.Sprintf("gone%04d", i))
	}
	orphan, bigOrphan := add("acct-a", "gone-a1", "gone-a2"), add("acct-b", big...)
	withRecord, withCredential := add("acct-c", "gone-c", "rec-c"), add("acct-d", "cred-d")
	if err := mr.Set(cachekeys.APIKeyLineage("rec-c").String(), "{}"); err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(cachekeys.APIKey("cred-d").String(), "{}"); err != nil {
		t.Fatal(err)
	}
	_, owners, err := lineageRecordHashes(ctx, rdb)
	if err != nil {
		t.Fatal(err)
	}
	if n, del, err := sweepOwnerSets(ctx, rdb, false, owners); err != nil || n != 2 || del != 0 || !mr.Exists(orphan) || !mr.Exists(bigOrphan) {
		t.Fatalf("dry run: orphans %d, deleted %d, err %v; want 2 reported and none deleted", n, del, err)
	}
	if n, del, err := sweepOwnerSets(ctx, rdb, true, owners); err != nil || n != 2 || del != 2 {
		t.Fatalf("write: orphans %d, deleted %d, err %v; want 2 and 2", n, del, err)
	}
	if mr.Exists(orphan) || mr.Exists(bigOrphan) || !mr.Exists(withRecord) || !mr.Exists(withCredential) {
		t.Errorf("after write: orphan %v, big orphan %v, with record %v, with credential %v; want only the orphans gone",
			mr.Exists(orphan), mr.Exists(bigOrphan), mr.Exists(withRecord), mr.Exists(withCredential))
	}
	if n, del, err := sweepOwnerSets(ctx, rdb, true, owners); err != nil || n != 0 || del != 0 {
		t.Fatalf("rerun: orphans %d, deleted %d, err %v; want 0", n, del, err)
	}
}

// The delete re-checks: a set that gained a member, or whose member gained a record, since it was read stays.
func TestDeleteOrphanOwnerSet_RechecksAtomically(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	owner := cachekeys.APIKeyLineageOwner("acct-a").String()
	if _, err := mr.SAdd(owner, "h1", "h2"); err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.DeleteOrphanOwnerSet(ctx, rdb, owner, []string{"h1"}); err != nil || ok {
		t.Fatalf("set with an unread member: deleted %v, err %v; want kept", ok, err)
	}
	if err := mr.Set(cachekeys.APIKeyLineage("h2").String(), "{}"); err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.DeleteOrphanOwnerSet(ctx, rdb, owner, []string{"h1", "h2"}); err != nil || ok || !mr.Exists(owner) {
		t.Fatalf("set whose member has a record: deleted %v, err %v; want kept", ok, err)
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
	orphans := auth.LineageBackfillReport{LineageOrphans: 3, LineageOrphansDeleted: 2, LineageOwnerOrphans: 5, LineageOwnerOrphansDeleted: 4}
	if out := lineageBackfillSummary(orphans, false); !strings.Contains(out, "would delete 3 orphaned lineage records") ||
		!strings.Contains(out, "would delete 5 orphaned lineage owner sets") {
		t.Errorf("dry-run output lacks the orphan counts:\n%s", out)
	}
	if out := lineageBackfillSummary(orphans, true); !strings.Contains(out, "deleted 2 of 3 orphaned lineage records") ||
		!strings.Contains(out, "deleted 4 of 5 orphaned lineage owner sets") {
		t.Errorf("write output lacks the orphan deletes:\n%s", out)
	}
	denied := lineageBackfillSummary(auth.LineageBackfillReport{LineageDenied: true}, false)
	if !strings.Contains(denied, "not checked, the Redis ACL denies apikey-lineage:*") || strings.Contains(denied, "lineage record of 0 keys") {
		t.Errorf("a denied lineage check must never read as 0 missing:\n%s", denied)
	}
}
