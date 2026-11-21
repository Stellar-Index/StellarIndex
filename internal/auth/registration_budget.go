// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import "context"

// registrationBudgetKey is the one key the all-caller budget counts under; it lives only here so no route can count elsewhere.
const registrationBudgetKey = "all"

// RegistrationThrottle is the per-key throttle the budget counts on (the signup-IP throttle's shape).
type RegistrationThrottle interface {
	CheckIP(ctx context.Context, key string) error
}

// RegistrationBudget is the deployment's hourly all-caller account-creation budget: every route that creates an account spends
// one unit of this one counter.
type RegistrationBudget struct{ throttle RegistrationThrottle }

// NewRegistrationBudget binds t to the budget's key; a nil t is no budget (nil).
func NewRegistrationBudget(t RegistrationThrottle) *RegistrationBudget {
	if t == nil {
		return nil
	}
	return &RegistrationBudget{throttle: t}
}

// Spend takes one unit: nil, [ErrSignupRateLimited] when the hour's units are gone, or [ErrThrottleUnavailable] once the store
// has been down past its dwell time. A nil budget is uncapped.
func (b *RegistrationBudget) Spend(ctx context.Context) error {
	if b == nil {
		return nil
	}
	return b.throttle.CheckIP(ctx, registrationBudgetKey)
}
