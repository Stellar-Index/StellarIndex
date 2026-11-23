// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"errors"
	"reflect"
	"strings"
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

// TestCensusShortfalls pins the partition comparison: a dropped partition (absent from system.parts) and a short one are
// reported; a partition over its count is recounted by identity, so duplicates that complete it pass and duplicates standing
// in for lost events (54: 12 rows, 8 distinct) are reported at the identity count.
func TestCensusShortfalls(t *testing.T) {
	expected := []EventCensusShortfall{
		{Partition: 50, FirstEventLedger: 50_457_424, Expected: 10},
		{Partition: 51, FirstEventLedger: 51_000_000, Expected: 10},
		{Partition: 52, FirstEventLedger: 52_000_003, Expected: 10},
		{Partition: 53, FirstEventLedger: 53_000_000, Expected: 10},
		{Partition: 54, FirstEventLedger: 54_000_000, Expected: 10},
	}
	present := map[uint32]uint64{50: 10, 52: 9, 53: 25, 54: 12}
	distinct := map[uint32]uint64{53: 10, 54: 8}
	var recounted []uint32
	got, err := censusShortfalls(expected, present, func(p uint32) (uint64, error) {
		recounted = append(recounted, p)
		return distinct[p], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []EventCensusShortfall{
		{Partition: 51, FirstEventLedger: 51_000_000, Expected: 10, Present: 0},
		{Partition: 52, FirstEventLedger: 52_000_003, Expected: 10, Present: 9},
		{Partition: 54, FirstEventLedger: 54_000_000, Expected: 10, Present: 8},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("censusShortfalls = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(recounted, []uint32{53, 54}) {
		t.Fatalf("recounted %v, want only the over-count partitions [53 54]", recounted)
	}
}

// The identity recount runs over a whole 1M-ledger partition on every over-count, so it must stream (FINAL over the ORDER BY
// key, not an in-memory uniqExact set) and carry the package's memory and time caps.
func TestEventCensusIdentityQuery_StreamsUnderCaps(t *testing.T) {
	q := strings.Join(strings.Fields(eventCensusIdentityQuery), " ")
	for _, want := range []string{
		"FROM stellar.contract_events FINAL WHERE ledger_seq BETWEEN ? AND ?",
		"do_not_merge_across_partitions_select_final = 1",
		"max_memory_usage = 8589934592",
		"max_execution_time = 600",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("identity recount query lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "uniqExact") {
		t.Errorf("identity recount must not build an in-memory uniqExact set:\n%s", q)
	}
}

func TestCensusShortfalls_RecountErrorFails(t *testing.T) {
	expected := []EventCensusShortfall{{Partition: 53, Expected: 10}}
	if _, err := censusShortfalls(expected, map[uint32]uint64{53: 11}, func(uint32) (uint64, error) {
		return 0, errors.New("boom")
	}); err == nil {
		t.Fatal("a failed identity recount must fail the census, not pass the partition")
	}
}
