// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

// This file extends the i128 truncation guard (ADR-0003) to struct
// SHAPE: a float32/float64 struct field named like a money quantity
// (Amount/Rate*/Price*/Reserve*/Supply*) is either an unconverted
// decode/write boundary or an outright precision-loss bug — the
// canonical shape is canonical.Amount (*big.Int) in Go and NUMERIC
// text on the wire, never a float. It is a sibling test in the same
// package, reusing loadRepoPackages / basicKind from
// i128_truncation_guard_test.go rather than a second go/types walk.
//
// What counts as a violation: any struct field (an embedded/anonymous
// field is skipped — it carries no name to judge) whose identifier
// contains "amount", "rate", "price", "reserve", "supply", "cap",
// "balance", "fee" or "tvl" (case-insensitive) and whose type, after
// stripping one pointer level OR one map-value/slice-element level, is
// float32 or float64 — so map[string]float64 and []float64 money
// fields are caught the same as a bare float64 one.
//
// Deliberately NOT matched: bare Pct/APR/Ratio/Confidence/Score/Weight
// names (UtilizationPct, DivergencePct, BorrowAPR, Confidence, …) —
// dimensionless ratios are correctly float64 throughout this repo and
// a stem that caught them would flag the majority of the tree's
// legitimate float fields for no reason. "SupplyAPR" and "*Rate" names
// that are themselves percentages still match the money stems here
// (their name contains "Supply"/"Rate") and rely on the marker below,
// same as the SQL lint's documented stance on `rate` (lint-migrations.sh)
// — the fix for the false-positive is the escape hatch, not a smarter
// stem.
//
// Escape hatch: a `//floatmoney:ok <reason>` comment on the field's
// line (or the line above) exempts it, same contract as i128:ok —
// reasons are mandatory, and a marker that exempts nothing (the field
// it names was fixed or removed) fails the test so the allowlist can
// only shrink.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"strings"
	"testing"
)

// floatMoneyNameRe matches a struct field identifier that looks like a
// money quantity. See the file doc comment for what is deliberately
// excluded.
var floatMoneyNameRe = regexp.MustCompile(`(?i)(amount|rate|price|reserve|supply|cap|balance|fee|tvl)`)

var floatMoneyOkMarker = regexp.MustCompile(`^\s*floatmoney:ok\s+\S+`)

// floatMoneyMarkerLines returns the line numbers in f carrying a
// //floatmoney:ok marker.
func floatMoneyMarkerLines(fset *token.FileSet, f *ast.File) map[int]bool {
	out := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimPrefix(c.Text, "//")
			if floatMoneyOkMarker.MatchString(text) {
				out[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	return out
}

// unwrapOneLevel strips a single pointer, map (by value type) or
// slice/array (by element type) layer off t. ok is false once t is
// none of those, so the caller's loop terminates on the innermost
// type instead of spinning.
func unwrapOneLevel(t types.Type) (elem types.Type, ok bool) {
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return u.Elem(), true
	case *types.Map:
		return u.Elem(), true
	case *types.Slice:
		return u.Elem(), true
	case *types.Array:
		return u.Elem(), true
	default:
		return t, false
	}
}

// checkFloatMoneyField returns a violation message when v is a
// float32/float64 struct field named like a money quantity, else "".
// The field's declared type is unwrapped through any chain of
// pointers, maps (by value type) and slices/arrays (by element type)
// before the float check, so map[string]float64, []float64 and
// map[string]map[string]float64 (a raw decode boundary before
// reshaping) are all caught the same as a bare float64 field.
func checkFloatMoneyField(v *types.Var) string {
	if !v.IsField() || !floatMoneyNameRe.MatchString(v.Name()) {
		return ""
	}
	t := v.Type()
	for depth := 0; depth < 8; depth++ {
		elem, ok := unwrapOneLevel(t)
		if !ok {
			break
		}
		t = elem
	}
	switch basicKind(t) {
	case types.Float32, types.Float64:
	default:
		return ""
	}
	return fmt.Sprintf("field %s is %s and named like a money quantity — money is canonical.Amount (*big.Int) in Go / NUMERIC text over the wire, never a float (ADR-0003)", v.Name(), types.TypeString(v.Type(), nil))
}

// TestFloatMoneyFieldGuard — ADR-0003 extended to struct shape. Every
// float32/float64 field named like Amount/Rate*/Price*/Reserve*/Supply*
// in internal/, cmd/ or pkg/ must be converted or carry a
// //floatmoney:ok marker with a reason.
func TestFloatMoneyFieldGuard(t *testing.T) {
	pkgs := loadRepoPackages(t)

	type site struct {
		pos token.Position
		msg string
	}
	var violations []site
	usedMarkers := map[string]bool{}
	allMarkers := map[string]token.Position{}

	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			filename := pkg.Fset.Position(f.Pos()).Filename
			markers := floatMoneyMarkerLines(pkg.Fset, f)
			for line := range markers {
				allMarkers[fmt.Sprintf("%s:%d", filename, line)] = token.Position{Filename: filename, Line: line}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok || st.Fields == nil {
					return true
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						v, ok := pkg.TypesInfo.Defs[name].(*types.Var)
						if !ok {
							continue
						}
						msg := checkFloatMoneyField(v)
						if msg == "" {
							continue
						}
						pos := pkg.Fset.Position(name.Pos())
						if markers[pos.Line] || markers[pos.Line-1] {
							mLine := pos.Line
							if !markers[mLine] {
								mLine = pos.Line - 1
							}
							usedMarkers[fmt.Sprintf("%s:%d", pos.Filename, mLine)] = true
							continue
						}
						violations = append(violations, site{pos: pos, msg: msg})
					}
				}
				return true
			})
		}
	}

	for _, v := range violations {
		t.Errorf("%s: %s (ADR-0003; annotate with `//floatmoney:ok <reason>` ONLY if genuinely non-monetary or tracked known debt)", v.pos, v.msg)
	}
	for key, pos := range allMarkers {
		if !usedMarkers[key] {
			t.Errorf("%s: stale //floatmoney:ok marker — it exempts no field on its own or the next line; remove it", pos)
		}
	}
}

// TestFloatMoneyFieldGuard_PositiveControl proves the detector still
// FIRES on the exact field shape it exists to catch (same "a guard that
// never fails is decorative" concern the i128 guard's positive control
// documents) — a synthetic, self-contained source so it cannot go stale
// against the real tree.
func TestFloatMoneyFieldGuard_PositiveControl(t *testing.T) {
	const src = `package sink

type Position struct {
	Holders     int
	Amount      float64
	RateUSD     float64
	PriceUSD    float32
	Reserve0    *float64
	SupplyAPR   float64
	MarketCap   float64
	Balance0    float64
	FeeUSD      float64
	PoolTVL     float64
	DayRates    map[string]float64
	History7dRates []float64
	Count       int64
	Weight      float64
	Confidence  float64
	Capacity    int
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sink.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{}).Check("example.test/sink", fset, []*ast.File{f}, info); err != nil {
		t.Fatalf("type-check synthetic source: %v", err)
	}

	fired := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				v, ok := info.Defs[name].(*types.Var)
				if !ok {
					continue
				}
				fired[name.Name] = checkFloatMoneyField(v) != ""
			}
		}
		return true
	})

	mustFire := []string{"Amount", "RateUSD", "PriceUSD", "Reserve0", "SupplyAPR", "MarketCap", "Balance0", "FeeUSD", "PoolTVL", "DayRates", "History7dRates"}
	mustPass := []string{"Holders", "Count", "Weight", "Confidence", "Capacity"}
	for _, k := range mustFire {
		if seen, ok := fired[k]; !ok {
			t.Fatalf("positive-control field %s never inspected — synthetic source drifted", k)
		} else if !seen {
			t.Errorf("DETECTOR ROT: checkFloatMoneyField did NOT fire on float field %s — the float-money guard no longer catches new float money fields", k)
		}
	}
	for _, k := range mustPass {
		if seen, ok := fired[k]; !ok {
			t.Fatalf("control field %s never inspected — synthetic source drifted", k)
		} else if seen {
			t.Errorf("checkFloatMoneyField FALSE-fired on non-money field %s", k)
		}
	}
}
