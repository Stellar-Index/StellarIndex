package completeness

import (
	"errors"
	"reflect"
	"testing"
)

func TestBlindSpotsMerge(t *testing.T) {
	a := BlindSpots{Ledgers: []uint32{5, 9}, UndecodableMatched: 2}
	b := BlindSpots{Ledgers: []uint32{3, 9}, UndecodableMatched: 1, Unreconstructable: 1}
	got := a.Merge(b)
	want := BlindSpots{Ledgers: []uint32{3, 5, 9}, UndecodableMatched: 3, Unreconstructable: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %+v, want %+v", got, want)
	}
	if !(BlindSpots{}).Merge(a).Any() || (BlindSpots{}).Merge(BlindSpots{}).Any() {
		t.Fatal("Merge with an empty side must preserve Any()")
	}
}

func TestGuardRecoversPanic(t *testing.T) {
	if err := Guard(func() {}); err != nil {
		t.Fatalf("Guard(no panic) = %v, want nil", err)
	}
	err := Guard(func() { panic(errors.New("boom")) })
	if err == nil || err.Error() != "recovered: boom" {
		t.Fatalf("Guard(panic) = %v, want recovered: boom", err)
	}
}
