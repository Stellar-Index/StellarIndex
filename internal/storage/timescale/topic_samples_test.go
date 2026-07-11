package timescale

import (
	"strings"
	"testing"
	"time"
)

// TestDistinctSorobanTopicSamplesWindowedQuery_CarriesTheBound pins the
// 2026-07-11 fix: an operator's non-`-ch` compute-completeness run
// against the (by then full-history) soroban_events table walked the
// old unbounded DISTINCT ON scan for 2h before being cancelled — the
// same failure mode the gap detector hit on 2026-07-06 against the
// same table's growth (see gap_window_test.go). The query MUST always
// carry the trailing-window floor ($3), independent of whether the
// ledger_ingest_log range-covered fast path also applies, or a future
// edit could silently drop the bound while leaving the (optional)
// [$4,$5] chunk-pruning bound in place.
func TestDistinctSorobanTopicSamplesWindowedQuery_CarriesTheBound(t *testing.T) {
	t.Parallel()
	const bound = "ledger_close_time >= $3"

	for _, rangeCovered := range []bool{false, true} {
		q := distinctSorobanTopicSamplesWindowedQuery(rangeCovered)
		if !strings.Contains(q, bound) {
			t.Errorf("rangeCovered=%v: query missing trailing-window bound %q:\n%s", rangeCovered, bound, q)
		}
		// The query must still read soroban_events by its ledger range
		// (the [from,to] the caller asked for) — the window bound is
		// additive, not a replacement.
		if !strings.Contains(q, "ledger BETWEEN $1 AND $2") {
			t.Errorf("rangeCovered=%v: query missing the base [from,to] ledger bound:\n%s", rangeCovered, q)
		}
	}

	covered := distinctSorobanTopicSamplesWindowedQuery(true)
	if !strings.Contains(covered, "ledger_close_time BETWEEN $4 AND $5") {
		t.Errorf("rangeCovered=true: query missing the chunk-pruning bound:\n%s", covered)
	}
	uncovered := distinctSorobanTopicSamplesWindowedQuery(false)
	if strings.Contains(uncovered, "$4") || strings.Contains(uncovered, "$5") {
		t.Errorf("rangeCovered=false: query should not reference $4/$5:\n%s", uncovered)
	}
}

// TestDistinctTopicSampleWindow_IsBounded guards against the constant
// silently growing back toward "full history" — e.g. someone bumping
// it to cover a specific incident instead of using the Phase-2/3
// fallback path that already exists for shapes outside the window.
func TestDistinctTopicSampleWindow_IsBounded(t *testing.T) {
	t.Parallel()
	if distinctTopicSampleWindow <= 0 {
		t.Fatal("distinctTopicSampleWindow must be positive")
	}
	if distinctTopicSampleWindow > 90*24*time.Hour {
		t.Errorf("distinctTopicSampleWindow = %s; want <= 90d — a wider trailing window reintroduces the unbounded-scan cost class as history grows, defeating the point of a fixed window", distinctTopicSampleWindow)
	}
}

// TestOneSorobanTopicSampleQuery_IsIndexBacked pins the PHASE 3
// per-pair fallback query shape: equality on (contract_id,
// topic_0_sym) — the exact columns of
// soroban_events_contract_topic_idx (migration 0041) — plus a LIMIT 1
// with no ORDER BY, so Postgres can stop at the first matching index
// entry instead of sorting the pair's full row set.
func TestOneSorobanTopicSampleQuery_IsIndexBacked(t *testing.T) {
	t.Parallel()
	q := oneSorobanTopicSampleQuery
	if !strings.Contains(q, "WHERE e.contract_id = $1 AND e.topic_0_sym = $2") {
		t.Fatal("fallback sample query must filter by the (contract_id, topic_0_sym) equality prefix of soroban_events_contract_topic_idx")
	}
	if !strings.Contains(q, "LIMIT 1") {
		t.Fatal("fallback sample query must cap at one row — recognition needs any one example, not every row for the pair")
	}
	if strings.Contains(q, "ORDER BY") {
		t.Error("fallback sample query must not ORDER BY — sorting defeats the index-backed LIMIT 1 stop-early plan")
	}
}

// TestDistinctSorobanContractTopicPairsQuery_IsIndexOnly pins the
// PHASE 2 discovery query: it must filter topic_0_sym IS NOT NULL to
// match soroban_events_contract_topic_idx's partial-index predicate
// exactly (migration 0041) — any other predicate (or none) forces a
// full index scan or heap access instead of an index-only scan, right
// back to the O(rows) cost this whole fix removes.
func TestDistinctSorobanContractTopicPairsQuery_IsIndexOnly(t *testing.T) {
	t.Parallel()
	q := distinctSorobanContractTopicPairsQuery
	if !strings.Contains(q, "WHERE topic_0_sym IS NOT NULL") {
		t.Fatal("pair-discovery query must filter topic_0_sym IS NOT NULL to match the partial index soroban_events_contract_topic_idx")
	}
	if strings.Contains(q, "ledger_close_time") || strings.Contains(q, "ledger BETWEEN") {
		t.Error("pair-discovery query must stay unscoped by ledger range — adding one would force heap access (ledger isn't in the composite index), defeating the index-only scan")
	}
}
