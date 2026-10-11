package dashboardauth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestNewHandlers_ValidatesCallerConfigInPlace: one Config is passed to NewHandlers and then to
// Middleware. NewHandlers must validate the caller's struct, not a copy, or validate()'s defaults
// (Generator.Secret, Logger, Now, PasskeyCeremonyGuard) never reach Middleware.
func TestNewHandlers_ValidatesCallerConfigInPlace(t *testing.T) {
	cfg := Config{
		Accounts:         newFakeAccountStore(),
		Users:            newFakeUserStore(),
		Tokens:           newFakeTokenStore(nil),
		Sender:           &notify.NoopSender{},
		DashboardBaseURL: "https://app.stellarindex.io",
		EmailFrom:        "Stellar Index <hello@stellarindex.io>",
	}
	if cfg.PasskeyCeremonyGuard != nil {
		t.Fatal("test setup: PasskeyCeremonyGuard must start nil")
	}

	if _, err := NewHandlers(&cfg); err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}

	// The exact fields validate() defaults (handlers.go), read back
	// off the CALLER's own cfg — not off Handlers.cfg — mirroring
	// main.go's dashboardauth.Middleware(&authCfg) call built from
	// the same variable NewHandlers was given.
	if cfg.PasskeyCeremonyGuard == nil {
		t.Error("caller's Config.PasskeyCeremonyGuard is still nil after NewHandlers — " +
			"validate() defaulted a copy, not this struct; Middleware(&cfg) would run unguarded")
	}
	if cfg.Logger == nil {
		t.Error("caller's Config.Logger is still nil after NewHandlers")
	}
	if cfg.Now == nil {
		t.Error("caller's Config.Now is still nil after NewHandlers")
	}
	if cfg.Generator == nil || len(cfg.Generator.Secret) == 0 {
		t.Error("caller's Config.Generator.Secret is still unset after NewHandlers")
	}
}

// TestAsyncTouchSessionGoroutineRecovers requires the fire-and-forget TouchSession goroutine in
// resolveSession to register a recover(). An unrecovered panic there terminates the whole API
// process, and this path runs on every authenticated request.
//
// It checks source shape, as internal/api/v1/source_shape_rules_internal_test.go does for the same
// problem class, because driving a real panic would take down the whole test binary. Deleting the
// deferred recover() from the goroutine fails this test.
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

// A per-process fallback secret breaks every passkey ceremony that crosses
// instances or a restart with a 400 indistinguishable from tampering, so
// wiring passkeys without a configured secret must fail at construction.
func TestNewHandlers_PasskeysRequireConfiguredSecret(t *testing.T) {
	base := func() Config {
		return Config{
			Accounts:         newFakeAccountStore(),
			Users:            newFakeUserStore(),
			Tokens:           struct{ platform.TokenStore }{},
			Sender:           &notify.NoopSender{},
			Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			DashboardBaseURL: "https://app.stellarindex.io",
			EmailFrom:        "Stellar Index <hello@stellarindex.io>",
		}
	}

	cfg := base()
	cfg.Passkeys = struct {
		platform.WebAuthnCredentialStore
	}{}
	if _, err := NewHandlers(&cfg); err == nil {
		t.Fatal("NewHandlers accepted passkeys with no server secret")
	}

	cfg = base()
	cfg.Passkeys = struct {
		platform.WebAuthnCredentialStore
	}{}
	cfg.Generator = &Generator{Read: NewGenerator().Read, Secret: []byte("configured-secret")}
	if _, err := NewHandlers(&cfg); err != nil {
		t.Fatalf("NewHandlers with passkeys and a secret: %v", err)
	}

	cfg = base()
	if _, err := NewHandlers(&cfg); err != nil {
		t.Fatalf("NewHandlers without passkeys must keep the per-process fallback: %v", err)
	}
	if len(cfg.Generator.Secret) == 0 {
		t.Fatal("fallback secret was not installed")
	}
}
