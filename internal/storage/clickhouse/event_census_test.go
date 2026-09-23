package clickhouse

import (
	"reflect"
	"testing"
)

// TestCensusShortfalls pins the partition comparison: a dropped partition
// (absent from system.parts) and a short one are reported; unmerged duplicates
// (present > expected) are not.
func TestCensusShortfalls(t *testing.T) {
	expected := []EventCensusShortfall{
		{Partition: 50, FirstEventLedger: 50_457_424, Expected: 10},
		{Partition: 51, FirstEventLedger: 51_000_000, Expected: 10},
		{Partition: 52, FirstEventLedger: 52_000_003, Expected: 10},
		{Partition: 53, FirstEventLedger: 53_000_000, Expected: 10},
	}
	present := map[uint32]uint64{50: 10, 52: 9, 53: 25}
	got := censusShortfalls(expected, present)
	want := []EventCensusShortfall{
		{Partition: 51, FirstEventLedger: 51_000_000, Expected: 10, Present: 0},
		{Partition: 52, FirstEventLedger: 52_000_003, Expected: 10, Present: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("censusShortfalls = %+v, want %+v", got, want)
	}
}
