// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// middleware.ClampMintScopes' own doc calls itself "the single
// chokepoint every mint path funnels through" so that "a credential can
// never mint a more-privileged credential than itself" holds by
// construction. That was true of the CUSTOMER path only: POST
// /v1/admin/keys gated on tier alone and passed the requested scopes
// straight into the store.
//
// The escalation is closed, not theoretical: this very handler mints
// `tier: operator` keys WITH an explicit scope list, so a narrowed
// operator credential exists by construction — and an empty scope list
// means every capability (checkScopes short-circuits on
// len(Scopes)==0). A staff key minted as operator/["admin"] —
// deliberately able to drive /v1/admin/* but not to read customer data —
// could POST scopes:[] here and mint itself full access, with a
// best-effort audit row as the only signal.
//
// Both directions are asserted on the STORE's received request, not on
// the status code: a 201 that persists the wrong scope set is the bug.

// narrowedOperatorSubject is an operator credential confined to the
// /v1/admin/* family — the shape the admin mint path itself can issue.
func narrowedOperatorSubject() auth.Subject {
	s := operatorSubject()
	s.Scopes = []string{platform.KeyScopeAdmin}
	return s
}
