// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// [New] requires its store to answer the account kill switch ([AccountStatusReader]), so the
// white-box fakes, which model only webhook tables, report every account active. The kill
// switch itself is pinned in worker_test.go (delivery path) and
// test/integration/pg_auth_webhooks_test.go (SQL).

// WebhookAccountStatus completes the DeliveryStore contract for nopStore.
func (nopStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}

// WebhookAccountStatus completes the DeliveryStore contract for ctxHonouringStore. It does
// not model a store round-trip: the lifetime tests turn on the attempt context expiring
// inside GetWebhook, and a second delay here would move the deadline they pin.
func (*ctxHonouringStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}
