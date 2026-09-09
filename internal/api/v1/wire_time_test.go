// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWireTime_MarshalsUTCRegardlessOfLocation is the unit-level
// contract: the SAME INSTANT renders identically no matter which
// location the value carries. This is what a plain time.Time field
// does not give you.
func TestWireTime_MarshalsUTCRegardlessOfLocation(t *testing.T) {
	instant := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("CEST", 2*60*60),
		time.FixedZone("CET", 1*60*60),
		time.FixedZone("NEG", -5*60*60),
	}
	const want = `"2026-06-01T10:00:00Z"`
	for _, loc := range zones {
		got, err := json.Marshal(WireTime(instant.In(loc)))
		if err != nil {
			t.Fatalf("marshal in %s: %v", loc, err)
		}
		if string(got) != want {
			t.Errorf("marshal in %s = %s, want %s", loc, got, want)
		}
	}

	// The two production leaks, verbatim.
	priceAt := time.Date(2026, 6, 1, 2, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	if got, _ := json.Marshal(WireTime(priceAt)); string(got) != `"2026-06-01T00:00:00Z"` {
		t.Errorf("price/at observed_at = %s, want 2026-06-01T00:00:00Z", got)
	}
	sinceInception := time.Date(2017, 1, 17, 1, 0, 0, 0, time.FixedZone("CET", 1*60*60))
	if got, _ := json.Marshal(WireTime(sinceInception)); string(got) != `"2017-01-17T00:00:00Z"` {
		t.Errorf("since-inception t = %s, want 2017-01-17T00:00:00Z", got)
	}
}

// TestWireTime_MatchesTimeTimeForUTCValues pins the compatibility
// claim that made adopting this type safe on 40-odd already-correct
// fields: for a value that was ALREADY UTC, the bytes are unchanged.
// Adopting WireTime is therefore not a response-shape change.
func TestWireTime_MatchesTimeTimeForUTCValues(t *testing.T) {
	for _, instant := range []time.Time{
		time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 10, 0, 0, 123456789, time.UTC),
		time.Date(2026, 6, 1, 10, 0, 0, 500000000, time.UTC),
		time.Unix(0, 0).UTC(),
	} {
		std, err := json.Marshal(instant)
		if err != nil {
			t.Fatalf("marshal time.Time: %v", err)
		}
		wire, err := json.Marshal(WireTime(instant))
		if err != nil {
			t.Fatalf("marshal WireTime: %v", err)
		}
		if string(std) != string(wire) {
			t.Errorf("UTC %s: time.Time => %s, WireTime => %s", instant, std, wire)
		}
	}
}

// TestWireTime_RoundTrips keeps clients (and this package's own
// decode-side tests) able to read what the server writes.
func TestWireTime_RoundTrips(t *testing.T) {
	in := WireTime(time.Date(2026, 6, 1, 10, 0, 0, 250000000, time.FixedZone("X", 3*60*60)))
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WireTime
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if !out.Time().Equal(in.Time()) {
		t.Errorf("round-trip = %s, want %s", out, in)
	}
	if out.Time().Location() != time.UTC {
		t.Errorf("round-trip location = %s, want UTC", out.Time().Location())
	}

	var null WireTime
	if err := json.Unmarshal([]byte("null"), &null); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if !null.IsZero() {
		t.Errorf("null decoded to %s, want the zero time", null)
	}
}

// TestWireTime_OutOfRangeYearRendersNull pins the one place this type
// deliberately diverges from time.Time: a year RFC 3339 cannot express
// degrades one field to null instead of failing the whole response.
func TestWireTime_OutOfRangeYearRendersNull(t *testing.T) {
	if _, err := json.Marshal(time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("precondition: time.Time is expected to reject year -1")
	}
	got, err := json.Marshal(WireTime(time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("WireTime must not fail the response: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("out-of-range year = %s, want null", got)
	}
}

// TestWireTime_NoRawTimeOnTheWire is the structural ratchet.
//
// The payload scan in wire_time_payload_test.go can only see the
// endpoints it lists. This one sees every response struct in the
// package, including ones nobody has written a test for yet: it
// parses the package's own source and fails if any json-tagged field
// is a raw time.Time. Such a field renders in whatever location its
// value happens to carry, which is precisely how /v1/price/at and
// /v1/history/since-inception came to serve `+02:00` and `+01:00`.
//
// A new endpoint that reaches for time.Time on the wire fails HERE,
// at the point the field is declared, rather than in production.
func TestWireTime_NoRawTimeOnTheWire(t *testing.T) {
	// Both directories that marshal JSON to a v1 client: the handlers,
	// and the SSE producer whose payload is documented as field-
	// compatible with /v1/price. The producer is a separate package and
	// was leaking the same way — an envelope built there is no less on
	// the wire for living next door.
	dirs := []string{".", filepath.Join("..", "streampublish")}

	fset := token.NewFileSet()
	var pkgFiles []*ast.File
	for _, dir := range dirs {
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		if len(pkgs) == 0 {
			t.Fatalf("parsed no packages in %s — the scan would pass vacuously", dir)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				pkgFiles = append(pkgFiles, file)
			}
		}
	}

	var offenders []string
	files := 0
	fieldsChecked := 0
	for _, file := range pkgFiles {
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				if f.Tag == nil {
					continue
				}
				tag, err := strconv.Unquote(f.Tag.Value)
				if err != nil {
					continue
				}
				if _, ok := reflect.StructTag(tag).Lookup("json"); !ok {
					continue
				}
				fieldsChecked++
				if !isRawTimeTime(f.Type) {
					continue
				}
				name := "<embedded>"
				if len(f.Names) > 0 {
					name = f.Names[0].Name
				}
				offenders = append(offenders, fmt.Sprintf("%s: field %s",
					fset.Position(f.Pos()), name))
			}
			return true
		})
	}
	if files == 0 || fieldsChecked == 0 {
		t.Fatalf("scanned %d files / %d json-tagged fields — the scan would pass vacuously",
			files, fieldsChecked)
	}
	for _, o := range offenders {
		t.Errorf("json-tagged time.Time on the v1 wire (use WireTime): %s", o)
	}
}

// isRawTimeTime reports whether an AST type expression is `time.Time`
// or `*time.Time`.
func isRawTimeTime(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Time" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time"
}

// TestWireTime_ScanDetectsAPlantedOffender proves the AST scan can
// fail — the same anti-vacuity check the payload scanner gets.
func TestWireTime_ScanDetectsAPlantedOffender(t *testing.T) {
	const src = `package p
import "time"
type Good struct {
	A WireTime  ` + "`json:\"a\"`" + `
	B time.Time // untagged: not on the wire
}
type Bad struct {
	C time.Time  ` + "`json:\"c\"`" + `
	D *time.Time ` + "`json:\"d\"`" + `
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "planted.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			tag, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				continue
			}
			if _, ok := reflect.StructTag(tag).Lookup("json"); !ok {
				continue
			}
			if isRawTimeTime(f.Type) && len(f.Names) > 0 {
				hits = append(hits, f.Names[0].Name)
			}
		}
		return true
	})
	if got, want := strings.Join(hits, ","), "C,D"; got != want {
		t.Errorf("planted scan found %q, want %q", got, want)
	}
}
