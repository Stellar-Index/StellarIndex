// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http/httptest"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// newAdminClampServer wires the admin surface WITH the key-budget stores
// (production wires these in
// cmd/stellarindex-api, and the handler must actually use them).
func newAdminClampServer(
	t *testing.T,
	accounts v1.PlatformAccountStore,
	budgets v1.APIKeyBudgetStores,
	sink v1.AuditSink,
) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(operatorSubject()),
		PlatformAccounts: accounts,
		APIKeyBudgets:    budgets,
		Audit:            sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}
