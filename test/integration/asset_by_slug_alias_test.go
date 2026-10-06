//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GetAssetBySlug must resolve an asset_id through every canonical alias
// form: a configured SAC wrapper id finds the classic catalogue row.
func TestGetAssetBySlug_ResolvesAliasForms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		classic = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sac     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	)
	reg, err := c.NewAliasRegistry(c.PubnetPassphrase, map[string]string{sac: classic})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	c.InstallAliasRegistry(reg)
	t.Cleanup(func() { c.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDirectoryAsset(t, ctx, store.DB(), classic, "USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "usdc")

	for _, id := range []string{classic, sac} {
		row, err := store.GetAssetBySlug(ctx, id)
		if err != nil {
			t.Fatalf("GetAssetBySlug(%s): %v", id, err)
		}
		if row.AssetID != classic {
			t.Errorf("GetAssetBySlug(%s).AssetID = %s, want %s", id, row.AssetID, classic)
		}
	}
}
