//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The churn ceiling on the daily directory sync, end to end against a
// real Timescale.
//
// The upstream is an unpinned branch of a third-party repo and a scam
// tag withholds the issuer's price. Before the ceiling, ReplaceDirectory
// refused only an EMPTY snapshot: a hijacked or partial one that kept
// a single row would prune every other label (un-withholding every
// scam issuer) or newly flag thousands of issuers (withholding their
// prices) in one committed transaction with nothing failing. These
// cases pin that such a snapshot is refused whole, that the refusal is
// a rollback, that ordinary churn still lands, and that the operator's
// explicit opt-in still accepts it.

// dirChurnEntries renders n distinct upstream rows, the first `flagged`
// of them carrying a scam-class tag.
func dirChurnEntries(n, flagged int) []timescale.DirectoryEntry {
	out := make([]timescale.DirectoryEntry, 0, n)
	for i := range n {
		tag := "exchange"
		if i < flagged {
			tag = "malicious"
		}
		out = append(out, dirEntry(dirAddress("CHURN"+dirLetters(i)), fmt.Sprintf("Row %d", i), tag))
	}
	return out
}

// dirLetters spells i in four base-26 letters: the strkey CHECK's
// alphabet is A-Z2-7, so decimal digits 0/1/8/9 would not pass it.
func dirLetters(i int) string {
	var b [4]byte
	for k := 3; k >= 0; k-- {
		b[k] = byte('A' + i%26)
		i /= 26
	}
	return string(b[:])
}

func dirSourceCounts(t *testing.T, ctx context.Context, store *timescale.Store, source string) (rows, flagged int64) {
	t.Helper()
	db := store.DB()
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM account_directory WHERE source = $1`, source).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM account_directory
		 WHERE source = $1 AND EXISTS (SELECT 1 FROM unnest(tags) t WHERE lower(t) = 'malicious')`, source).Scan(&flagged); err != nil {
		t.Fatalf("count flagged: %v", err)
	}
	return rows, flagged
}

func TestDirectorySync_ChurnCeiling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Bootstrap: 4000 rows, none flagged. The first sync of a source is
	// unbounded (nothing held yet), so this lands whatever its size.
	full := dirChurnEntries(4000, 0)
	res, err := store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, full, timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("bootstrap sync: %v", err)
	}
	if res.Existing != 0 || res.Upserted != 4000 {
		t.Fatalf("bootstrap: existing=%d upserted=%d, want 0/4000", res.Existing, res.Upserted)
	}
	// Default ceiling over 4000 held rows: max(100, ceil(5 %)) = 200.

	// 1. A snapshot that keeps only 3000 rows would prune 1000 > 200:
	//    refused whole, and the table is exactly as it was.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, full[:3000])
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("mass-prune snapshot: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if rows, _ := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 4000 {
		t.Fatalf("after refused prune: %d rows, want 4000 (the refusal must be a rollback)", rows)
	}

	// 2. The same 4000 rows with 1000 newly carrying `malicious` would
	//    withhold 1000 issuers' prices at once: refused, nothing flagged.
	_, _, err = store.ReplaceDirectory(ctx, dirUpstreamSource, dirChurnEntries(4000, 1000))
	if !errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		t.Fatalf("mass-flag snapshot: err = %v, want ErrDirectoryChurnExceeded", err)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 4000 || flagged != 0 {
		t.Fatalf("after refused flagging: rows=%d flagged=%d, want 4000/0", rows, flagged)
	}

	// 3. Ordinary churn — 150 newly flagged, 150 pruned, both under 200
	//    — lands, and the run reports what it did.
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirChurnEntries(3850, 150), timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("ordinary churn: %v", err)
	}
	if res.Existing != 4000 || res.Pruned != 150 || res.NewlyFlagged != 150 {
		t.Fatalf("ordinary churn: %+v, want existing=4000 pruned=150 newlyFlagged=150", res)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 3850 || flagged != 150 {
		t.Fatalf("after ordinary churn: rows=%d flagged=%d, want 3850/150", rows, flagged)
	}

	// 4. Re-syncing the same flagged rows counts nothing as NEWLY
	//    flagged: the base is "flagged before", not "flagged".
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, dirChurnEntries(3850, 150), timescale.DefaultDirectoryChurnLimit)
	if err != nil {
		t.Fatalf("idempotent resync: %v", err)
	}
	if res.NewlyFlagged != 0 || res.Pruned != 0 {
		t.Fatalf("idempotent resync: %+v, want newlyFlagged=0 pruned=0", res)
	}

	// 5. The operator's explicit opt-in accepts the mass prune.
	res, err = store.ReplaceDirectoryWithin(ctx, dirUpstreamSource, full[:1000], timescale.DirectoryChurnUnbounded)
	if err != nil {
		t.Fatalf("-accept-churn sync: %v", err)
	}
	if res.Pruned != 2850 {
		t.Fatalf("-accept-churn sync: pruned=%d, want 2850", res.Pruned)
	}
	if rows, flagged := dirSourceCounts(t, ctx, store, dirUpstreamSource); rows != 1000 || flagged != 0 {
		t.Fatalf("after -accept-churn: rows=%d flagged=%d, want 1000/0", rows, flagged)
	}
}
