// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// middleware.ClampMintScopes is "the single chokepoint every mint path funnels through", so that
// "a credential can never mint a more-privileged credential than itself" holds by construction.
// POST /v1/admin/keys must go through it too, not gate on tier alone and pass the requested
// scopes straight into the store.
//
// The escalation is real: this handler mints `tier: operator` keys WITH an explicit scope list,
// so a narrowed operator credential exists, and an empty scope list means every capability
// (checkScopes short-circuits on len(Scopes)==0). A staff key minted as operator/["admin"],
// able to drive /v1/admin/* but not to read customer data, could POST scopes:[] and mint itself
// full access.
//
// Both directions are asserted on the STORE's received request, not on the status code: a 201
// that persists the wrong scope set is the bug.

// narrowedOperatorSubject is an operator credential confined to the
// /v1/admin/* family — the shape the admin mint path itself can issue.
func narrowedOperatorSubject() auth.Subject {
	s := operatorSubject()
	s.Scopes = []string{platform.KeyScopeAdmin}
	return s
}
