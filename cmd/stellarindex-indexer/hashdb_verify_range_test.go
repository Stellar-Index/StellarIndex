package main

import "testing"

// TestVerifyHashDBRangeFlags_Validate pins the -verify-hashdb-from/-to
// contract. Neither set leaves full ingest unchanged; a range far behind the
// live tip must pass through untouched (the CLI path is decoupled from
// hashDBVerifySweep's trailing window); a half-set or inverted range must
// fail loudly with requested=true so the caller surfaces the error instead of
// silently ingesting.
func TestVerifyHashDBRangeFlags_Validate(t *testing.T) {
	cases := []struct {
		name          string
		flags         verifyHashDBRangeFlags
		wantFrom      uint32
		wantTo        uint32
		wantRequested bool
		wantErr       bool
	}{
		{name: "neither set is not requested"},
		{name: "explicit older range is accepted", flags: verifyHashDBRangeFlags{from: 1_000_000, to: 1_000_500}, wantFrom: 1_000_000, wantTo: 1_000_500, wantRequested: true},
		{name: "only from set is rejected", flags: verifyHashDBRangeFlags{from: 100}, wantRequested: true, wantErr: true},
		{name: "only to set is rejected", flags: verifyHashDBRangeFlags{to: 100}, wantRequested: true, wantErr: true},
		{name: "from after to is rejected", flags: verifyHashDBRangeFlags{from: 200, to: 100}, wantRequested: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, requested, err := tc.flags.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate(%+v) err = %v, wantErr %v", tc.flags, err, tc.wantErr)
			}
			if requested != tc.wantRequested {
				t.Fatalf("validate(%+v) requested = %v, want %v", tc.flags, requested, tc.wantRequested)
			}
			if !tc.wantErr && (from != tc.wantFrom || to != tc.wantTo) {
				t.Fatalf("validate(%+v) = (%d, %d), want (%d, %d)", tc.flags, from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}
