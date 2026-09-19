// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SEC-06 / RLT-420: [New] requires its store to answer the account kill
// switch ([AccountStatusReader]), so the package's white-box fakes —
// which model the webhook tables only and never a suspended account —
// complete the contract here by reporting every account active. The
// kill switch's own behaviour is pinned in account_killswitch_test.go
// (delivery path) and in
// test/integration/customerwebhook_account_killswitch_test.go (SQL).

// WebhookAccountStatus completes the DeliveryStore contract for nopStore.
func (nopStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}

// WebhookAccountStatus completes the DeliveryStore contract for
// ctxHonouringStore. Unlike its GetWebhook this does not model a store
// round-trip: the K025 lifetime tests turn on the attempt context
// expiring inside GetWebhook, and a second delay here would move the
// deadline they pin.
func (*ctxHonouringStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}
