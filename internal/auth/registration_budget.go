// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import "context"

// The budget's keys live only here so no route can count elsewhere. Proof-free creations (POST /v1/register, passkey
// sign-up) share one; an email-proven first sign-in has its own, so the proof-free routes cannot drain it.
const (
	registrationBudgetKey            = "all"
	registrationBudgetEmailProvenKey = "email-proven"
)

// RegistrationThrottle is the per-key throttle the budget counts on (the signup-IP throttle's shape).
type RegistrationThrottle interface {
	CheckIP(ctx context.Context, key string) error
}

// RegistrationBudget is the deployment's hourly account-creation budget: every route that creates an account spends one
// unit, proof-free routes from one counter and email-proven sign-ups (see [RegistrationBudget.EmailProven]) from another.
type RegistrationBudget struct {
	throttle RegistrationThrottle
	key      string
}

// NewRegistrationBudget binds t to the proof-free key; a nil t is no budget (nil).
func NewRegistrationBudget(t RegistrationThrottle) *RegistrationBudget {
	if t == nil {
		return nil
	}
	return &RegistrationBudget{throttle: t, key: registrationBudgetKey}
}

// EmailProven is the budget for an account whose owner has just proven control of its email address, on the same
// throttle and cap under its own key. A nil budget stays nil (uncapped).
func (b *RegistrationBudget) EmailProven() *RegistrationBudget {
	if b == nil {
		return nil
	}
	return &RegistrationBudget{throttle: b.throttle, key: registrationBudgetEmailProvenKey}
}

// Spend takes one unit: nil, [ErrSignupRateLimited] when the hour's units are gone, or [ErrThrottleUnavailable] once the store
// has been down past its dwell time. A nil budget is uncapped.
func (b *RegistrationBudget) Spend(ctx context.Context) error {
	if b == nil {
		return nil
	}
	return b.throttle.CheckIP(ctx, b.key)
}
