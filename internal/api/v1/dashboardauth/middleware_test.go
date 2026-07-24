package dashboardauth

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestMiddleware_NilNowDoesNotPanic is a regression for the live
// production bug where main.go built the auth Config without a Now
// func and passed it raw to Middleware. NewHandlers defaulted Now on
// its own copy, but the resolver Middleware kept the nil — so
// resolveSession's cfg.Now() nil-derefed on every authenticated
// request. The magic-link cookie resolved fine; /v1/account/me then
// 500'd, making login look broken. Middleware must default Now (and
// Logger) so a valid session resolves without panicking.
func TestMiddleware_NilNowDoesNotPanic(t *testing.T) {
	accounts := newFakeAccountStore()
	users := newFakeUserStore()

	acct, err := accounts.Create(context.Background(), platform.Account{
		Name:   "tester",
		Slug:   "tester",
		Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID,
		Email:     "tester@example.com",
		Role:      platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := users.CreateSession(context.Background(), platform.Session{
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Config with Now AND Logger left nil — the exact shape that
	// panicked in production.
	cfg := &Config{
		Accounts: accounts,
		Users:    users,
		Tokens:   newFakeTokenStore(nil),
	}

	var resolved bool
	h := Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, resolved = SessionFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.ID.String()})
	rec := httptest.NewRecorder()

	// The bug manifested as a panic recovered upstream into a 500;
	// here an unrecovered panic fails the test directly.
	h.ServeHTTP(rec, req)

	if !resolved {
		t.Fatal("expected the valid session cookie to resolve, got anonymous")
	}
	if cfg.Now == nil {
		t.Fatal("Middleware should have defaulted a nil cfg.Now")
	}
}

// TestAsyncTouchSessionGoroutineRecovers is a guard-coverage test for
// AGT-12: the fire-and-forget `go func(){...}()` inside resolveSession
// that writes TouchSession MUST register a recover(). An unrecovered
// panic in that goroutine terminates the WHOLE API process — nothing
// upstream wraps a bare `go func(){}()` spawned from inside a request
// handler, and the dashboard session-resolve path runs on every
// authenticated request.
//
// Shape rather than a behavioural test, matching
// internal/api/v1/sse_producer_recover_test.go's rationale for the
// same problem class: actually driving a real panic through
// TouchSession and observing "the test process didn't crash" is not a
// safe or deterministic thing to assert inline (an unrecovered panic
// there would take the whole `go test` binary down with it, not just
// fail this one test). Parsing the source and requiring a deferred
// recover() inside the spawned literal is the reliable regression
// guard: it fails on the unfixed code (no recover in the goroutine)
// and passes once the guard is added.
//
// Proven red: deleting the `defer func(){ recover() ... }()` from the
// goroutine in resolveSession fails this test.
func TestAsyncTouchSessionGoroutineRecovers(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "middleware.go", nil, 0)
	if err != nil {
		t.Fatalf("parse middleware.go: %v", err)
	}

	var resolveSessionDecl *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name != nil && d.Name.Name == "resolveSession" {
			resolveSessionDecl = d
			return false
		}
		return true
	})
	if resolveSessionDecl == nil {
		t.Fatal("resolveSession not found in middleware.go — this test has drifted from the code")
	}

	var spawnedLit *ast.FuncLit
	ast.Inspect(resolveSessionDecl, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
				spawnedLit = lit
				return false
			}
		}
		return true
	})
	if spawnedLit == nil {
		t.Fatal("resolveSession no longer spawns a `go func(){...}()` literal for TouchSession — " +
			"this test has drifted from the code")
	}

	if !bodyRegistersRecover(spawnedLit.Body) {
		t.Error("the async TouchSession goroutine in resolveSession does not register a recover(). " +
			"An unrecovered panic in a goroutine terminates the WHOLE process, and this goroutine is " +
			"spawned on every authenticated dashboard request — add a deferred recover() that logs " +
			"and drops only this touch-write (AGT-12).")
	}
}

// bodyRegistersRecover reports whether body contains a deferred call
// whose body invokes recover().
func bodyRegistersRecover(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "recover" {
					found = true
				}
			}
			return true
		})
		return true
	})
	return found
}

// TestTouchTracker_EvictsAgedEntries is the REL-05 regression:
// touchTracker.last must not grow without bound. Without an
// eviction sweep, every distinct session ID ever seen stays in the
// map for the process lifetime — one permanent entry per session.
// Here 50 sessions are each touched once, then time is advanced past
// the debounce interval and a single FRESH session is touched (which
// triggers the opportunistic sweep): all 50 aged-out entries must be
// gone, leaving only the fresh one.
func TestTouchTracker_EvictsAgedEntries(t *testing.T) {
	tr := newTouchTracker(time.Minute)
	base := time.Unix(1_750_000_000, 0)

	const n = 50
	for i := 0; i < n; i++ {
		id := uuid.New()
		if !tr.shouldTouch(id, base) {
			t.Fatalf("session %d: first touch should always be due", i)
		}
	}
	if got := len(tr.last); got != n {
		t.Fatalf("after seeding: touchTracker.last size = %d, want %d", got, n)
	}

	later := base.Add(2 * time.Minute)
	fresh := uuid.New()
	if !tr.shouldTouch(fresh, later) {
		t.Fatal("fresh session's first touch should be due")
	}

	if got := len(tr.last); got != 1 {
		t.Errorf("after sweep: touchTracker.last size = %d, want 1 — "+
			"aged-out entries were not evicted (REL-05: unbounded growth)", got)
	}
}
