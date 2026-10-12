package cachekeys_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestAPIKeyIndex pins the wire strings of the API-key index family. Both keys sit under `apikey-index:` so one Redis ACL pattern
// (`~apikey-index:*`) admits the index and its build lock; neither sits under `apikey:`, whose credential walk GETs every hit and
// would fail every fallback lookup with WRONGTYPE on a HASH.
func TestAPIKeyIndex(t *testing.T) {
	if got, want := cachekeys.APIKeyIndex().String(), "apikey-index:v1"; got != want {
		t.Errorf("APIKeyIndex = %q, want %q", got, want)
	}
	if got, want := cachekeys.APIKeyIndexBuildLock().String(), "apikey-index:build-lock"; got != want {
		t.Errorf("APIKeyIndexBuildLock = %q, want %q", got, want)
	}
	recordPrefix := cachekeys.APIKey("").String()
	for _, k := range []string{cachekeys.APIKeyIndex().String(), cachekeys.APIKeyIndexBuildLock().String()} {
		if !strings.HasPrefix(k, "apikey-index:") {
			t.Errorf("%q is outside the apikey-index: family the ACL pattern admits", k)
		}
		if strings.HasPrefix(k, recordPrefix) {
			t.Errorf("%q is inside the %q record namespace the credential walk GETs", k, recordPrefix)
		}
	}
}
