package config_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

const usdcKey = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func TestTradesConfig_USDPeggedClassics(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if got := (config.TradesConfig{}).USDPeggedClassics(logger); got != nil {
		t.Fatalf("empty list: got %v, want nil", got)
	}

	tc := config.TradesConfig{USDPeggedClassicAssets: []string{"not an asset", usdcKey, "native", "crypto:USDT"}}
	got := tc.USDPeggedClassics(logger)
	if len(got) != 1 {
		t.Fatalf("got %d assets %v, want only the classic peg", len(got), got)
	}
	want, err := canonical.ParseAsset(usdcKey)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != want || got[0].Type != canonical.AssetClassic {
		t.Fatalf("got %+v, want %+v", got[0], want)
	}

	logs := buf.String()
	if n := strings.Count(logs, "skipping malformed entry"); n != 1 {
		t.Errorf("malformed warnings = %d, want 1:\n%s", n, logs)
	}
	if n := strings.Count(logs, "ignoring non-classic asset"); n != 2 {
		t.Errorf("non-classic warnings = %d, want 2:\n%s", n, logs)
	}
}
