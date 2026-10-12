// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"sync/atomic"
)

// installedPassphrase overrides [PubnetPassphrase] as the network [Asset.SacContractID] derives against; nil means pubnet. Written
// once at start-up ([InstallNetworkPassphrase]); the atomic pointer keeps that race-free and lets tests swap it. Same idiom as
// [InstallAliasRegistry]: leaf callers have no config.Config in scope.
var installedPassphrase atomic.Pointer[string]

// InstallNetworkPassphrase publishes p as the process-wide passphrase [Asset.SacContractID] derives against; call ONCE at start-up, pubnet until then, and an empty p resets to pubnet.
func InstallNetworkPassphrase(p string) {
	if p == "" {
		installedPassphrase.Store(nil)
		installedNativeSAC.Store(nil)
		networkBaseRegistry.Store(nil)
		return
	}
	installedPassphrase.Store(&p)
	sac, err := NativeAsset().sacContractIDOn(p)
	if err != nil {
		installedNativeSAC.Store(nil)
		networkBaseRegistry.Store(nil)
		return
	}
	installedNativeSAC.Store(&sac)
	networkBaseRegistry.Store(&AliasRegistry{families: baseAliasFamiliesFor(sac)})
}

// installedNativeSAC caches the native-XLM SAC derived from the installed
// passphrase so hot paths (orientation, MEV grouping) do not rehash per call.
var installedNativeSAC atomic.Pointer[string]

// NativeSACContractID returns the native-XLM SAC address on the installed network, or [XLMSacContractID] when none is installed.
func NativeSACContractID() string {
	if s := installedNativeSAC.Load(); s != nil {
		return *s
	}
	return XLMSacContractID
}

// InstallNetwork publishes the alias registry and network passphrase process-wide; call once after config load, else SAC derivation and the XLM alias family fall back to pubnet.
func InstallNetwork(passphrase string, sacWrappers map[string]string) error {
	reg, err := NewAliasRegistry(passphrase, sacWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	InstallAliasRegistry(reg)
	InstallNetworkPassphrase(passphrase)
	return nil
}

// NetworkPassphrase returns the installed network passphrase, or [PubnetPassphrase] when none is installed.
func NetworkPassphrase() string {
	if p := installedPassphrase.Load(); p != nil {
		return *p
	}
	return PubnetPassphrase
}
