package guardscan_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// fixture writes src as main.go of package fx/<name> inside mod.
func fixture(t *testing.T, mod, name, src string) string {
	t.Helper()
	dir := filepath.Join(mod, "fx", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
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

// TestScan_FuncLiteral covers the shape the original guard tests
// understood, plus the two ways a literal can fail to be guarded.
func TestScan_FuncLiteral(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "funclit", `package main

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

// TestScan_NamedSameFileCallee: a walk that returns early on any non-FuncLit callee makes `go loop(ctx)` invisible,
// so the site count would omit it. Both spellings must be seen and told apart.
func TestScan_NamedSameFileCallee(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "named", `package main

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

// TestScan_NamedCalleeGoStmtsAreWalked: a named callee may start further goroutines in another file of the package.
// ScanFile must surface all three sites, attributing the two inner ones to workers.go, not report only the one.
func TestScan_NamedCalleeGoStmtsAreWalked(t *testing.T) {
	dir := filepath.Join(guardscan.FixtureModule(t), "fx", "namedcallee_gostmts")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workers.go"), []byte(`package main

import "github.com/Stellar-Index/StellarIndex/internal/worker"

func startWorkers() {
	defer worker.Recover(nil, "startWorkers")
	go unrecoveredOne()
	go unrecoveredTwo()
}

func unrecoveredOne() {}
func unrecoveredTwo() {}
`), 0o600); err != nil {
		t.Fatalf("write workers.go: %v", err)
	}
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(`package main

func run() {
	go startWorkers()
}
`), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	sites := scan(t, path)
	if len(sites) != 3 {
		t.Fatalf("found %d go statements, want 3 (go startWorkers() plus its two inner goroutines); targets: %v", len(sites), targets(sites))
	}
	byTarget := map[string]guardscan.Site{}
	for _, s := range sites {
		byTarget[s.Target] = s
	}
	top, ok := byTarget["startWorkers"]
	if !ok {
		t.Fatalf("go startWorkers() was not discovered; targets: %v", targets(sites))
	}
	if top.Kind != guardscan.KindPackageFunc || !top.Recovers {
		t.Errorf("go startWorkers(): kind=%q recovers=%v, want package-func/true", top.Kind, top.Recovers)
	}
	for _, name := range []string{"unrecoveredOne", "unrecoveredTwo"} {
		inner, ok := byTarget[name]
		if !ok {
			t.Fatalf("go %s(), started inside startWorkers() in workers.go, was never discovered; targets: %v", name, targets(sites))
		}
		if inner.Recovers {
			t.Errorf("go %s(): reported as recovering, but neither startWorkers nor %s defers a guard around it", name, name)
		}
		if !strings.HasPrefix(inner.Origin, "workers.go:") {
			t.Errorf("go %s(): origin=%q, want a position in workers.go", name, inner.Origin)
		}
	}
}

// TestScan_CrossPackageResolvesRealModuleSource resolves against the real internal/worker package, not fixtures.
// Both must report Recovers=false: worker.Recover calls recover() but defers nothing, so `go worker.Recover(...)` guards nothing.
func TestScan_CrossPackageResolvesRealModuleSource(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "crosspkg", `package main

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

// TestScan_CrossPackageGuardedCallee is the positive half: a callee in another package of this
// module that DOES defer a guard is accepted, and its unguarded sibling is not. Two fixture
// packages, so the module-local import path is followed for real.
func TestScan_CrossPackageGuardedCallee(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	fixture(t, mod, "dep", `package dep

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
	path := fixture(t, mod, "crosspkg_guarded", `package main

import (
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/fx/dep"
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

// TestScan_MethodOnConstructedValue covers `go x.M(...)` where x's type is only knowable from its constructor's
// declared result type, which the scanner reads from the package index before looking up the method.
func TestScan_MethodOnConstructedValue(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "method", `package main

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

// TestScan_UnresolvableFailsClosed: a callee the scanner cannot follow (a method on an out-of-module type) must come back
// as KindUnresolved so the calling test demands a literal wrapper, never silently skip it.
func TestScan_UnresolvableFailsClosed(t *testing.T) {
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "unresolvable", `package main

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
	mod := guardscan.FixtureModule(t)
	path := fixture(t, mod, "content", `package main

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
