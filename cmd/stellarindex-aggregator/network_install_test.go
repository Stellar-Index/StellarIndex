package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

func TestInstallCanonicalNetwork_TestnetDerivesTestnetSAC(t *testing.T) {
	t.Cleanup(func() {
		canonical.InstallNetworkPassphrase("")
		canonical.InstallAliasRegistry(nil)
	})
	cfg := config.Config{Stellar: config.StellarConfig{Network: "testnet"}}
	if err := installCanonicalNetwork(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	if got == canonical.XLMSacContractID {
		t.Fatalf("testnet aggregator derived the pubnet XLM SAC %s", got)
	}
}
