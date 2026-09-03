package guardscan_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// fixture writes a Go file into a temp package inside THIS module, so
// the scanner's module-relative import resolution (go.mod lookup +
// import-path → directory mapping) is exercised for real rather than
// mocked. The package sits under internal/worker/guardscan/testdata at
// run time, which is inside the module tree but not compiled into any
// binary (go tooling ignores testdata/).
func fixture(t *testing.T, name, src string) string {
	t.Helper()
	dir := filepath.Join("testdata", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		// Remove the parent too once the last fixture has gone; this
		// fails harmlessly while other fixtures still live in it, so the
		// tree is left exactly as it was found.
		_ = os.Remove("testdata")
	})
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func scan(t *testing.T, path string) []guardscan.Site {
	t.Helper()
	sites, err := guardscan.ScanFile(path, guardscan.Config{
		Guards: []string{"worker.Recover", "worker.Report", "recoverBackgroundWorker"},
	})
	if err != nil {
		t.Fatalf("ScanFile(%s): %v", path, err)
	}
	return sites
}

// TestScan_FuncLiteral covers the shape the pre-#368 guard tests already
// understood, plus the two ways a literal can fail to be guarded.
func TestScan_FuncLiteral(t *testing.T) {
	path := fixture(t, "funclit", `package main

import "log/slog"

import "github.com/Stellar-Index/StellarIndex/internal/worker"

func run(logger *slog.Logger) {
	go func() {
		defer worker.Recover(logger, "guarded")
		work()
	}()
	go func() {
		work()
	}()
	go func() {
		// A bare recover() swallows the panic without counting it.
		defer func() { _ = recover() }()
		work()
	}()
}

func work() {}
`)
	sites := scan(t, path)
	if len(sites) != 3 {
		t.Fatalf("found %d go statements, want 3", len(sites))
	}
	if !sites[0].Recovers || sites[0].Guard != "worker.Recover" {
		t.Errorf("guarded literal: Recovers=%v Guard=%q, want true/worker.Recover", sites[0].Recovers, sites[0].Guard)
	}
	if sites[1].Recovers {
		t.Errorf("unguarded literal reported as recovering")
	}
	if sites[2].Recovers {
		t.Errorf("a deferred bare recover() must NOT satisfy the guard — it hides the panic instead of counting it")
	}
	for i, s := range sites {
		if s.Kind != guardscan.KindFuncLit {
			t.Errorf("site %d kind = %q, want %q", i, s.Kind, guardscan.KindFuncLit)
		}
		if s.Enclosing != "run" {
			t.Errorf("site %d enclosing = %q, want run", i, s.Enclosing)
		}
	}
}

// TestScan_NamedSameFileCallee is the #368 M1 defect in miniature: the
// pre-fix walk returned early on any callee that was not a *ast.FuncLit,
// so `go loop(ctx)` was not merely unchecked — it was INVISIBLE, and the
// site count used to certify the guard "covers everything" did not
// include it. Both spellings must now be seen, and told apart.
func TestScan_NamedSameFileCallee(t *testing.T) {
	path := fixture(t, "named", `package main

import "log/slog"

import "github.com/Stellar-Index/StellarIndex/internal/worker"

func run(logger *slog.Logger) {
	go guarded(logger)
	go bare(logger)
}

func guarded(logger *slog.Logger) {
	defer worker.Recover(logger, "guarded")
	work()
}

func bare(logger *slog.Logger) {
	work()
	// A defer inside a nested literal guards the LITERAL, not this
	// function, so it must not satisfy the guard here.
	go func() { defer worker.Recover(logger, "inner"); work() }()
}

func work() {}
`)
	sites := scan(t, path)
	if len(sites) != 3 {
		t.Fatalf("found %d go statements, want 3 (two named + one nested literal)", len(sites))
	}
	byTarget := map[string]guardscan.Site{}
	for _, s := range sites {
		byTarget[s.Target] = s
	}
	g, ok := byTarget["guarded"]
	if !ok {
		t.Fatalf("named callee `guarded` was not discovered; targets: %v", targets(sites))
	}
	if g.Kind != guardscan.KindPackageFunc || !g.Recovers {
		t.Errorf("go guarded(): kind=%q recovers=%v, want package-func/true", g.Kind, g.Recovers)
	}
	b, ok := byTarget["bare"]
	if !ok {
		t.Fatalf("named callee `bare` was not discovered; targets: %v", targets(sites))
	}
	if b.Recovers {
		t.Errorf("go bare(): a guard registered inside a NESTED literal was counted as guarding the outer function")
	}
}

// TestScan_CrossPackageResolvesRealModuleSource points the resolver at a
// REAL package of this module — internal/worker — and checks it lands on
// the real declarations. This is the leg that proves go.mod discovery and
// the import-path → directory mapping work against the actual tree, not
// just against fixtures.
//
// Both targets correctly report Recovers=false, and that is the point
// worth pinning: worker.Recover's body calls recover() but DEFERS
// nothing, so a `go worker.Recover(…)` would not guard anything —
// recover() only fires from a function the panicking goroutine deferred.
// A scanner that grep'd for the word "recover" in a resolved body would
// answer true here and certify an unguarded goroutine.
func TestScan_CrossPackageResolvesRealModuleSource(t *testing.T) {
	path := fixture(t, "crosspkg", `package main

import "log/slog"

import "github.com/Stellar-Index/StellarIndex/internal/worker"

func run(logger *slog.Logger) {
	go worker.Recover(logger, "calls-recover-but-defers-nothing")
	go worker.Report(logger, "does-not-recover", "x")
}
`)
	sites := scan(t, path)
	if len(sites) != 2 {
		t.Fatalf("found %d go statements, want 2", len(sites))
	}
	for _, s := range sites {
		if s.Kind != guardscan.KindImportedFunc {
			t.Fatalf("%s: kind=%q reason=%q, want imported-func — cross-package resolution did not run",
				s.Target, s.Kind, s.Reason)
		}
		if !strings.HasPrefix(s.Origin, "recover.go:") {
			t.Errorf("%s resolved to %q, want a position in internal/worker/recover.go", s.Target, s.Origin)
		}
		if s.Recovers {
			t.Errorf("%s (%s) reported as guarding its goroutine; neither function DEFERS a guard, and an undeferred recover() never fires",
				s.Target, s.Origin)
		}
	}
}

// TestScan_CrossPackageGuardedCallee is the positive half: a callee in
// another package of this module that DOES defer a guard must be
// resolved and accepted, and its unguarded sibling in the same package
// must not be. Two fixture packages rather than one, so the module-local
// import path is followed for real (go.mod → directory → parse → index).
func TestScan_CrossPackageGuardedCallee(t *testing.T) {
	fixture(t, "dep", `package dep

import (
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Guarded defers the shared guard, so a goroutine started on it is
// protected even though the go statement carries no defer of its own.
func Guarded(logger *slog.Logger) {
	defer worker.Recover(logger, "dep-guarded")
	Work()
}

// Bare is the same shape without the guard.
func Bare(logger *slog.Logger) { Work() }

func Work() {}
`)
	path := fixture(t, "crosspkg_guarded", `package main

import (
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan/testdata/dep"
)

func run(logger *slog.Logger) {
	go dep.Guarded(logger)
	go dep.Bare(logger)
}
`)
	sites := scan(t, path)
	if len(sites) != 2 {
		t.Fatalf("found %d go statements, want 2", len(sites))
	}
	for _, s := range sites {
		if s.Kind != guardscan.KindImportedFunc {
			t.Fatalf("%s: kind=%q reason=%q, want imported-func", s.Target, s.Kind, s.Reason)
		}
	}
	if !sites[0].Recovers || sites[0].Guard != "worker.Recover" {
		t.Errorf("dep.Guarded: recovers=%v guard=%q (origin %s), want true/worker.Recover",
			sites[0].Recovers, sites[0].Guard, sites[0].Origin)
	}
	if sites[1].Recovers {
		t.Errorf("dep.Bare: an unguarded cross-package callee was reported as guarded (origin %s)", sites[1].Origin)
	}
}

// TestScan_MethodOnConstructedValue covers `go x.M(…)` where x's type is
// only knowable from the constructor it was assigned from. The scanner
// reads the constructor's declared result type out of the same package
// index, then looks the method up on it.
func TestScan_MethodOnConstructedValue(t *testing.T) {
	path := fixture(t, "method", `package main

import "log/slog"

import "github.com/Stellar-Index/StellarIndex/internal/worker"

type loop struct{ logger *slog.Logger }

func newLoop(logger *slog.Logger) *loop { return &loop{logger: logger} }

func (l *loop) Run() {
	defer worker.Recover(l.logger, "loop")
}

func (l *loop) RunBare() {}

func run(logger *slog.Logger) {
	l := newLoop(logger)
	go l.Run()
	go l.RunBare()
}
`)
	sites := scan(t, path)
	if len(sites) != 2 {
		t.Fatalf("found %d go statements, want 2", len(sites))
	}
	if sites[0].Kind != guardscan.KindImportedMethod || !sites[0].Recovers {
		t.Errorf("go l.Run(): kind=%q recovers=%v reason=%q, want imported-method/true",
			sites[0].Kind, sites[0].Recovers, sites[0].Reason)
	}
	if sites[1].Recovers {
		t.Errorf("go l.RunBare(): unguarded method reported as guarded")
	}
}

// TestScan_UnresolvableFailsClosed is the safety property that makes the
// syntactic approach sound. A callee this scanner cannot follow —
// here a method on a type from OUTSIDE the module — must come back as
// KindUnresolved so the calling test can fail and demand a literal
// wrapper. Silently skipping it would be a guard that widens itself
// every time someone writes an unusual spelling.
func TestScan_UnresolvableFailsClosed(t *testing.T) {
	path := fixture(t, "unresolvable", `package main

import "net/http"

func run(srv *http.Server) {
	go srv.ListenAndServe() //nolint:errcheck // fixture
}
`)
	sites := scan(t, path)
	if len(sites) != 1 {
		t.Fatalf("found %d go statements, want 1", len(sites))
	}
	if sites[0].Kind != guardscan.KindUnresolved {
		t.Fatalf("kind=%q, want unresolved — a callee outside the module has no source here to check", sites[0].Kind)
	}
	if sites[0].Recovers {
		t.Fatal("an unresolved site must never report Recovers=true")
	}
	if sites[0].Reason == "" {
		t.Error("an unresolved site must explain itself so the failure message can tell an author what to do")
	}
}

// TestSite_ContentPredicates covers the helpers the binaries' guard
// tests use to justify an exemption by CONTENT rather than by line
// number.
func TestSite_ContentPredicates(t *testing.T) {
	path := fixture(t, "content", `package main

import (
	"net/http"
	"sync"
)

func run(srv *http.Server, wg *sync.WaitGroup, errs chan error) {
	go func() {
		defer wg.Done()
		errs <- srv.ListenAndServe()
	}()
}
`)
	sites := scan(t, path)
	if len(sites) != 1 {
		t.Fatalf("found %d go statements, want 1", len(sites))
	}
	s := sites[0]
	if !s.Calls("ListenAndServe") {
		t.Error("Calls should match a method call by its final identifier")
	}
	if s.Calls("PersistEvents") {
		t.Error("Calls matched a function the body never calls")
	}
	if !s.DefersCall("wg.Done") {
		t.Error("DefersCall should match a qualified deferred call")
	}
	if !s.SendsOn("errs") {
		t.Error("SendsOn should see the channel send")
	}
	if s.SendsOn("other") {
		t.Error("SendsOn matched a channel the body never sends on")
	}
}

func targets(sites []guardscan.Site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, s.Target)
	}
	return out
}
