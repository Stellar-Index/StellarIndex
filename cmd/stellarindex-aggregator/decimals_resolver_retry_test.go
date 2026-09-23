package main

// The decimals-assumption guard must be DELAYED by a cold lake, never
// disabled by one (audit-2026-09-02 F040).
//
// The resolver used to be dialled inline at startup: one failed ping
// emitted a single WARN and the guard — Backfill and periodic Sweep both
// — never existed for that process. That is the expected outcome of a
// reboot, not an edge case: clickhouse-server spends minutes loading
// metadata for the 150B-row lake and the aggregator unit's ordering does
// not wait for it. A non-7-decimal SEP-41 token listing afterwards then
// gets no nonstandard_decimals_assets row, aggregate.AdjustPrice applies
// no correction, and every served price on its pairs is skewed by
// 10^(7-decimals) — silently, since no metric separates "the guard found
// nothing" from "the guard never ran".
//
// Proven red against the pre-fix wiring (a single
// clickhouse.NewExplorerReader call whose error logged and fell
// through): one dial, no retry, guard never armed.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// Source-level tripwire (the decimals_boot_guard_test.go /
// freeze_wiring_guard_test.go pattern): the retry above is only worth
// anything if the guard's wiring actually goes through it. The branch
// lives inside run(), which needs a config file, a Postgres pool and a
// Redis client before it is reachable, so the WIRING is asserted here.
//
// Putting clickhouse.NewExplorerReader (or clickhouse.NewTxIndexReader)
// back inline — which reads like a simplification to anyone who does not
// know the cold-boot race — restores the defect in full.
//
// K024: the MEV tx-order resolver and the priceless-coverage SAC resolver
// used to be the same single-shot-then-permanent-degrade shape as the
// pre-fix decimals guard — carried deliberately for a time as a smaller,
// different harm (a SAC mis-ticketed as priceless; sandwich detection
// silently off), but that reasoning does not hold once the retrying dial
// is a two-line call (dialLakeReaderWithRetry) rather than a bespoke
// rewrite, so both now go through it too.
func TestDecimalsGuardResolverIsDialledWithRetry(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// knownDirectDials is the number of OTHER components that still dial
	// a lake reader inline, each accounted for:
	//
	//  1. the supply refresher's close-time source — fails CLOSED
	//     (`return err`), so a cold lake refuses startup rather than
	//     degrading silently; nothing to fix.
	//
	// A SECOND direct dial means some other component went back inline.
	const knownDirectDials = 1
	// wantViaGenericRetry is dialDecimalsResolver's own delegation +
	// the MEV tx-order resolver + the priceless-coverage SAC resolver
	// (K024).
	const wantViaGenericRetry = 3

	direct, directTxIndex, viaRetry, viaGenericRetry := 0, 0, 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			// A CALL of clickhouse.NewExplorerReader/NewTxIndexReader —
			// not a reference to one, which is how each is handed to the
			// retrying dial helpers.
			pkg, pkgOK := fn.X.(*ast.Ident)
			if !pkgOK || pkg.Name != "clickhouse" || fn.Sel == nil {
				return true
			}
			switch fn.Sel.Name {
			case "NewExplorerReader":
				direct++
			case "NewTxIndexReader":
				directTxIndex++
			}
		case *ast.Ident:
			switch fn.Name {
			case "dialDecimalsResolver":
				viaRetry++
			case "dialLakeReaderWithRetry":
				viaGenericRetry++
			}
		}
		return true
	})

	if viaRetry != 1 {
		t.Errorf("found %d dialDecimalsResolver call(s) in main.go, want exactly 1 — a "+
			"single dial at startup is what disabled the decimals-assumption guard for the "+
			"whole process lifetime whenever ClickHouse was still loading metadata after a "+
			"reboot, so the guard's resolver must go through the retrying dial", viaRetry)
	}
	if viaGenericRetry != wantViaGenericRetry {
		t.Errorf("found %d dialLakeReaderWithRetry call(s) in main.go, want %d — the MEV "+
			"tx-order resolver and the priceless-coverage SAC resolver must both dial "+
			"through the retrying helper, not a bare clickhouse.New*Reader call, or a cold "+
			"lake at boot disables them for the process lifetime again (K024)", viaGenericRetry, wantViaGenericRetry)
	}
	if direct != knownDirectDials {
		t.Errorf("found %d direct clickhouse.NewExplorerReader call(s) in main.go, want %d — "+
			"a new one is a component that gives up on a cold lake for its whole process "+
			"lifetime; either fail closed like the supply close-time reader or retry like "+
			"the decimals guard, then account for it here", direct, knownDirectDials)
	}
	if directTxIndex != 0 {
		t.Errorf("found %d direct clickhouse.NewTxIndexReader call(s) in main.go, want 0 — "+
			"the MEV tx-order resolver must dial through dialLakeReaderWithRetry, not "+
			"inline, or a cold lake at boot disables sandwich/oracle-sandwich detection for "+
			"the process lifetime (K024)", directTxIndex)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// shortenDecimalsResolverBackoff makes the retry loop test-fast without
// changing its shape.
func shortenDecimalsResolverBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := decimalsResolverRetryMin, decimalsResolverRetryMax
	decimalsResolverRetryMin = time.Millisecond
	decimalsResolverRetryMax = 4 * time.Millisecond
	t.Cleanup(func() {
		decimalsResolverRetryMin, decimalsResolverRetryMax = oldMin, oldMax
	})
}

func TestDialDecimalsResolver_ColdLakeDelaysTheGuardRatherThanDisablingIt(t *testing.T) {
	shortenDecimalsResolverBackoff(t)

	// A ClickHouse still loading metadata: the first dials fail, then it
	// answers. The shipped reader value is opaque here — what matters is
	// that the guard gets one at all.
	const failures = 5
	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		if attempts.Add(1) <= failures {
			return nil, errors.New("clickhouse: ping explorer reader 127.0.0.1:9300: dial tcp: connection refused")
		}
		return &clickhouse.ExplorerReader{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader, ok := dialDecimalsResolver(ctx, quietLogger(), "127.0.0.1:9300", dial)
	if !ok {
		t.Fatalf("resolver gave up after %d failed dials — a cold lake must delay the "+
			"decimals guard, not disable it for the process lifetime", attempts.Load())
	}
	if reader == nil {
		t.Fatal("ok=true with a nil reader — the guard would be built on nothing")
	}
	if got := attempts.Load(); got != failures+1 {
		t.Errorf("dial attempts = %d, want %d (%d failures then the success) — "+
			"the pre-fix wiring dialled exactly once and gave up", got, failures+1, failures)
	}
}

// Shutdown must still stop the loop promptly: a retry that ignores
// cancellation would hold the refresher WaitGroup open and hang the
// aggregator's shutdown.
func TestDialDecimalsResolver_StopsOnShutdown(t *testing.T) {
	shortenDecimalsResolverBackoff(t)

	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		attempts.Add(1)
		return nil, errors.New("clickhouse: unreachable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader, ok := dialDecimalsResolver(ctx, quietLogger(), "127.0.0.1:9300", dial)
		if ok {
			t.Errorf("ok=true from a resolver that never answered")
		}
		if reader != nil {
			t.Errorf("reader = %v on the shutdown path, want nil", reader)
		}
	}()

	// Let it fail a few times, then shut down.
	deadline := time.After(2 * time.Second)
	for attempts.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("the resolver did not retry at all before shutdown")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dialDecimalsResolver did not return after context cancellation — " +
			"shutdown would hang on the refresher WaitGroup")
	}
}

// A dial that succeeds first time must not pay any backoff: the common
// case is a warm lake and the guard has to arm immediately.
func TestDialDecimalsResolver_WarmLakeArmsImmediately(t *testing.T) {
	var attempts atomic.Int32
	dial := func(context.Context, string) (*clickhouse.ExplorerReader, error) {
		attempts.Add(1)
		return &clickhouse.ExplorerReader{}, nil
	}

	start := time.Now()
	reader, ok := dialDecimalsResolver(context.Background(), quietLogger(), "127.0.0.1:9300", dial)
	elapsed := time.Since(start)

	if !ok || reader == nil {
		t.Fatalf("warm dial returned ok=%v reader=%v, want a reader", ok, reader)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("dial attempts = %d on a healthy lake, want 1", got)
	}
	if elapsed >= decimalsResolverRetryMin {
		t.Errorf("a successful first dial waited %v; it must not pay the backoff", elapsed)
	}
}
