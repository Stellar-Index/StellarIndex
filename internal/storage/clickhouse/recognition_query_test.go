// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestDistinctShapesWindowQuery_NoWideColumns pins the structural half of the
// recognition OOM fix: the distinct-shape scan reads ONLY the
// narrow identity columns. Any argMax(topics_xdr)/argMax(data_xdr)
// exemplar state — one wide string pair per distinct key — is what scales the
// query's footprint with the post-P23 distinct-shape population until it dies
// at any server memory cap. topics_xdr is read only for the non-Symbol
// topic[0] key, and is empty for every Symbol shape.
func TestDistinctShapesWindowQuery_NoWideColumns(t *testing.T) {
	q := distinctShapesWindowQuery(ClassicTokenTopic0Syms)
	for _, forbidden := range []string{"data_xdr", "op_args_xdr", "argMax"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("shape scan must not touch %q (wide-column read is phase 2's batched exemplar fetch):\n%s", forbidden, q)
		}
	}
	if got := strings.Count(q, "topics_xdr"); got != 3 || strings.Count(q, "if(topic_0_sym = '', ") != 3 {
		t.Errorf("topics_xdr must appear only in the three non-Symbol-guarded key columns (got %d):\n%s", got, q)
	}
	for _, required := range []string{
		"GROUP BY contract_id, topic_0_sym, t0, t1, tn",
		"count() AS cnt",
		"min(ledger_seq) AS lo",
		"max(ledger_seq) AS hi",
		"WHERE ledger_seq BETWEEN ? AND ?",
		"topic_0_sym NOT IN ('transfer','mint','burn','clawback','approve','set_admin','set_authorized')",
	} {
		if !strings.Contains(q, required) {
			t.Errorf("shape scan missing %q:\n%s", required, q)
		}
	}
}

// TestDistinctShapesWindowQuery_BoundedSettings — the scan carries its own
// per-query bounds (low threads + external group-by spill), so a window whose
// distinct set grows costs disk and time, never an OOM kill.
func TestDistinctShapesWindowQuery_BoundedSettings(t *testing.T) {
	q := distinctShapesWindowQuery(nil)
	for _, s := range []string{
		"SETTINGS",
		"max_threads = 2",
		"max_memory_usage = 8589934592",
		"max_bytes_before_external_group_by = 4294967296",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("shape scan missing bounded setting %q:\n%s", s, q)
		}
	}
	if strings.Contains(q, "NOT IN") {
		t.Errorf("no-exclusion query must not carry a NOT IN clause:\n%s", q)
	}
}

// TestDistinctShapesWatchedQuery_IncludesTopicsAndContracts pins the
// watched-SEP41 scoped census: unlike the global scan's NOT IN
// exclusion, this query INCLUDEs the given topic[0] set and restricts to the
// given contract set — the shape that lets a watched SEP-41 source's own
// classic-token event kinds be audited without re-scanning the firehose for
// every contract in the lake.
func TestDistinctShapesWatchedQuery_IncludesTopicsAndContracts(t *testing.T) {
	q := distinctShapesWatchedQuery(FirehoseExcludeSyms, []string{"CAAA", "CBBB"})
	for _, required := range []string{
		"topic_0_sym IN ('transfer','mint','burn','clawback','approve','set_authorized')",
		"contract_id IN ('CAAA','CBBB')",
		"GROUP BY contract_id, topic_0_sym",
		"'' AS t0",
		"WHERE ledger_seq BETWEEN ? AND ?",
	} {
		if !strings.Contains(q, required) {
			t.Errorf("watched shape scan missing %q:\n%s", required, q)
		}
	}
	if strings.Contains(q, "NOT IN") {
		t.Errorf("watched shape scan must INCLUDE the topic set, not exclude it:\n%s", q)
	}
	if strings.Contains(q, "topics_xdr") {
		t.Errorf("watched shape scan only matches Symbol topics, so it must not read topics_xdr:\n%s", q)
	}
}

// TestShapeExemplarQuery — phase 2 fetches each shape's representative pinned
// to the shape's OWN MaxLedger (primary-key range of one ledger per shape),
// with lake-sourced identity strings escaped.
func TestShapeExemplarQuery(t *testing.T) {
	shapes := []TopicShape{
		{ContractID: "CAAA", Topic0Sym: "swap", MaxLedger: 51_000_123},
		{ContractID: "CBBB", Topic0Sym: "it's odd", MaxLedger: 62_999_999},
		{ContractID: "CCCC", Topic0Sym: "swap", MaxLedger: 51_000_123}, // shares a ledger with CAAA
		{ContractID: "CDDD", t0: "AAAAAQ==", t1: "AAAAAg==", tn: 3, MaxLedger: 62_999_999},
	}
	q := shapeExemplarQuery(shapes)
	for _, required := range []string{
		"argMax((event_type, topics_xdr, data_xdr), ledger_seq) AS ex",
		"GROUP BY contract_id, topic_0_sym, t0, t1, tn",
		"(contract_id, topic_0_sym, t0, t1, tn) IN (",
		"('CAAA','swap','','',0)",
		`('CBBB','it\'s odd','','',0)`,
		"('CCCC','swap','','',0)",
		"('CDDD','','AAAAAQ==','AAAAAg==',3)",
		"62999999",
		"SETTINGS",
		"max_threads = 2",
	} {
		if !strings.Contains(q, required) {
			t.Errorf("exemplar query missing %q:\n%s", required, q)
		}
	}
	// The shared MaxLedger dedups in the IN-set.
	if strings.Count(q, "51000123") != 1 {
		t.Errorf("shared MaxLedger should appear once in the ledger IN-set:\n%s", q)
	}
}
