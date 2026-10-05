// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"sync/atomic"
)

// installedPassphrase overrides [PubnetPassphrase] as the network the
// canonical model derives network-dependent values against (today: the
// Stellar Asset Contract address a classic/native asset resolves to via
// [Asset.SacContractID]). A nil pointer means "not installed" → the
// pubnet default is used. Written at most once, at binary start-up,
// before any read/derive path runs (see [InstallNetworkPassphrase]); the
// atomic pointer makes that publish race-free and lets tests swap it.
//
// Same idiom as [InstallAliasRegistry]: a network-derived value is needed
// in leaf positions with no config.Config in scope, so it is published
// once at start-up rather than threaded through every SacContractID
// caller.
var installedPassphrase atomic.Pointer[string]

// InstallNetworkPassphrase publishes p as the process-wide network
// passphrase [Asset.SacContractID] derives against. Call it ONCE, at
// binary start-up, from cfg.Stellar.Passphrase(), after config load and
// before serving. Until installed (unit tests, or a binary that never
// serves canonical SAC addresses) the pubnet passphrase is used, so
// pubnet behaviour is invariant.
//
// An empty p RESETS to the pubnet default (the safe fallback, and the
// reset path unit tests use in t.Cleanup) so a mis-wired config never
// derives SAC addresses against "".
func InstallNetworkPassphrase(p string) {
	if p == "" {
		installedPassphrase.Store(nil)
		installedNativeSAC.Store(nil)
		return
	}
	installedPassphrase.Store(&p)
	sac, err := NativeAsset().sacContractIDOn(p)
	if err != nil {
		installedNativeSAC.Store(nil)
		return
	}
	installedNativeSAC.Store(&sac)
}

// installedNativeSAC caches the native-XLM SAC derived from the installed
// passphrase so hot paths (orientation, MEV grouping) do not rehash per call.
var installedNativeSAC atomic.Pointer[string]

// NativeSACContractID returns the native-XLM Stellar Asset Contract
// address on the installed network, or [XLMSacContractID] when none
// is installed, so an uninstalled binary behaves exactly as pubnet did.
func NativeSACContractID() string {
	if s := installedNativeSAC.Load(); s != nil {
		return *s
	}
	return XLMSacContractID
}

// InstallNetwork publishes the alias registry and the network passphrase
// process-wide. Call once at start-up after config load: without it, SAC
// derivation and the XLM alias family fall back to pubnet on a test net.
func InstallNetwork(passphrase string, sacWrappers map[string]string) error {
	reg, err := NewAliasRegistry(passphrase, sacWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	InstallAliasRegistry(reg)
	InstallNetworkPassphrase(passphrase)
	return nil
}

// NetworkPassphrase returns the installed network passphrase, or
// [PubnetPassphrase] when none is installed. It is the network-aware
// replacement for reading PubnetPassphrase directly in SAC derivation.
func NetworkPassphrase() string {
	if p := installedPassphrase.Load(); p != nil {
		return *p
	}
	return PubnetPassphrase
}
