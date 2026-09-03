// Package guardscan finds every detached goroutine a Go source file
// starts and reports whether each one registers panic recovery.
//
// It exists because an unrecovered panic in ANY goroutine terminates the
// whole Go process — it is not confined to the goroutine that panicked —
// so each binary carries an AST guard test asserting that every `go`
// statement in its main.go defers a recovery helper. Those tests used to
// be per-binary copies that only understood the `go func(){…}()` form:
//
//	go func() { defer worker.Recover(logger, "x"); loop(ctx) }()   // seen
//	go loop(ctx)                                                   // INVISIBLE
//
// The second form is the one that bites, because it looks tidier. At the
// time this package was written the stellarindex-api binary started four
// workers that way and its guard test could not see a single one of them
// (#368 M1). This package resolves a named callee to its declaration —
// same package (any file), or another package of the SAME MODULE, whose
// source is located from go.mod and parsed — and checks the guard there.
//
// Resolution is syntactic (go/parser only, no type checker and no
// golang.org/x/tools dependency), so it cannot resolve everything. That
// is deliberate and safe in ONE direction only: a site it cannot resolve
// is reported as [KindUnresolved], never as "guarded". Callers must fail
// on unresolved sites and ask for a `go func(){ defer … }()` wrapper,
// which is always expressible. A scanner that quietly skipped what it
// could not understand would be a guard that widens itself.
package guardscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Kind records how a `go` statement's target was resolved.
type Kind string

const (
	// KindFuncLit is `go func(){ … }()` — the body is right there.
	KindFuncLit Kind = "func-literal"
	// KindPackageFunc is `go f(…)` resolved to a declaration in the
	// scanned file's own package (this or any sibling file).
	KindPackageFunc Kind = "package-func"
	// KindImportedFunc is `go pkg.F(…)` resolved to a declaration in
	// another package of the same module.
	KindImportedFunc Kind = "imported-func"
	// KindImportedMethod is `go x.M(…)` resolved to a method whose
	// receiver type was determined syntactically.
	KindImportedMethod Kind = "imported-method"
	// KindUnresolved is a `go` statement whose body this scanner could
	// not locate. Callers MUST treat it as a failure.
	KindUnresolved Kind = "unresolved"
)

// Config selects the recovery helpers a body may use to satisfy the guard.
//
// Guards are call names as written in source: a bare "recoverBackgroundWorker"
// or a qualified "worker.Recover". A bare `recover()` is deliberately NOT
// accepted on its own — swallowing a panic without logging it and moving
// stellarindex_worker_panics_total turns a loud crash into a silent one,
// which is the failure this whole guard exists to prevent. A deferred
// function literal counts only when it calls recover() AND one of Guards.
type Config struct {
	Guards []string
}

// Site is one `go` statement and what the scanner could prove about it.
type Site struct {
	// Line is the line of the `go` keyword in the scanned file.
	Line int
	// Target is the callee as written, e.g. "func literal",
	// "prewarmCaches" or "apiSrv.StartIngestionSnapshotRefresh".
	Target string
	Kind   Kind
	// Origin is "file.go:LINE" of the resolved declaration — the same
	// as the go statement for a literal, elsewhere for a named callee.
	Origin string
	// Enclosing is the name of the function the `go` statement is
	// written in. Callers use it to scope assertions that only make
	// sense inside one function — a shutdown WaitGroup declared in
	// run(), say, is not in scope for a goroutine started elsewhere.
	Enclosing string
	// Recovers reports whether the resolved body defers one of
	// Config.Guards at the body's own statement level.
	Recovers bool
	// Guard names the marker that satisfied Recovers ("" when none).
	Guard string
	// Reason explains a KindUnresolved site.
	Reason string

	body *ast.BlockStmt
	fset *token.FileSet
}

// Calls reports whether the site's body calls name anywhere, including
// inside nested function literals. name matches either the qualified
// rendering ("pipeline.PersistEvents") or the bare final identifier
// ("ListenAndServe"), so a call through a variable receiver still matches.
//
// This is the hook for CONTENT-checked exemptions: a caller that wants to
// exempt the HTTP listener asserts the body calls "ListenAndServe" rather
// than trusting a line number, so the exemption cannot silently migrate
// onto an unrelated worker.
func (s Site) Calls(name string) bool {
	if s.body == nil {
		return false
	}
	found := false
	ast.Inspect(s.body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if matchesCallName(call.Fun, name) {
			found = true
		}
		return true
	})
	return found
}

// DefersCall reports whether the site's body defers name at its OWN
// statement level — descending through blocks, if/for/select bodies, but
// never into a nested function literal. A defer inside a nested literal
// belongs to that literal, not to this goroutine, so counting it would
// certify a guard that does not run.
func (s Site) DefersCall(name string) bool {
	if s.body == nil {
		return false
	}
	return defersAtOwnLevel(s.body, func(call *ast.CallExpr) bool {
		return matchesCallName(call.Fun, name)
	})
}

// SendsOn reports whether the site's body contains a channel send on the
// named channel, at any depth. Used to assert that a site which recovers
// a panic in a LOAD-BEARING goroutine still re-signals the fault as fatal
// rather than quietly continuing.
func (s Site) SendsOn(chanName string) bool {
	if s.body == nil {
		return false
	}
	found := false
	ast.Inspect(s.body, func(n ast.Node) bool {
		send, ok := n.(*ast.SendStmt)
		if !ok {
			return true
		}
		if id, ok := send.Chan.(*ast.Ident); ok && id.Name == chanName {
			found = true
		}
		return true
	})
	return found
}

// ScanFile parses path and returns one Site per `go` statement in it, in
// source order. Sites are returned for every `go` statement including
// unresolvable ones; an error is returned only when path itself (or the
// module it lives in) cannot be read.
func ScanFile(path string, cfg Config) ([]Site, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	modRoot, modPath, err := findModule(dir)
	if err != nil {
		return nil, err
	}
	r := &resolver{
		fset:    fset,
		cfg:     cfg,
		modRoot: modRoot,
		modPath: modPath,
		pkgs:    map[string]*pkgIndex{},
	}
	self, err := r.indexDir(dir)
	if err != nil {
		return nil, err
	}

	var sites []Site
	// Walk top-level declarations so each `go` statement knows the
	// function it sits in — needed to type a method receiver from the
	// enclosing signature.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			g, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			sites = append(sites, r.site(g, file, fn, self))
			return true
		})
	}
	return sites, nil
}

// ─── resolution ─────────────────────────────────────────────────────

type pkgIndex struct {
	dir   string
	fset  *token.FileSet
	funcs map[string]*ast.FuncDecl // package-level funcs by name
	// methods is keyed "TypeName.MethodName" with the receiver's
	// pointer star stripped.
	methods map[string]*ast.FuncDecl
}

type resolver struct {
	fset    *token.FileSet
	cfg     Config
	modRoot string
	modPath string
	pkgs    map[string]*pkgIndex // keyed by directory
}

func (r *resolver) site(g *ast.GoStmt, file *ast.File, enclosing *ast.FuncDecl, self *pkgIndex) Site {
	s := Site{
		Line:   r.fset.Position(g.Pos()).Line,
		Target: exprString(g.Call.Fun),
		fset:   r.fset,
	}
	if enclosing != nil && enclosing.Name != nil {
		s.Enclosing = enclosing.Name.Name
	}

	switch fun := g.Call.Fun.(type) {
	case *ast.FuncLit:
		s.Kind = KindFuncLit
		s.Target = "func literal"
		s.body = fun.Body
		s.Origin = shortPos(r.fset, g.Pos())
	case *ast.Ident:
		decl, ok := self.funcs[fun.Name]
		if !ok {
			s.Kind = KindUnresolved
			s.Reason = fmt.Sprintf("no declaration of %s() in package %s", fun.Name, self.dir)
			return s
		}
		s.Kind = KindPackageFunc
		s.body = decl.Body
		s.Origin = shortPos(self.fset, decl.Pos())
	case *ast.SelectorExpr:
		r.resolveSelector(&s, fun, file, enclosing, self)
	default:
		s.Kind = KindUnresolved
		s.Reason = fmt.Sprintf("unsupported callee expression %T", g.Call.Fun)
	}

	if s.body != nil {
		s.Guard = r.guardIn(s.body)
		s.Recovers = s.Guard != ""
	}
	return s
}

// resolveSelector handles `go pkg.F(…)` and `go recv.M(…)`.
func (r *resolver) resolveSelector(s *Site, sel *ast.SelectorExpr, file *ast.File, enclosing *ast.FuncDecl, self *pkgIndex) {
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		s.Kind = KindUnresolved
		s.Reason = fmt.Sprintf("receiver of .%s is an expression (%T), not a plain identifier", sel.Sel.Name, sel.X)
		return
	}

	// Case 1: pkg.F — x names an import of this file.
	if importPath, ok := importPathFor(file, x.Name); ok {
		idx, err := r.indexImport(importPath)
		if err != nil {
			s.Kind = KindUnresolved
			s.Reason = err.Error()
			return
		}
		decl, ok := idx.funcs[sel.Sel.Name]
		if !ok {
			s.Kind = KindUnresolved
			s.Reason = fmt.Sprintf("no func %s in %s", sel.Sel.Name, importPath)
			return
		}
		s.Kind = KindImportedFunc
		s.body = decl.Body
		s.Origin = shortPos(idx.fset, decl.Pos())
		return
	}

	// Case 2: recv.M — determine recv's type syntactically, then find
	// the method on it.
	pkgAlias, typeName, ok := r.typeOfIdent(x.Name, file, enclosing, self)
	if !ok {
		s.Kind = KindUnresolved
		s.Reason = fmt.Sprintf("cannot determine the type of %q syntactically", x.Name)
		return
	}
	idx := self
	if pkgAlias != "" {
		importPath, ok := importPathFor(file, pkgAlias)
		if !ok {
			s.Kind = KindUnresolved
			s.Reason = fmt.Sprintf("%s is not an import of this file", pkgAlias)
			return
		}
		var err error
		if idx, err = r.indexImport(importPath); err != nil {
			s.Kind = KindUnresolved
			s.Reason = err.Error()
			return
		}
	}
	decl, ok := idx.methods[typeName+"."+sel.Sel.Name]
	if !ok {
		s.Kind = KindUnresolved
		s.Reason = fmt.Sprintf("no method %s.%s in %s", typeName, sel.Sel.Name, idx.dir)
		return
	}
	s.Kind = KindImportedMethod
	s.body = decl.Body
	s.Origin = shortPos(idx.fset, decl.Pos())
}

// typeOfIdent determines the declared type of a local identifier without a
// type checker. It handles the three shapes that carry a written-down type:
// the enclosing function's receiver/parameters/named results, an explicit
// `var x pkg.T`, and `x := pkg.NewT(…)` where NewT's result type is
// declared in a package this scanner can parse. Anything else returns
// false, which the caller turns into KindUnresolved.
func (r *resolver) typeOfIdent(name string, file *ast.File, enclosing *ast.FuncDecl, self *pkgIndex) (pkgAlias, typeName string, ok bool) {
	if enclosing != nil {
		if p, t, ok := typeFromFieldLists(name, enclosing.Recv, enclosing.Type.Params, enclosing.Type.Results); ok {
			return p, t, true
		}
	}
	if enclosing == nil || enclosing.Body == nil {
		return "", "", false
	}

	found := false
	ast.Inspect(enclosing.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		var p, t string
		var ok bool
		switch stmt := n.(type) {
		case *ast.DeclStmt:
			p, t, ok = typeFromVarDecl(stmt, name)
		case *ast.AssignStmt:
			p, t, ok = r.typeFromDefine(stmt, name, file, self)
		}
		if ok {
			pkgAlias, typeName, found = p, t, true
			return false
		}
		return true
	})
	return pkgAlias, typeName, found
}

// typeFromVarDecl reads `var x pkg.T` / `var x *pkg.T`.
func typeFromVarDecl(stmt *ast.DeclStmt, name string) (pkgAlias, typeName string, ok bool) {
	gd, isGen := stmt.Decl.(*ast.GenDecl)
	if !isGen || gd.Tok != token.VAR {
		return "", "", false
	}
	for _, spec := range gd.Specs {
		vs, isValue := spec.(*ast.ValueSpec)
		if !isValue || vs.Type == nil {
			continue
		}
		for _, id := range vs.Names {
			if id.Name == name {
				return splitTypeExpr(vs.Type)
			}
		}
	}
	return "", "", false
}

// typeFromDefine reads `x := …`, including the `x, err := f()` form where
// the single right-hand call supplies both results.
func (r *resolver) typeFromDefine(stmt *ast.AssignStmt, name string, file *ast.File, self *pkgIndex) (pkgAlias, typeName string, ok bool) {
	if stmt.Tok != token.DEFINE {
		return "", "", false
	}
	multiResult := len(stmt.Rhs) == 1 && len(stmt.Lhs) > 1
	for i, lhs := range stmt.Lhs {
		id, isIdent := lhs.(*ast.Ident)
		if !isIdent || id.Name != name {
			continue
		}
		switch {
		case i < len(stmt.Rhs):
			return r.typeOfExpr(stmt.Rhs[i], file, self)
		case multiResult && i == 0:
			return r.typeOfExpr(stmt.Rhs[0], file, self)
		}
	}
	return "", "", false
}

// typeOfExpr types a right-hand side: a composite literal, an address-of
// a composite literal, or a call to a function whose declared first result
// this scanner can read.
func (r *resolver) typeOfExpr(expr ast.Expr, file *ast.File, self *pkgIndex) (pkgAlias, typeName string, ok bool) {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return r.typeOfExpr(e.X, file, self)
		}
	case *ast.CompositeLit:
		return splitTypeExpr(e.Type)
	case *ast.CallExpr:
		decl, idxFile, ok := r.declOfCall(e.Fun, file, self)
		if !ok || decl.Type.Results == nil || len(decl.Type.Results.List) == 0 {
			return "", "", false
		}
		p, t, ok := splitTypeExpr(decl.Type.Results.List[0].Type)
		if !ok {
			return "", "", false
		}
		// A result type written without a package qualifier inside
		// package P names a type in P — re-qualify it with the alias
		// this file uses for P, so the method lookup lands in the right
		// package index.
		if p == "" && idxFile != "" {
			return idxFile, t, true
		}
		return p, t, true
	}
	return "", "", false
}

// declOfCall resolves a call's callee to its declaration. The second
// result is the package alias (as this file imports it) when the callee
// lives in another package, "" when it is package-local.
func (r *resolver) declOfCall(fun ast.Expr, file *ast.File, self *pkgIndex) (*ast.FuncDecl, string, bool) {
	switch f := fun.(type) {
	case *ast.Ident:
		decl, ok := self.funcs[f.Name]
		return decl, "", ok
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		if !ok {
			return nil, "", false
		}
		importPath, ok := importPathFor(file, x.Name)
		if !ok {
			return nil, "", false
		}
		idx, err := r.indexImport(importPath)
		if err != nil {
			return nil, "", false
		}
		decl, ok := idx.funcs[f.Sel.Name]
		return decl, x.Name, ok
	}
	return nil, "", false
}

// indexImport maps a MODULE-LOCAL import path to its directory and
// indexes it. Packages outside the module (stdlib, third-party) are
// deliberately not resolvable: their source is not in the tree this test
// guards, and a `go` statement into one must be wrapped in a literal so
// the guard is visible where the goroutine is started.
func (r *resolver) indexImport(importPath string) (*pkgIndex, error) {
	if importPath != r.modPath && !strings.HasPrefix(importPath, r.modPath+"/") {
		return nil, fmt.Errorf("%s is outside module %s — wrap the call in a func literal so the guard is visible at the go statement", importPath, r.modPath)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(importPath, r.modPath), "/")
	return r.indexDir(filepath.Join(r.modRoot, filepath.FromSlash(rel)))
}

// indexDir parses every non-test .go file in dir and indexes its
// package-level funcs and methods.
func (r *resolver) indexDir(dir string) (*pkgIndex, error) {
	if idx, ok := r.pkgs[dir]; ok {
		return idx, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read package dir %s: %w", dir, err)
	}
	idx := &pkgIndex{
		dir:     dir,
		fset:    token.NewFileSet(),
		funcs:   map[string]*ast.FuncDecl{},
		methods: map[string]*ast.FuncDecl{},
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(idx.fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, name), err)
		}
		idx.addDecls(f)
	}
	r.pkgs[dir] = idx
	return idx, nil
}

// addDecls files one parsed file's function and method declarations into
// the index. Split out of indexDir to keep it under the gocognit ceiling.
func (idx *pkgIndex) addDecls(f *ast.File) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			idx.funcs[fn.Name.Name] = fn
			continue
		}
		if _, recvType, ok := splitTypeExpr(fn.Recv.List[0].Type); ok {
			idx.methods[recvType+"."+fn.Name.Name] = fn
		}
	}
}

// guardIn returns the name of the recovery marker the body registers, or
// "" when it registers none. Only defers at the body's own statement
// level count (see DefersCall), and a deferred literal counts only when
// it both calls recover() and reports through one of the configured
// guards — a bare recover() swallows the panic silently.
func (r *resolver) guardIn(body *ast.BlockStmt) string {
	var guard string
	defersAtOwnLevel(body, func(call *ast.CallExpr) bool {
		for _, name := range r.cfg.Guards {
			if matchesCallName(call.Fun, name) {
				guard = name
				return true
			}
		}
		if lit, ok := call.Fun.(*ast.FuncLit); ok && lit.Body != nil {
			if inner := r.guardInLiteral(lit.Body); inner != "" {
				guard = inner
				return true
			}
		}
		return false
	})
	return guard
}

// guardInLiteral checks a deferred function literal: it must call
// recover() AND one of the configured guards.
func (r *resolver) guardInLiteral(body *ast.BlockStmt) string {
	var recovers bool
	var guard string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
			recovers = true
		}
		for _, name := range r.cfg.Guards {
			if matchesCallName(call.Fun, name) {
				guard = name
			}
		}
		return true
	})
	if recovers && guard != "" {
		return guard
	}
	return ""
}

// ─── small AST helpers ──────────────────────────────────────────────

// defersAtOwnLevel walks body's statements, descending into blocks but
// NOT into function literals, and calls match for every DeferStmt found.
// It returns true as soon as match does.
func defersAtOwnLevel(body *ast.BlockStmt, match func(*ast.CallExpr) bool) bool {
	// ast.Inspect already does the descent, so no recursive closure is
	// needed here — the two-step `var walk func(...); walk = ...` form the
	// first cut used was both unnecessary and a staticcheck finding.
	found := false
	ast.Inspect(body, func(inner ast.Node) bool {
		if found {
			return false
		}
		switch node := inner.(type) {
		case *ast.FuncLit:
			// A defer inside a nested literal guards THAT literal, not
			// the body we were asked about — do not descend.
			return false
		case *ast.DeferStmt:
			if match(node.Call) {
				found = true
			}
			// Either way, do not descend into the deferred call's own
			// literal body looking for further defers.
			return false
		}
		return true
	})
	return found
}

// matchesCallName reports whether a callee expression matches name, which
// may be qualified ("worker.Recover") or bare ("ListenAndServe").
func matchesCallName(fun ast.Expr, name string) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == name
	case *ast.SelectorExpr:
		if f.Sel == nil {
			return false
		}
		if !strings.Contains(name, ".") {
			return f.Sel.Name == name
		}
		return exprString(f) == name
	}
	return false
}

// typeFromFieldLists looks name up across a function's receiver,
// parameters and named results.
func typeFromFieldLists(name string, lists ...*ast.FieldList) (pkgAlias, typeName string, ok bool) {
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, id := range field.Names {
				if id.Name == name {
					return splitTypeExpr(field.Type)
				}
			}
		}
	}
	return "", "", false
}

// splitTypeExpr renders a type expression as (package alias, type name),
// stripping any pointer star. `*v1.Server` → ("v1", "Server");
// `decimalsCache` → ("", "decimalsCache").
func splitTypeExpr(expr ast.Expr) (pkgAlias, typeName string, ok bool) {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return splitTypeExpr(t.X)
	case *ast.Ident:
		return "", t.Name, true
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok && t.Sel != nil {
			return x.Name, t.Sel.Name, true
		}
	}
	return "", "", false
}

// importPathFor returns the import path a file refers to by the given
// local name, honouring explicit aliases and falling back to the last
// path segment for unaliased imports.
func importPathFor(file *ast.File, local string) (string, bool) {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if imp.Name != nil {
			if imp.Name.Name == local {
				return path, true
			}
			continue
		}
		if lastSegment(path) == local {
			return path, true
		}
	}
	return "", false
}

// lastSegment is the conventional package name of an unaliased import.
// It is a convention, not a guarantee (a package may declare a name that
// differs from its directory), which is one more reason an unresolved
// site fails rather than passes.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.FuncLit:
		return "func literal"
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	}
	return fmt.Sprintf("%T", expr)
}

func shortPos(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
}

// findModule walks up from dir to the nearest go.mod and returns its
// directory and declared module path.
func findModule(dir string) (root, modPath string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for {
		candidate := filepath.Join(abs, "go.mod")
		if data, err := os.ReadFile(candidate); err == nil { //nolint:gosec // walking up from a test's own directory
			for _, line := range strings.Split(string(data), "\n") {
				if after, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return abs, strings.TrimSpace(after), nil
				}
			}
			return "", "", fmt.Errorf("%s has no module line", candidate)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", "", fmt.Errorf("no go.mod found above %s", dir)
		}
		abs = parent
	}
}
