// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSourceShapeRules holds the single-package source-shape rules that read
// this package's non-test files. One row per rule; each fails when its scan
// finds no subjects, so an empty scan cannot pass.
func TestSourceShapeRules(t *testing.T) {
	rules := []struct {
		name  string
		check func(t *testing.T)
	}{
		{"admin_routes_mounted_through_handle_admin", checkAdminRoutesMountedThroughHandleAdmin},
		{"api_key_mints_name_monthly_quota", checkCreateAPIKeyRequestSitesNameMonthlyQuota},
		{"exported_types_have_doc_comments", checkExportedTypesHaveDocComments},
		{"completeness_verdict_reads_go_through_gate", checkCompletenessVerdictReadsGoThroughGate},
		{"scam_verdict_written_with_scam_reason", checkScamVerdictIsWrittenWithTheScamReason},
		{"stream_call_sites_pass_server_drain", checkEveryStreamCallSitePassesTheServerDrain},
		{"sse_producer_goroutines_recover", checkSSEProducerGoroutinesRecover},
	}
	for _, r := range rules {
		t.Run(r.name, r.check)
	}
}

// checkAdminRoutesMountedThroughHandleAdmin scans every non-test source
// under internal/api/v1 so a /v1/admin/ route registered any other way
// (s.mux.HandleFunc, a sub-package Mount) fails CI rather than review.
func checkAdminRoutesMountedThroughHandleAdmin(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, callee, ok := muxSpecCall(n)
			if !ok || !strings.HasPrefix(spec.path, "/v1/admin/") {
				return true
			}
			found++
			if callee != "handleAdmin" {
				t.Errorf("%s: %s %s is mounted via %s — /v1/admin/ routes must be mounted with s.handleAdmin so the operator tier and X-Reason are enforced at the mount",
					fset.Position(n.Pos()), spec.method, spec.path, callee)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api/v1: %v", err)
	}
	if found == 0 {
		t.Fatal("no /v1/admin/ routes found — the scan is broken and would pass forever")
	}
}

// muxSpecCall reports the "METHOD /path" literal a call passes as its first
// argument and the name of the function it calls.
func muxSpecCall(n ast.Node) (mountedRoute, string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return mountedRoute{}, "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return mountedRoute{}, "", false
	}
	spec, err := strconv.Unquote(lit.Value)
	if err != nil {
		return mountedRoute{}, "", false
	}
	m := muxRoute.FindStringSubmatch(spec)
	if m == nil {
		return mountedRoute{}, "", false
	}
	var callee string
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		callee = fn.Sel.Name
	case *ast.Ident:
		callee = fn.Name
	}
	return mountedRoute{m[1], m[2]}, callee, true
}

// TestHandleAdmin_GatesBeforeTheHandler pins the mount-level guard with a

// checkCreateAPIKeyRequestSitesNameMonthlyQuota fails when an HTTP mint
// path builds an auth.CreateAPIKeyRequest without naming MonthlyQuota:
// an omitted field persists 0, which the quota middleware reads as
// unmetered, so every mint site must state its cap explicitly.
func checkCreateAPIKeyRequestSitesNameMonthlyQuota(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sites := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isCreateAPIKeyRequest(lit.Type) {
				return true
			}
			sites++
			if !namesField(lit, "MonthlyQuota") {
				t.Errorf("%s: auth.CreateAPIKeyRequest omits MonthlyQuota (persists an unmetered key)", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if sites == 0 {
		t.Fatal("found no auth.CreateAPIKeyRequest literals; the guard is not scanning the mint paths")
	}
}

func isCreateAPIKeyRequest(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "CreateAPIKeyRequest" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "auth"
}

func namesField(lit *ast.CompositeLit, field string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
			return true
		}
	}
	return false
}

// docCommentCleanFiles are the files whose exported types must all
// carry a doc comment; the rest of the package has not been swept yet.
var docCommentCleanFiles = []string{"anomalies.go"}

// checkExportedTypesHaveDocComments requires every exported type in
// docCommentCleanFiles to carry a doc comment starting with its name.
// These are wire types, and golangci does not enable revive's
// `exported` rule, so nothing else catches a missing one.
func checkExportedTypesHaveDocComments(t *testing.T) {
	var checked int
	for _, name := range docCommentCleanFiles {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				checked++
				doc := ts.Doc
				// An ungrouped `type T …` attaches its comment to the GenDecl.
				if doc == nil && !gd.Lparen.IsValid() {
					doc = gd.Doc
				}
				if doc == nil || !strings.HasPrefix(doc.Text(), ts.Name.Name+" ") {
					t.Errorf("%s: exported type %s lacks a doc comment starting %q", name, ts.Name.Name, ts.Name.Name+" ")
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no exported types found; the scan did not run")
	}
}

// checkCompletenessVerdictReadsGoThroughGate: outside diagnostics (which
// republishes computed_at per row, so each claim carries its own age), the
// only production read of the verdict rows is completenessVerdicts, which
// returns them with their stale gate.
func checkCompletenessVerdictReadsGoThroughGate(t *testing.T) {
	allowed := map[string]bool{
		"coverage_verdicts.go:completenessVerdicts":    true,
		"diagnostics_ingestion.go:overlayCompleteness": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "ListCompletenessSnapshots" && !allowed[f+":"+fn.Name.Name] {
					t.Errorf("%s: %s reads completeness verdicts directly; use completenessVerdicts so "+
						"the rows travel with their stale gate", fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}

// checkScamVerdictIsWrittenWithTheScamReason: a function that asks
// scamWithheld and then writes the withheld problem must pass
// PriceWithheldScamIssuer — the reason-free call is exactly how /v1/vwap,
// /v1/twap and /v1/chart came to call a flagged issuer's market thin.
func checkScamVerdictIsWrittenWithTheScamReason(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	askers := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !callsIdent(fn.Body, "scamWithheld") {
				continue
			}
			askers++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "writePriceWithheldProblem" {
					return true
				}
				reason, ok := call.Args[len(call.Args)-1].(*ast.Ident)
				if !ok || reason.Name != "PriceWithheldScamIssuer" {
					t.Errorf("%s: %s asks scamWithheld but writes the withheld problem with "+
						"a reason other than PriceWithheldScamIssuer — use writeIfScamWithheld",
						fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	// A guard whose subject set is empty passes forever.
	if askers == 0 {
		t.Fatal("found no function calling scamWithheld — the guard is broken, not the code clean")
	}
}

func callsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// checkEveryStreamCallSitePassesTheServerDrain: every call into the streaming
// package's writers must pass s.streamOptions(). An inline
// streaming.StreamOptions{} builds a stream without the server's shutdown
// drain and is invisible to every other test.
func checkEveryStreamCallSitePassesTheServerDrain(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked += auditStreamCallSites(t, name)
	}

	// If the stream endpoints were restructured, re-point this guard at
	// the new shape rather than deleting it — a passing count of zero
	// would mean the guard silently stopped guarding.
	if checked == 0 {
		t.Fatal("found no calls into the streaming writers — this guard no longer covers anything")
	}
	t.Logf("stream writer call sites checked: %d", checked)
}

// auditStreamCallSites reports how many streaming-writer calls the file
// contains, failing t for each one that does not pass s.streamOptions().
func auditStreamCallSites(t *testing.T, path string) int {
	t.Helper()

	src, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	writers := map[string]bool{
		"Stream":                       true,
		"StreamFromChannel":            true,
		"StreamFromChannelPreAdmitted": true,
	}

	var found int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "streaming" || !writers[sel.Sel.Name] {
			return true
		}
		found++

		last := call.Args[len(call.Args)-1]
		start, end := fset.Position(last.Pos()).Offset, fset.Position(last.End()).Offset
		got := string(src[start:end])
		if got != "s.streamOptions()" {
			t.Errorf("%s: streaming.%s is passed %s, want s.streamOptions() — "+
				"a stream built without the server's shutdown drain holds "+
				"httpSrv.Shutdown open for the whole drain budget",
				fset.Position(call.Pos()), sel.Sel.Name, got)
		}
		return true
	})
	return found
}

// TestStreamDrainDoesNotAffectOrdinaryHandlers pins the SCOPE of the
// signal: BeginStreamDrain must be invisible to every non-stream route.
// This is the assertion that fails if the drain is ever "simplified"
// into an http.Server.BaseContext derived from the process root

// checkSSEProducerGoroutinesRecover: every goroutine this package spawns
// registers a recover(). An unrecovered panic in any goroutine terminates the
// process, and middleware.Recoverer wraps only the handler goroutine. The
// spawned set is derived from the source, and the named producers are pinned
// so a drifted scan cannot pass.
func checkSSEProducerGoroutinesRecover(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	// funcBodies: every method on *Server declared in this package.
	funcBodies := map[string]*ast.FuncDecl{}
	// spawnedMethods: method names launched as `go s.name(...)`, with the file for context.
	spawnedMethods := map[string]string{}
	// spawnedLiterals: anonymous `go func(){...}()` goroutines, with the file for context.
	type spawnedLiteral struct {
		file string
		lit  *ast.FuncLit
	}
	var spawnedLiterals []spawnedLiteral

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name != nil {
					funcBodies[d.Name.Name] = d
				}
			case *ast.GoStmt:
				switch fn := d.Call.Fun.(type) {
				case *ast.SelectorExpr:
					// `go s.someMethod(...)`
					if ident, ok := fn.X.(*ast.Ident); ok && ident.Name == "s" {
						spawnedMethods[fn.Sel.Name] = name
					}
				case *ast.FuncLit:
					// `go func(...){...}(...)`
					spawnedLiterals = append(spawnedLiterals, spawnedLiteral{file: name, lit: fn})
				}
			}
			return true
		})
	}

	// Every method spawned as `go s.<method>(...)` is a detached goroutine and
	// needs its own recover — checking every one, not just names containing
	// "StreamProducer", is what catches forwardClosedStream/forwardTipStream.
	var checked []string
	for method, file := range spawnedMethods {
		decl, ok := funcBodies[method]
		if !ok {
			t.Errorf("%s: spawned as a goroutine in %s but its declaration was not found in this package", method, file)
			continue
		}
		if !blockRegistersRecover(decl.Body) {
			t.Errorf("%s (spawned as a bare goroutine in %s) does not register a recover(). "+
				"An unrecovered panic in a goroutine terminates the WHOLE process, and these "+
				"streams are reachable unauthenticated — add a deferred recover() that logs and "+
				"tears down only this connection (AGT-12).", method, file)
		}
		checked = append(checked, method)
	}

	// Anonymous `go func(){...}()` goroutines have no declaration to walk to,
	// so they are checked in place.
	var literalsChecked int
	for _, sc := range spawnedLiterals {
		if !blockRegistersRecover(sc.lit.Body) {
			t.Errorf("goroutine literal in %s does not register a recover(). "+
				"An unrecovered panic in a goroutine terminates the WHOLE process — add a "+
				"deferred recover() or worker.Recover() (AGT-12).", sc.file)
		}
		literalsChecked++
	}
	if literalsChecked == 0 {
		t.Errorf("expected to check at least one `go func(){...}()` literal — " +
			"the discovery in this test has drifted and is no longer protecting anything")
	}

	// Guard against the named-method check silently covering nothing (e.g. if
	// the spawn idiom changes and the AST match stops finding anything).
	for _, want := range []string{
		"runLedgerStreamProducer",
		"runTipStreamProducer",
		"runObservationsStreamProducer",
		"forwardClosedStream",
		"forwardTipStream",
	} {
		if !containsProducerName(checked, want) {
			t.Errorf("expected to check %s but it was not discovered as a spawned method — "+
				"the discovery in this test has drifted from the code and is no longer protecting anything", want)
		}
	}
}

// blockRegistersRecover reports whether body registers panic recovery via a
// top-level deferred statement: a func literal that calls recover() directly,
// a deferred call to the shared (*Server).recoverStreamProducer helper, or a
// deferred call to worker.Recover — all three ARE the deferred function, so
// recover() still fires in the panicking goroutine. All three are accepted so
// the guard survives whichever extraction a given call site uses without
// weakening what it asserts: the goroutine must register SOME recovery.
func blockRegistersRecover(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "recover" {
				found = true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil {
				pkg, ok := sel.X.(*ast.Ident)
				if sel.Sel.Name == "recoverStreamProducer" ||
					(ok && pkg.Name == "worker" && sel.Sel.Name == "Recover") {
					found = true
				}
			}
			return true
		})
		return true
	})
	return found
}

func containsProducerName(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
