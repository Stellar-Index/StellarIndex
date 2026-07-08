// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestDistinctShapesWindowQuery_NoWideColumns pins the structural half of the
// 2026-07-08 recognition OOM fix: the distinct-shape scan reads ONLY the
// narrow identity columns. The old form's argMax(topics_xdr)/argMax(data_xdr)
// exemplar state — one wide string pair per distinct key — is what scaled the
// query's footprint with the post-P23 distinct-shape population until it died
// at any server memory cap.
func TestDistinctShapesWindowQuery_NoWideColumns(t *testing.T) {
	q := distinctShapesWindowQuery(ClassicTokenTopic0Syms)
	for _, forbidden := range []string{"topics_xdr", "data_xdr", "op_args_xdr", "argMax"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("shape scan must not touch %q (wide-column read is phase 2's batched exemplar fetch):\n%s", forbidden, q)
		}
	}
	for _, required := range []string{
		"GROUP BY contract_id, topic_0_sym",
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

// TestShapeExemplarQuery — phase 2 fetches each shape's representative pinned
// to the shape's OWN MaxLedger (primary-key range of one ledger per shape),
// with lake-sourced identity strings escaped.
func TestShapeExemplarQuery(t *testing.T) {
	shapes := []TopicShape{
		{ContractID: "CAAA", Topic0Sym: "swap", MaxLedger: 51_000_123},
		{ContractID: "CBBB", Topic0Sym: "it's odd", MaxLedger: 62_999_999},
		{ContractID: "CCCC", Topic0Sym: "swap", MaxLedger: 51_000_123}, // shares a ledger with CAAA
	}
	q := shapeExemplarQuery(shapes)
	for _, required := range []string{
		"argMax(event_type, ledger_seq)",
		"argMax(topics_xdr, ledger_seq)",
		"argMax(data_xdr, ledger_seq)",
		"GROUP BY contract_id, topic_0_sym",
		"('CAAA','swap')",
		`('CBBB','it\'s odd')`,
		"('CCCC','swap')",
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
