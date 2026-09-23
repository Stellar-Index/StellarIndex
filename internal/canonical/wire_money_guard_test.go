package canonical

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// moneyWireName matches a JSON field name that carries a stroop- or
// token-denominated amount. It is matched per underscore-separated word.
var moneyWireName = regexp.MustCompile(`^(fee|fees|reserve|reserves|amount|amounts|balance|balances|stroops|coins)$`)

// numericGoType is a Go type that marshals to a JSON number.
var numericGoType = regexp.MustCompile(`^\*?(u?int(8|16|32|64)?|float(32|64))$`)

// wireMoneyViolations reports every struct field in src whose JSON name
// is monetary, whose type marshals to a JSON number, and whose tag does
// not force a string.
func wireMoneyViolations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" || strings.Contains(","+opts+",", ",string,") {
				continue
			}
			if !numericGoType.MatchString(exprString(field.Type)) || !monetaryName(name) {
				continue
			}
			out = append(out, fset.Position(field.Pos()).String()+" json:"+name)
		}
		return true
	})
	return out
}

// notAnAmount marks a name whose unit is a count or a rate, not money
// (balance_entries, fee_bps).
var notAnAmount = regexp.MustCompile(`^(bps|count|entries|pct|ratio|rate)$`)

func monetaryName(name string) bool {
	money := false
	for _, w := range strings.Split(name, "_") {
		if notAnAmount.MatchString(w) {
			return false
		}
		money = money || moneyWireName.MatchString(w)
	}
	return money
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	}
	return ""
}

// ADR-0003: a served amount is a JSON string, never a JSON number. The
// i128 guard sees only XDR word conversions; this one sees the wire.
func TestServedMoneyIsNeverAJSONNumber(t *testing.T) {
	root := filepath.Join(repoRoot(), "internal", "api")
	fset := token.NewFileSet()
	scanned := 0
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		violations = append(violations, wireMoneyViolations(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s", root)
	}
	for _, v := range violations {
		t.Errorf("%s: monetary field marshals as a JSON number; serve a decimal string", v)
	}
	t.Logf("scanned %d files under %s", scanned, root)
}

func TestServedMoneyGuard_PositiveControl(t *testing.T) {
	const src = `package p
type V struct {
	FeeCharged  int64  ` + "`json:\"fee_charged\"`" + `
	BaseReserve uint32 ` + "`json:\"base_reserve\"`" + `
	TotalCoins  string ` + "`json:\"total_coins\"`" + `
	MaxFee      int64  ` + "`json:\"max_fee,string\"`" + `
	TxCount     uint32 ` + "`json:\"tx_count\"`" + `
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := wireMoneyViolations(fset, f)
	if len(got) != 2 || !strings.HasSuffix(got[0], "json:fee_charged") || !strings.HasSuffix(got[1], "json:base_reserve") {
		t.Fatalf("violations = %v, want exactly fee_charged and base_reserve", got)
	}
}
