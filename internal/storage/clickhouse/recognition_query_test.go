// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestDistinctSymbolShapesWindowQuery_NoWideColumns pins the recognition OOM and scan-cost fixes:
// the Symbol scan reads ONLY the narrow identity columns. argMax exemplar state scales with the
// distinct-shape population until it dies at any memory cap, and topics_xdr is the lake's widest column.
func TestDistinctSymbolShapesWindowQuery_NoWideColumns(t *testing.T) {
	q := distinctSymbolShapesWindowQuery(ClassicTokenTopic0Syms, nil)
	for _, forbidden := range []string{"topics_xdr", "data_xdr", "op_args_xdr", "argMax", "contract_id IN"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("Symbol shape scan must not contain %q:\n%s", forbidden, q)
		}
	}
	for _, required := range []string{
		"GROUP BY contract_id, topic_0_sym",
		"'' AS t0",
		"count() AS cnt",
		"min(ledger_seq) AS lo",
		"max(ledger_seq) AS hi",
		"WHERE ledger_seq BETWEEN ? AND ? AND topic_0_sym != ''",
		"AND topic_0_sym NOT IN ('transfer','mint','burn','clawback','approve','set_admin','set_authorized')",
	} {
		if !strings.Contains(q, required) {
			t.Errorf("Symbol shape scan missing %q:\n%s", required, q)
		}
	}
}

// TestDistinctSymbolShapesWindowQuery_WatchedContractsKeepExcludedTopics — a watched SEP-41
// contract's transfer/mint/burn stay in scope, so a watched source dropping them still gaps.
func TestDistinctSymbolShapesWindowQuery_WatchedContractsKeepExcludedTopics(t *testing.T) {
	q := distinctSymbolShapesWindowQuery(FirehoseExcludeSyms, []string{"CAAA", "C'BB"})
	want := "AND (topic_0_sym NOT IN ('transfer','mint','burn','clawback','approve','set_authorized') OR contract_id IN ('CAAA','C\\'BB'))"
	if !strings.Contains(q, want) {
		t.Errorf("Symbol shape scan missing %q:\n%s", want, q)
	}
	if q := distinctSymbolShapesWindowQuery(nil, []string{"CAAA"}); strings.Contains(q, "contract_id IN") || strings.Contains(q, "NOT IN") {
		t.Errorf("no exclusion means nothing to re-admit; got:\n%s", q)
	}
}

// TestDistinctNonSymbolShapesWindowQuery_PrewhereGuardsTopics — topics_xdr is read only behind
// a PREWHERE on an empty topic_0_sym, and no alias may shadow topic_0_sym (CH resolves aliases in PREWHERE).
func TestDistinctNonSymbolShapesWindowQuery_PrewhereGuardsTopics(t *testing.T) {
	q := distinctNonSymbolShapesWindowQuery()
	for _, required := range []string{
		"PREWHERE topic_0_sym = ''",
		"WHERE ledger_seq BETWEEN ? AND ?",
		"topics_xdr[1] AS t0",
		"length(topics_xdr) AS tn",
		"GROUP BY contract_id, topic_0_sym, t0, t1, tn",
	} {
		if !strings.Contains(q, required) {
			t.Errorf("non-Symbol shape scan missing %q:\n%s", required, q)
		}
	}
	if strings.Contains(q, "AS topic_0_sym") || strings.Contains(q, "data_xdr") {
		t.Errorf("non-Symbol shape scan must not alias topic_0_sym or read data_xdr:\n%s", q)
	}
}

// TestShapeScanQueries_BoundedSettings — each scan carries its own per-query bounds (low threads
// + external group-by spill), so a window whose distinct set grows costs disk and time, never an OOM kill.
func TestShapeScanQueries_BoundedSettings(t *testing.T) {
	for _, q := range []string{distinctSymbolShapesWindowQuery(nil, nil), distinctNonSymbolShapesWindowQuery()} {
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
