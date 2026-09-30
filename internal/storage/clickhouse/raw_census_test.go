// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"reflect"
	"testing"
)

// TestRawCensusShortfalls pins the per-table partition comparison: loss is
// reported, duplicates and partitions that owe no rows are not, and the
// presence-only tables report an empty partition that has operations.
func TestRawCensusShortfalls(t *testing.T) {
	expected := map[uint32]rawCensusHeader{
		61: {tx: 10, op: 20, ev: 5},
		60: {tx: 10, op: 20, ev: 0},
		62: {tx: 4, op: 0, ev: 0},
	}
	present := map[string]map[uint32]uint64{
		// 60 short by one; 61 duplicated (present > expected); 62 exact.
		"transactions": {60: 9, 61: 25, 62: 4},
		// 61 absent (dropped); 62 owes nothing.
		"operations": {60: 20},
		// 60 owes no events and holds none; 61 exact.
		"contract_events": {61: 5},
		// 61 empty despite ops; 62 empty with no ops.
		"operation_results": {60: 3},
		// both partitions with ops have rows.
		"operation_participants": {60: 1, 61: 40},
	}

	short, totals := rawCensusShortfalls(expected, present)

	wantShort := []RawTableShortfall{
		{Table: "transactions", Partition: 60, Expected: 10, Present: 9},
		{Table: "operations", Partition: 61, Expected: 20, Present: 0},
		{Table: "operation_results", Partition: 61, Expected: 1, Present: 0},
	}
	if !reflect.DeepEqual(short, wantShort) {
		t.Fatalf("shortfalls = %+v, want %+v", short, wantShort)
	}
	wantTotals := []RawTableTotal{
		{Table: "transactions", Expected: 24, Present: 38},
		{Table: "operations", Expected: 40, Present: 20},
		{Table: "contract_events", Expected: 5, Present: 5},
		{Table: "operation_results", Expected: 40, Present: 3, PresenceOnly: true},
		{Table: "operation_participants", Expected: 40, Present: 41, PresenceOnly: true},
	}
	if !reflect.DeepEqual(totals, wantTotals) {
		t.Fatalf("totals = %+v, want %+v", totals, wantTotals)
	}

	// Map iteration must not leak into the output order.
	for i := 0; i < 20; i++ {
		again, _ := rawCensusShortfalls(expected, present)
		if !reflect.DeepEqual(again, wantShort) {
			t.Fatalf("run %d: shortfalls = %+v, want %+v", i, again, wantShort)
		}
	}
}

func TestRawCensusShortfalls_EmptyPresent(t *testing.T) {
	short, _ := rawCensusShortfalls(map[uint32]rawCensusHeader{7: {tx: 1, op: 1, ev: 1}}, nil)
	if len(short) != len(rawCensusTables) {
		t.Fatalf("an empty lake must short every table: got %+v", short)
	}
}
