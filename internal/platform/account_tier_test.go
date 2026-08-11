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
