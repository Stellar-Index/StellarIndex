package diagnostics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// verify-decoders and verify-external print how many members were
// silent; the exit status must carry the same finding. 9/9 silent used
// to exit 0.
func TestSilentVerdict(t *testing.T) {
	cases := []struct {
		name          string
		silent, total int
		failOnSilent  bool
		wantErr       bool
	}{
		{"every member silent, flag off", 9, 9, false, true},
		{"every member silent, flag on", 6, 6, true, true},
		{"nothing registered", 0, 0, false, true},
		{"some silent, flag on", 5, 6, true, true},
		{"some silent, flag off", 2, 9, false, false},
		{"none silent, flag on", 0, 6, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := silentVerdict("verify-x", "members", tc.silent, tc.total, tc.failOnSilent)
			if (err != nil) != tc.wantErr {
				t.Fatalf("silentVerdict(silent=%d, total=%d, failOnSilent=%v) = %v; wantErr %v",
					tc.silent, tc.total, tc.failOnSilent, err, tc.wantErr)
			}
		})
	}
}

// The handlers need a live datastore or live vendors, so pin the wiring
// instead: each must END by returning silentVerdict, so no later edit can
// fall back to an unconditional `return nil` after the table.
func TestVerifyHandlersReturnSilentVerdict(t *testing.T) {
	for file, fn := range map[string]string{
		"verify_decoders.go": "verifyDecoders",
		"verify_external.go": "verifyExternal",
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				body = fd.Body
			}
		}
		if body == nil || len(body.List) == 0 {
			t.Fatalf("%s: func %s not found", file, fn)
		}
		if !returnsCallTo(body.List[len(body.List)-1], "silentVerdict") {
			t.Errorf("%s: %s does not end with `return silentVerdict(...)`; its exit code ignores the silent count", file, fn)
		}
	}
}

func returnsCallTo(stmt ast.Stmt, callee string) bool {
	ret, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == callee
}
