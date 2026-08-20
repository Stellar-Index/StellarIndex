package pgarray

import (
	"reflect"
	"testing"
)

// TestStringsScan feeds the exact Postgres TEXT-array representation that the
// pgx stdlib driver hands to database/sql for a text[]/varchar[] column and
// asserts the decoded []string. This is the behaviour that a plain *[]string
// scan destination CANNOT provide under pgx (it fails with "unsupported Scan,
// storing driver.Value type string into type *[]string") — the whole reason
// this adapter exists.
func TestStringsScan(t *testing.T) {
	cases := []struct {
		name string
		src  any
		want []string
	}{
		{"simple", "{alpha,beta,gamma}", []string{"alpha", "beta", "gamma"}},
		{"quoted comma", `{"a,b",c}`, []string{"a,b", "c"}},
		{"embedded quote+backslash", `{"d\"e","f\\g"}`, []string{`d"e`, `f\g`}},
		{"empty string element", `{"",x}`, []string{"", "x"}},
		{"bytes source", []byte("{one,two}"), []string{"one", "two"}},
		{"empty array is non-nil", "{}", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			if err := Strings(&got).Scan(tc.src); err != nil {
				t.Fatalf("Scan(%v) error: %v", tc.src, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Scan(%v) = %#v; want %#v", tc.src, got, tc.want)
			}
			if tc.name == "empty array is non-nil" && got == nil {
				t.Fatalf("empty array must decode to a non-nil slice (matches pq.StringArray)")
			}
		})
	}
}

// TestStringsScanNULL: a SQL NULL (src == nil) decodes to a nil slice.
func TestStringsScanNULL(t *testing.T) {
	got := []string{"stale"}
	if err := Strings(&got).Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if got != nil {
		t.Fatalf("Scan(nil) = %#v; want nil slice", got)
	}
}

// TestByteaScan feeds the Postgres TEXT representation of a bytea[] column
// (each element hex-escaped as \x…) and asserts the decoded [][]byte,
// including an empty element and high/zero bytes.
func TestByteaScan(t *testing.T) {
	src := `{"\\x0102","\\x","\\xff007f"}`
	want := [][]byte{{0x01, 0x02}, {}, {0xff, 0x00, 0x7f}}
	var got [][]byte
	if err := Bytea(&got).Scan(src); err != nil {
		t.Fatalf("Scan(%q) error: %v", src, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan(%q) = %#v; want %#v", src, got, want)
	}
}

func TestByteaScanNULL(t *testing.T) {
	got := [][]byte{{9}}
	if err := Bytea(&got).Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if got != nil {
		t.Fatalf("Scan(nil) = %#v; want nil slice", got)
	}
}
