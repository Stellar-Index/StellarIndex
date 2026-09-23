package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestOpenAPIAccountTierEnumMatchesAuthTier pins Account.tier's documented
// enum to the values the Account schema can actually carry: every auth.Tier
// constant except TierAnonymous. /v1/account/me and /v1/account/keys serve
// string(Tier) verbatim, and both 401 an anonymous caller before building
// an Account (asserted below), so `anonymous` in the enum is a documented
// value no response can hold. The constants are read from source so a new
// tier fails here until the spec names it.
func TestOpenAPIAccountTierEnumMatchesAuthTier(t *testing.T) {
	var want []string
	for _, tier := range goStringConsts(t, filepath.Join("internal", "auth"), "Tier") {
		if tier != string(auth.TierAnonymous) {
			want = append(want, tier)
		}
	}
	if len(want) == 0 {
		t.Fatal("found no non-anonymous auth.Tier constants — the source walk is broken")
	}

	got := specEnumAt(t, loadSpecDoc(t), "components", "schemas", "Account", "properties", "tier")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi Account.tier enum = %v, want %v (every auth.Tier except %q) — "+
			"account.go serves string(Tier) directly, so an undocumented or "+
			"unreachable enum value here is a client-visible contract gap",
			got, want, auth.TierAnonymous)
	}
}

// TestAccountSchemaRoutesRejectAnonymousTier holds the premise that keeps
// `anonymous` out of the Account.tier enum: an explicitly anonymous
// subject never reaches a response built from the Account schema.
func TestAccountSchemaRoutesRejectAnonymousTier(t *testing.T) {
	anon := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s := auth.Subject{Tier: auth.TierAnonymous, Identifier: "192.0.2.1"}
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), s)))
		})
	}
	h := New(Options{Auth: anon}).Handler()
	for _, path := range []string{"/v1/account/me", "/v1/account/keys"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s as tier %q = %d, want 401", path, auth.TierAnonymous, rec.Code)
		}
	}
}
