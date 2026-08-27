// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "sync/atomic"

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
		return
	}
	installedPassphrase.Store(&p)
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
