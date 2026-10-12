package explorer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Covers the /v1/operations first-page cache: miss on empty, hit within TTL, one entry serving every limit (a per-limit
// key would buy its own lake read), and an entry past opsDirTTL still returned with fresh=false. Staleness is the
// caller's call (serve flags.stale and kick a detached rebuild), as in hot_reads.go and contract_detail_cache.go.
func TestOpsDirCache(t *testing.T) {
	var c opsDirCache

	// Miss on an empty (zero-value) cache — must not panic.
	if _, ok, _ := c.get(); ok {
		t.Fatal("empty cache returned a hit")
	}

	view := OperationsView{NextCursor: "abc", Operations: make([]OpView, 3)}
	c.put(view)

	// Hit within TTL, marked fresh.
	e, ok, fresh := c.get()
	if !ok {
		t.Fatal("expected a hit right after put")
	}
	if !fresh {
		t.Error("entry right after put reported stale")
	}
	if e.view.NextCursor != "abc" || len(e.view.Operations) != 3 {
		t.Fatalf("cached view mismatch: %+v", e.view)
	}
	if e.cachedAt.IsZero() {
		t.Error("entry carries no fill time — the served as_of would be a lie")
	}

	// ONE entry serves every limit: the cache itself is limit-
	// agnostic now — slicing to the caller's requested limit is the
	// handler's job (sliceOperationsView), not this cache's.
	e2, ok, _ := c.get()
	if !ok || e2.view.NextCursor != "abc" {
		t.Fatal("a second get() must return the SAME entry, not miss on a different requested limit")
	}

	// Past the TTL: still returned, marked stale, with its real fill time.
	c.mu.Lock()
	backdated := time.Now().Add(-2 * opsDirTTL)
	entry := c.entry
	entry.cachedAt = backdated
	c.entry = entry
	c.mu.Unlock()

	e, ok, fresh = c.get()
	if !ok {
		t.Fatal("expired entry must still be RETURNED (stale-serve), not dropped")
	}
	if fresh {
		t.Error("entry older than opsDirTTL reported fresh")
	}
	if e.view.NextCursor != "abc" {
		t.Errorf("stale entry lost its payload: %+v", e.view)
	}
	if !e.cachedAt.Equal(backdated) {
		t.Errorf("stale entry cachedAt = %v, want the real fill time %v", e.cachedAt, backdated)
	}
}

// ledgerOpsTotalReader feeds GET /v1/ledgers/{seq}/operations a fixed page of
// rows and a controllable ledger header, to exercise the Total/Truncated
// signal independently of the response-byte-budget test's row content.
type ledgerOpsTotalReader struct {
	*capReader
	rows []clickhouse.OpRow
	hdr  clickhouse.LedgerHeader
}

func (r *ledgerOpsTotalReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

func (r *ledgerOpsTotalReader) LedgerBySeq(ctx context.Context, _ uint32) (clickhouse.LedgerHeader, bool, error) {
	r.probe.record(ctx)
	return r.hdr, true, nil
}

// ledgerOpsReader is a capReader whose OperationsByLedger returns a
// caller-supplied fixed page — used to feed GET /v1/ledgers/{seq}/operations
// rows with controlled BodyXDR sizes.
type ledgerOpsReader struct {
	*capReader
	rows []clickhouse.OpRow
}

func (r *ledgerOpsReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

// garbageXDR is undecodable so opView takes the decode-failure path, which echoes BodyXDR onto
// RawXDR: a deterministic signal that a row was FULLY processed. opViewLight never sets RawXDR, so
// RawXDR == "" signals the row was served undecoded.
func garbageXDR(n int) string {
	return strings.Repeat("Z", n)
}
