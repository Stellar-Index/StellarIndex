package v1_test

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// keyClampCase is one account-adjusted budget: a child key minted while its override was raised holds the raised value
// as its own, so clearing the override must lower that key in both stores.
type keyClampCase struct {
	account  func(*platform.Account)
	patch    string
	pgKey    platform.APIKey
	redisKey auth.APIKeyRecord
	check    func(t *testing.T, pg platform.APIKey, redis keyBudgetUpdateCall)
}

// Every *Override field on platform.Account must have a row: an override added without the key clamp leaves keys minted
// under it holding more than the account has once the override is cut.
func TestAdminAccountOverrides_EveryOverrideClampsChildKeysWhenCleared(t *testing.T) {
	const freeRate, freeQuota = 6000, 1_000_000
	if platform.TierFree.MaxRateLimitPerMin() != freeRate || platform.TierFree.MaxMonthlyQuota() != freeQuota {
		t.Fatalf("tier ladder moved: free = %d/min, %d/month", platform.TierFree.MaxRateLimitPerMin(), platform.TierFree.MaxMonthlyQuota())
	}
	ident := auth.AccountIdentifier("boosted")
	cases := map[string]keyClampCase{
		"RateLimitPerMinOverride": {
			account: func(a *platform.Account) { a.RateLimitPerMinOverride = 60_000 },
			patch:   `{"rate_limit_per_min_override":0}`,
			pgKey:   platform.APIKey{ID: "pg_child", RateLimitPerMin: 60_000, KeyHash: []byte{0xa1}},
			redisKey: auth.APIKeyRecord{
				KeyID: "rk_child", Identifier: ident, ParentKeyID: "rk_parent", RateLimitPerMin: 60_000,
			},
			check: func(t *testing.T, pg platform.APIKey, r keyBudgetUpdateCall) {
				if pg.RateLimitPerMin != freeRate || r.rateLimit != freeRate {
					t.Errorf("child rate = pg %d / redis %d, want both %d", pg.RateLimitPerMin, r.rateLimit, freeRate)
				}
			},
		},
		"MonthlyRequestQuotaOverride": {
			account: func(a *platform.Account) { a.MonthlyRequestQuotaOverride = 5_000_000 },
			patch:   `{"monthly_request_quota_override":0}`,
			pgKey:   platform.APIKey{ID: "pg_child", RateLimitPerMin: 30, MonthlyQuota: 5_000_000, KeyHash: []byte{0xa2}},
			redisKey: auth.APIKeyRecord{
				KeyID: "rk_child", Identifier: ident, ParentKeyID: "rk_parent", RateLimitPerMin: 30, MonthlyQuota: 5_000_000,
			},
			check: func(t *testing.T, pg platform.APIKey, r keyBudgetUpdateCall) {
				if pg.MonthlyQuota != freeQuota || r.quota != freeQuota {
					t.Errorf("child quota = pg %d / redis %d, want both %d", pg.MonthlyQuota, r.quota, freeQuota)
				}
			},
		},
	}

	at := reflect.TypeFor[platform.Account]()
	for i := range at.NumField() {
		if name := at.Field(i).Name; strings.HasSuffix(name, "Override") {
			if _, ok := cases[name]; !ok {
				t.Errorf("platform.Account.%s has no clamp case: clear it under a key minted at the raised value and assert the key drops", name)
			}
		}
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			acctID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
			acct := platform.Account{ID: acctID, Slug: "boosted", Tier: platform.TierFree, Status: platform.AccountActive}
			tc.account(&acct)
			pgKey := tc.pgKey
			pgKey.AccountID = acctID
			pgKeys := &fakePlatformAPIKeysForBridge{byAcct: map[uuid.UUID][]platform.APIKey{acctID: {pgKey}}}
			redisKeys := &fakeSelfServiceKeyManager{keys: map[string][]auth.APIKeyRecord{ident: {tc.redisKey}}}
			ts := newAdminClampServer(t, newFakePlatformAccountStore(acct),
				v1.APIKeyBudgetStores{Platform: pgKeys, Redis: redisKeys}, &recordingAuditSink{})

			resp := patchJSON(t, ts.URL+"/v1/admin/accounts/"+acctID.String(), "override ended", tc.patch)
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
			}
			if len(pgKeys.updates) != 1 || len(redisKeys.updates) != 1 {
				t.Fatalf("updates: platform=%+v redis=%+v, want the child lowered once in each store", pgKeys.updates, redisKeys.updates)
			}
			tc.check(t, pgKeys.updates[0], redisKeys.updates[0])
		})
	}
}
