//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ClassicCodesFoldingTo over real SQL: per key, the issuer's spellings equal ignoring case (the key's own included), never
// another issuer's, capped at three.
func TestClassicCodesFoldingTo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		other  = "GAS4V4XZ3JHFTGKHCTMTWIIVFHGLBUMSQMGZM4RIWYPQHNXBAOGZBKHR"
	)
	now := time.Now().UTC()
	for i, a := range []struct{ code, issuer string }{
		{"USDC", issuer},
		{"usdc", issuer},
		{"USTRY", issuer},
		{"ustry", other},
		{"AAA", issuer},
		{"aaa", issuer},
		{"Aaa", issuer},
		{"aAa", issuer},
	} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO classic_assets
			    (asset_id, code, issuer_g_strkey, slug,
			     first_seen_at, first_seen_ledger, last_seen_at, last_seen_ledger, observation_count)
			VALUES ($1, $2, $3, $4, $5, 1, $5, 100, 1)`,
			a.code+"-"+a.issuer, a.code, a.issuer, "fold-"+string(rune('a'+i)), now); err != nil {
			t.Fatalf("insert %s: %v", a.code, err)
		}
	}

	keys := []timescale.ClassicCodeKey{
		{Issuer: issuer, Code: "usdc"}, {Issuer: issuer, Code: "Ustry"}, {Issuer: issuer, Code: "AAA"}, {Issuer: issuer, Code: "EURC"},
	}
	got, err := store.ClassicCodesFoldingTo(ctx, keys)
	if err != nil {
		t.Fatalf("ClassicCodesFoldingTo: %v", err)
	}
	for k, want := range map[timescale.ClassicCodeKey][]string{
		keys[0]: {"USDC", "usdc"}, keys[1]: {"USTRY"}, keys[3]: nil,
	} {
		if !slices.Equal(got[k], want) {
			t.Errorf("%s: got %v, want %v", k.Code, got[k], want)
		}
	}
	if n := len(got[keys[2]]); n != 3 {
		t.Errorf("AAA: %d spellings, want the cap of 3 (%v)", n, got[keys[2]])
	}
}
