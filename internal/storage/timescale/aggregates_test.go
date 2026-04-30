package timescale

import (
	"reflect"
	"testing"
)

// TestStringArray_Scan covers the Postgres TEXT[] decoder used to
// scan the prices_1m `sources` column. Postgres serialises arrays
// in the text protocol as `{a,b,c}`; we don't pull pgx's full array
// decoder for this one column.
func TestStringArray_Scan(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"single-source", []byte("{soroswap}"), []string{"soroswap"}},
		{"multi-source", []byte("{soroswap,aquarius,phoenix}"), []string{"soroswap", "aquarius", "phoenix"}},
		{"with-hyphens", []byte("{reflector-dex,reflector-cex}"), []string{"reflector-dex", "reflector-cex"}},
		{"empty-array", []byte("{}"), []string{}},
		{"string-not-bytes", "{binance,kraken}", []string{"binance", "kraken"}},
		{"nil-source", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got stringArray
			if err := got.Scan(tc.in); err != nil {
				t.Fatalf("Scan(%v) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual([]string(got), tc.want) {
				t.Errorf("Scan(%v) = %v, want %v", tc.in, []string(got), tc.want)
			}
		})
	}
}

// TestStringArray_Scan_Errors covers the malformed-input + bad-type
// guards. We want a clear error rather than silent corruption when
// the database driver ever sends us a shape we don't expect (e.g.
// after a Postgres-side schema change).
func TestStringArray_Scan_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"unwrapped", []byte("a,b,c")},
		{"missing-open", []byte("a,b,c}")},
		{"missing-close", []byte("{a,b,c")},
		{"too-short", []byte("{")},
		{"unsupported-type", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got stringArray
			if err := got.Scan(tc.in); err == nil {
				t.Errorf("Scan(%v) expected error, got nil; result=%v", tc.in, []string(got))
			}
		})
	}
}

// TestStringArray_Scan_NullElement covers the (currently
// non-occurring) NULL-element case. array_agg(DISTINCT source) over
// the trades.source column won't ever produce NULL elements because
// the column is NOT NULL, but the parser still tolerates the
// literal "NULL" defensively — silently dropping it rather than
// emitting it as a string with the value "NULL".
func TestStringArray_Scan_NullElement(t *testing.T) {
	var got stringArray
	if err := got.Scan([]byte("{a,NULL,b}")); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"a", "b"}
	if !reflect.DeepEqual([]string(got), want) {
		t.Errorf("got %v, want %v (NULL element should be skipped)", []string(got), want)
	}
}

func TestNormalizeVwapSources(t *testing.T) {
	row := Vwap1mRow{Sources: []string{"soroswap", "aquarius", "band"}}

	normalizeVwapSources(&row)

	want := []string{"aquarius", "band", "soroswap"}
	if !reflect.DeepEqual(row.Sources, want) {
		t.Fatalf("sources = %v, want %v", row.Sources, want)
	}
}

func TestNormalizeVwapSources_ShortSlicesUntouched(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"sdex"},
	}
	for _, tc := range cases {
		row := Vwap1mRow{Sources: tc}
		normalizeVwapSources(&row)
		if !reflect.DeepEqual(row.Sources, tc) {
			t.Fatalf("sources = %v, want %v", row.Sources, tc)
		}
	}
}
