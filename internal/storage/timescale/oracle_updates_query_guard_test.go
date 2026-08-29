// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOracleUpdatesQueriesDeclareRawRowPolicy is the repo guard from
// the oracle capture-totality design (§4): the record layer keeps
// `raw:<symbol>` rows for oracle symbols that map to no canonical
// asset, so EVERY query over oracle_updates under internal/ must say
// what it does with them. A query passes when it is
//
//   - keyed by exact asset string (`asset = $n` / `asset = ANY(`) — a
//     raw row can never match a mapped key;
//   - explicitly excluding them (`asset NOT LIKE 'raw:%'`) — the
//     interpretation-layer posture (MEV scan);
//   - or marked `-- totality: includes unmapped` — a deliberate
//     record-layer read (counts, rosters, the streams catalogue).
//
// The liquidation-cascade correlator was the class this guards
// against: an unkeyed scan for which any row in a ledger bracket is
// evidence. An unlabelled query is the bug recurring.
func TestOracleUpdatesQueriesDeclareRawRowPolicy(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..")) // internal/
	var (
		keyed    = regexp.MustCompile(`\basset\s*=\s*(\$\d+|ANY\s*\()`)
		excluded = "asset NOT LIKE 'raw:%'"
		marker   = "-- totality: includes unmapped"
		from     = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+oracle_updates\b`)
	)
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !from.MatchString(lit.Value) {
				return true
			}
			checked++
			if keyed.MatchString(lit.Value) || strings.Contains(lit.Value, excluded) || strings.Contains(lit.Value, marker) {
				return true
			}
			t.Errorf("%s: oracle_updates query is neither asset-keyed, nor `%s`, nor marked `%s`:\n%s",
				fset.Position(lit.Pos()), excluded, marker, lit.Value)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if checked == 0 {
		t.Fatal("found no oracle_updates queries under internal/ — the guard did not run")
	}
	t.Logf("checked %d oracle_updates query literal(s) under internal/", checked)
}
