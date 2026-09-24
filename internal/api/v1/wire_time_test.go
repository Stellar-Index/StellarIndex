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
// endpoints it lists. This one sees every struct in every package under
// internal/api — the v1 handlers, their sub-packages and the SSE
// producers — including ones nobody has written a test for yet: it
// parses the source and fails if any json-tagged field is a raw
// time.Time. Such a field renders in whatever location its value happens
// to carry, which is precisely how /v1/price/at and
// /v1/history/since-inception came to serve `+02:00` and `+01:00`.
//
// The package set is walked, not listed, so a new package that marshals
// to a client is covered the day it is created. A struct that is not on
// the wire, or a known offender, is named in wireTimeExclusions.
func TestWireTime_NoRawTimeOnTheWire(t *testing.T) {
	fset := token.NewFileSet()
	byDir := parseAPITree(t, fset)

	files, fieldsChecked := 0, 0
	excludedSeen := map[string]bool{}
	for dir, dirFiles := range byDir {
		for _, file := range dirFiles {
			files++
			checked, raw := rawTimeWireFields(fset, dir, file)
			fieldsChecked += checked
			for _, f := range raw {
				if _, ok := wireTimeExclusions[f.key]; ok {
					excludedSeen[f.key] = true
					continue
				}
				t.Errorf("json-tagged time.Time on the v1 wire (use WireTime): %s: %s field %s",
					f.pos, f.key, f.name)
			}
		}
	}
	if files == 0 || fieldsChecked == 0 {
		t.Fatalf("scanned %d files / %d json-tagged fields — the scan would pass vacuously",
			files, fieldsChecked)
	}
	for key, why := range wireTimeExclusions {
		if !excludedSeen[key] {
			t.Errorf("wireTimeExclusions entry %s (%s) no longer has a raw time.Time field — delete it "+
				"so the list only shrinks", key, why)
		}
	}
}

// wireTimeExclusions names, as "<dir under internal/api>.<TypeName>", the
// structs TestWireTime_NoRawTimeOnTheWire tolerates.
var wireTimeExclusions = map[string]string{
	"streaming/redispub.ClosedBucketEvent": "aggregator-to-API Redis message; the subscriber " +
		"validates it and re-marshals the client frame, so this struct never reaches a client",

	// Known offenders: these reach a client as raw time.Time. The packages
	// cannot import WireTime (v1's tests import redispub; the dashboard
	// packages sit beside v1), so fixing them means moving WireTime to a
	// leaf package. Remove each entry as it is fixed; never add one.
	"streaming/redispub.closedBucketEnvelope": "known offender: /v1/price/stream frame as_of",
	"streaming/redispub.closedBucketWireData": "known offender: /v1/price/stream frame observed_at",
	"v1/dashboardkeys.keyDTO":                 "known offender: Postgres-sourced key timestamps",
	"v1/dashboardwebhooks.webhookDTO":         "known offender: Postgres-sourced webhook timestamps",
	"v1/dashboardwebhooks.deliveryDTO":        "known offender: Postgres-sourced delivery timestamps",
	"v1/dashboardpricealerts.priceAlertDTO":   "known offender: Postgres-sourced alert timestamps",
}

// parseAPITree parses the non-test sources of every package under
// internal/api, keyed by directory relative to it ("v1", "streampublish",
// "v1/dashboardkeys", …).
func parseAPITree(t *testing.T, fset *token.FileSet) map[string][]*ast.File {
	t.Helper()
	root := ".."
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", dir, err)
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				byDir[filepath.ToSlash(rel)] = append(byDir[filepath.ToSlash(rel)], file)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api: %v", err)
	}
	// The two packages the scan has always covered must still be found.
	for _, must := range []string{"v1", "streampublish"} {
		if len(byDir[must]) == 0 {
			t.Fatalf("walk found no sources in internal/api/%s — the scan would pass vacuously", must)
		}
	}
	return byDir
}

type rawWireTimeField struct{ key, pos, name string }

// rawTimeWireFields counts file's json-tagged struct fields and returns the
// raw time.Time ones, keyed "<dir>.<enclosing type>" (anonymous structs
// outside a type declaration key as "<dir>.<anonymous>").
func rawTimeWireFields(fset *token.FileSet, dir string, file *ast.File) (checked int, raw []rawWireTimeField) {
	owner := map[*ast.StructType]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		ast.Inspect(ts.Type, func(m ast.Node) bool {
			if st, ok := m.(*ast.StructType); ok {
				owner[st] = ts.Name.Name
			}
			return true
		})
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		typeName := owner[st]
		if typeName == "" {
			typeName = "<anonymous>"
		}
		for _, f := range st.Fields.List {
			if !hasJSONTag(f) {
				continue
			}
			checked++
			if !isRawTimeTime(f.Type) {
				continue
			}
			name := "<embedded>"
			if len(f.Names) > 0 {
				name = f.Names[0].Name
			}
			raw = append(raw, rawWireTimeField{
				key: dir + "." + typeName, pos: fset.Position(f.Pos()).String(), name: name,
			})
		}
		return true
	})
	return checked, raw
}

func hasJSONTag(f *ast.Field) bool {
	if f.Tag == nil {
		return false
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return false
	}
	_, ok := reflect.StructTag(tag).Lookup("json")
	return ok
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
	checked, raw := rawTimeWireFields(fset, "p", file)
	var hits []string
	for _, f := range raw {
		hits = append(hits, f.key+"."+f.name)
	}
	// The key names the enclosing type: wireTimeExclusions matches on it.
	if got, want := strings.Join(hits, ","), "p.Bad.C,p.Bad.D"; got != want {
		t.Errorf("planted scan found %q, want %q", got, want)
	}
	if checked != 3 {
		t.Errorf("planted scan checked %d json-tagged fields, want 3", checked)
	}
}
