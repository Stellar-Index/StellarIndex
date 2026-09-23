package platform

import "testing"

// TestTierCanonical pins the legacy-string mapping the free-platform
// model relies on: stored rows (and operator PATCH bodies) may still
// carry the migration-0027 five-string vocabulary, and every one of
// them must fold into exactly {anon, free, partner}.
func TestTierCanonical(t *testing.T) {
	cases := []struct {
		in   Tier
		want Tier
	}{
		{TierAnon, TierAnon},
		{TierFree, TierFree},
		{TierPartner, TierPartner},
		// Legacy paid plans: starter folds to free (identical
		// numbers); everything above folds to partner.
		{TierStarter, TierFree},
		{TierPro, TierPartner},
		{TierBusiness, TierPartner},
		{TierEnterprise, TierPartner},
		// Corrupt / unknown values fail closed to free.
		{Tier(""), TierFree},
		{Tier("platinum"), TierFree},
	}
	for _, c := range cases {
		if got := c.in.Canonical(); got != c.want {
			t.Errorf("Tier(%q).Canonical() = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTierStorageValue pins the write-side mapping onto the untouched
// migration-0027 CHECK constraint, including the round-trip property
// Canonical(StorageValue(t)) == Canonical(t) (anon folds to free —
// it is never an account tier).
func TestTierStorageValue(t *testing.T) {
	cases := []struct {
		in   Tier
		want string
	}{
		{TierFree, "free"},
		{TierStarter, "free"},
		{TierAnon, "free"},
		{Tier("garbage"), "free"},
		{TierPartner, "enterprise"},
		{TierPro, "enterprise"},
		{TierBusiness, "enterprise"},
		{TierEnterprise, "enterprise"},
	}
	for _, c := range cases {
		got := c.in.StorageValue()
		if got != c.want {
			t.Errorf("Tier(%q).StorageValue() = %q, want %q", c.in, got, c.want)
		}
		// CHECK-constraint legality: must be one of the five legacy
		// strings migration 0027 accepts.
		switch got {
		case "free", "starter", "pro", "business", "enterprise":
		default:
			t.Errorf("Tier(%q).StorageValue() = %q — not CHECK-legal under migration 0027", c.in, got)
		}
		if c.in != TierAnon {
			if rt := Tier(got).Canonical(); rt != c.in.Canonical() {
				t.Errorf("round-trip broken: Tier(%q) → storage %q → canonical %q, want %q",
					c.in, got, rt, c.in.Canonical())
			}
		}
	}
}

// TestTierLadders pins the three-level default numbers:
//
//	anon    — the unauthenticated per-IP baseline (60/min, no keys)
//	free    — the registered default, anchored to the old Starter plan
//	partner — ceilings only; staff-set per-account overrides are the
//	          real limits (old Enterprise numbers)
//
// Legacy strings must resolve through Canonical() to the same rungs.
func TestTierLadders(t *testing.T) {
	type row struct {
		tier     Tier
		rate     int
		keys     int
		monthly  int64
		webhooks int
		alerts   int
	}
	rows := []row{
		{TierAnon, 60, 0, 0, 0, 0},
		{TierFree, 1000, 25, 1_000_000, 10, 25},
		{TierPartner, 100_000, 250, 1_000_000_000, 100, 1000},
		// Legacy mapping: starter ≡ free, pro/business/enterprise ≡ partner.
		{TierStarter, 1000, 25, 1_000_000, 10, 25},
		{TierPro, 100_000, 250, 1_000_000_000, 100, 1000},
		{TierBusiness, 100_000, 250, 1_000_000_000, 100, 1000},
		{TierEnterprise, 100_000, 250, 1_000_000_000, 100, 1000},
		// Corrupt rows fail closed to free.
		{Tier("corrupt"), 1000, 25, 1_000_000, 10, 25},
	}
	for _, r := range rows {
		if got := r.tier.MaxRateLimitPerMin(); got != r.rate {
			t.Errorf("Tier(%q).MaxRateLimitPerMin() = %d, want %d", r.tier, got, r.rate)
		}
		if got := r.tier.MaxActiveKeys(); got != r.keys {
			t.Errorf("Tier(%q).MaxActiveKeys() = %d, want %d", r.tier, got, r.keys)
		}
		if got := r.tier.MaxMonthlyQuota(); got != r.monthly {
			t.Errorf("Tier(%q).MaxMonthlyQuota() = %d, want %d", r.tier, got, r.monthly)
		}
		if got := r.tier.MaxWebhooks(); got != r.webhooks {
			t.Errorf("Tier(%q).MaxWebhooks() = %d, want %d", r.tier, got, r.webhooks)
		}
		if got := r.tier.MaxPriceAlerts(); got != r.alerts {
			t.Errorf("Tier(%q).MaxPriceAlerts() = %d, want %d", r.tier, got, r.alerts)
		}
	}
}

// TestAccountEffectiveRateLimitPerMin pins GH-1074: a partner account
// comped BELOW the tier ceiling must read its comped limit, not the
// ceiling — the override is a floor that only ever RAISES the
// effective limit above the tier default, so a low override (a
// customer's actual paid-for limit) still reports the higher tier
// ceiling if this regresses to "always show the ceiling".
func TestAccountEffectiveRateLimitPerMin(t *testing.T) {
	cases := []struct {
		name     string
		tier     Tier
		override int
		want     int
	}{
		{"free, no override", TierFree, 0, 1000},
		{"partner, no override: ceiling", TierPartner, 0, 100_000},
		{"partner, override below ceiling: still the ceiling (override only raises)", TierPartner, 5000, 100_000},
		{"free, override above tier default: raised to the override", TierFree, 50_000, 50_000},
	}
	for _, c := range cases {
		a := Account{Tier: c.tier, RateLimitPerMinOverride: c.override}
		if got := a.EffectiveRateLimitPerMin(); got != c.want {
			t.Errorf("%s: EffectiveRateLimitPerMin() = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestAccountEffectiveMonthlyQuota pins GH-1074's quota half: the
// override is a ceiling that only ever LOWERS the effective quota
// below the tier default.
func TestAccountEffectiveMonthlyQuota(t *testing.T) {
	cases := []struct {
		name     string
		tier     Tier
		override int64
		want     int64
	}{
		{"free, no override", TierFree, 0, 1_000_000},
		{"partner, no override: ceiling", TierPartner, 0, 1_000_000_000},
		{"partner, override below ceiling: lowered to the override", TierPartner, 200_000, 200_000},
		{"free, override above tier default: stays at the ceiling (override only lowers)", TierFree, 5_000_000, 1_000_000},
	}
	for _, c := range cases {
		a := Account{Tier: c.tier, MonthlyRequestQuotaOverride: c.override}
		if got := a.EffectiveMonthlyQuota(); got != c.want {
			t.Errorf("%s: EffectiveMonthlyQuota() = %d, want %d", c.name, got, c.want)
		}
	}
}
