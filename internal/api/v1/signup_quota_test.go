// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestSignup_KeyIsMeteredAtFreeTierQuota drives POST /v1/signup through
// the production store and validator and asserts the minted key reaches
// the quota middleware with the free-tier monthly cap, not the 0 that
// middleware.MonthlyQuota treats as unmetered.
func TestSignup_KeyIsMeteredAtFreeTierQuota(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ts := newSignupTestServer(t, auth.NewRedisAPIKeyStore(rdb), newFakeSignupTracker())
	resp := postSignup(t, ts, `{"email":"quota@example.com","label":"app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/signup status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), body.Data.Plaintext)
	if err != nil {
		t.Fatalf("lookup signup key: %v", err)
	}
	if want := platform.TierFree.MaxMonthlyQuota(); sub.MonthlyQuota != want {
		t.Fatalf("signup key MonthlyQuota = %d, want %d (0 ships an unmetered key)", sub.MonthlyQuota, want)
	}
}

// TestCreateAPIKeyRequestSitesNameMonthlyQuota fails when an HTTP mint
// path builds an auth.CreateAPIKeyRequest without naming MonthlyQuota:
// an omitted field persists 0, which the quota middleware reads as
// unmetered, so every mint site must state its cap explicitly.
func TestCreateAPIKeyRequestSitesNameMonthlyQuota(t *testing.T) {
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
