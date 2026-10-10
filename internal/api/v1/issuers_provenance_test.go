package v1_test

// Auth-flag provenance on /v1/issuers/{g_strkey}.
//
// ~10.2k of ~59.2k known issuers (r1) have merged their account
// away. Their auth flags ARE recoverable from the last state before removal,
// but a recovered value is a HISTORICAL RECORD, not the issuer's current
// authorisation policy — so the wire has to say which it is, and the read
// path must never let a historical reading freeze in place.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const provenanceIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestIssuerAuthFlagsSourceIsLockstepped — `auth_flags_source` is an
// enumerated string set with copies that are never colocated: the Go
// constants, the published OpenAPI enum, and the Postgres CHECK in migration
// 0153. Adding a member to one and not the others compiles green while the
// database rejects every write and the spec-derived clients cannot express
// the value, so the copies are reconciled here.
func TestIssuerAuthFlagsSourceIsLockstepped(t *testing.T) {
	want := []string{
		string(clickhouse.AuthFlagsSourceLastKnownBeforeRemoval),
		string(clickhouse.AuthFlagsSourceLive),
	}
	sort.Strings(want)

	root := moduleRoot(t)

	spec, err := os.ReadFile(filepath.Join(root, "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path resolved above
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	specEnum := regexp.MustCompile(`auth_flags_source:\n\s+type: string\n\s+enum: \[([^\]]+)\]`).FindSubmatch(spec)
	if specEnum == nil {
		t.Fatal("openapi/stellar-index.v1.yaml declares no auth_flags_source enum — the served field is undocumented")
	}
	if got := splitCommaSorted(string(specEnum[1])); !reflect.DeepEqual(got, want) {
		t.Errorf("OpenAPI enum = %v, want %v", got, want)
	}

	mig, err := os.ReadFile(filepath.Join(root, "migrations", "0153_issuers_auth_flags_provenance.up.sql")) //nolint:gosec // repo-relative path resolved above
	if err != nil {
		t.Fatalf("read migration 0153: %v", err)
	}
	migEnum := regexp.MustCompile(`auth_flags_source IN \(([^)]+)\)`).FindSubmatch(mig)
	if migEnum == nil {
		t.Fatal("migration 0153 declares no auth_flags_source CHECK — an unlabelled value could be persisted")
	}
	if got := splitCommaSorted(strings.ReplaceAll(string(migEnum[1]), "'", "")); !reflect.DeepEqual(got, want) {
		t.Errorf("migration 0153 CHECK = %v, want %v", got, want)
	}
}

// moduleRoot walks up from the test's working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root from cwd")
	return ""
}

func splitCommaSorted(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
