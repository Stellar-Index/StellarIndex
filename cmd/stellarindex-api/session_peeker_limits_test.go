package main

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestSessionPeekerAdapter_ServesEnforcedDefaultKeyLimits pins GH-1074
// on the production adapter behind /v1/account/me: a partner comped to
// 5,000/min must read 5,000 (what auth enforces on its default-minted
// keys), and an un-comped partner 1,000 — never the 100,000 tier ceiling.
func TestSessionPeekerAdapter_ServesEnforcedDefaultKeyLimits(t *testing.T) {
	cases := []struct {
		name      string
		acct      platform.Account
		wantRate  int
		wantQuota int64
	}{
		{
			"partner comped to 5000/min",
			platform.Account{Tier: platform.TierPartner, RateLimitPerMinOverride: 5000, MonthlyRequestQuotaOverride: 200_000},
			5000, 200_000,
		},
		{
			"partner without overrides",
			platform.Account{Tier: platform.TierPartner},
			1000, platform.TierPartner.MaxMonthlyQuota(),
		},
	}
	for _, tc := range cases {
		ctx := dashboardauth.WithSession(context.Background(), dashboardauth.SessionContext{Account: tc.acct})
		info, ok := sessionPeekerAdapter{}.SessionFromContext(ctx)
		if !ok {
			t.Fatalf("%s: adapter found no session", tc.name)
		}
		if info.AccountRateLimitPerMin != tc.wantRate {
			t.Errorf("%s: AccountRateLimitPerMin = %d, want %d", tc.name, info.AccountRateLimitPerMin, tc.wantRate)
		}
		if info.AccountMonthlyRequestQuota != tc.wantQuota {
			t.Errorf("%s: AccountMonthlyRequestQuota = %d, want %d", tc.name, info.AccountMonthlyRequestQuota, tc.wantQuota)
		}
	}
}
