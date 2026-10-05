package main

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// installCanonicalNetwork publishes the alias registry and the network
// passphrase process-wide. Without the passphrase, SAC derivation (price-alert
// scam gate, holds, webhook withholding) falls back to pubnet on a test net.
func installCanonicalNetwork(cfg config.Config) error {
	aliasRegistry, err := canonical.NewAliasRegistry(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	canonical.InstallAliasRegistry(aliasRegistry)
	canonical.InstallNetworkPassphrase(cfg.Stellar.Passphrase())
	return nil
}
