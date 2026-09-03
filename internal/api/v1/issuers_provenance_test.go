package v1_test

// Auth-flag provenance on /v1/issuers/{g_strkey} (#374).
//
// ~10.2k of ~59.2k known issuers (r1, 2026-09-02) have merged their account
// away. Their auth flags ARE recoverable from the last state before removal,
// but a recovered value is a HISTORICAL RECORD, not the issuer's current
// authorisation policy — so the wire has to say which it is, and the read
// path must never let a historical reading freeze in place.

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const provenanceIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestHandleIssuer_LiveAccountStateStampsLiveProvenance — when the enrichment
// resolves a LIVE AccountEntry, the response must say so and pin the ledger
// the reading is true as of. Without that a consumer cannot tell a current
// policy from a last-known one, which is the whole point of being able to
// resolve the merged issuers at all.
func TestHandleIssuer_LiveAccountStateStampsLiveProvenance(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{GStrkey: provenanceIssuer}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0xA, // AUTH_REVOCABLE | AUTH_CLAWBACK
		HomeDomain:         "live-onchain.example",
		LastModifiedLedger: 64100000,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q", env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64100000 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64100000 (the entry's last-modified ledger)",
			env.Data.AuthFlagsAsOfLedger)
	}
	if env.Data.AuthRequired == nil || *env.Data.AuthRequired {
		t.Errorf("auth_required = %v, want false (mask 0xA)", env.Data.AuthRequired)
	}
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("auth_revocable = %v, want true (mask 0xA)", env.Data.AuthRevocable)
	}
	if env.Data.AuthClawback == nil || !*env.Data.AuthClawback {
		t.Errorf("auth_clawback = %v, want true (mask 0xA)", env.Data.AuthClawback)
	}
}

// TestHandleIssuer_LiveAccountEntryOutranksPersistedFlags — the re-creation
// case. Once the drain can persist a merged issuer's last-known flags, a row
// whose account has been RE-CREATED on-chain must resolve back to its live
// values and be labelled `live`. The account's own current AccountEntry is
// the authority on its own policy; the persisted column is a cache of it, and
// the drain's queue (`auth_required IS NULL`) will never revisit the row to
// correct it.
func TestHandleIssuer_LiveAccountEntryOutranksPersistedFlags(t *testing.T) {
	stale := true
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:       provenanceIssuer,
		AuthRequired:  &stale,
		AuthRevocable: &stale,
		AuthImmutable: &stale,
		AuthClawback:  &stale,
	}}
	// Re-created account: every flag cleared.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0,
		LastModifiedLedger: 64212818,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	for _, f := range []struct {
		name string
		got  *bool
	}{
		{"auth_required", env.Data.AuthRequired},
		{"auth_revocable", env.Data.AuthRevocable},
		{"auth_immutable", env.Data.AuthImmutable},
		{"auth_clawback", env.Data.AuthClawback},
	} {
		if f.got == nil || *f.got {
			t.Errorf("%s = %v, want false — the live AccountEntry outranks the persisted value", f.name, f.got)
		}
	}
	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q", env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64212818 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64212818", env.Data.AuthFlagsAsOfLedger)
	}
}

// TestHandleIssuer_NoLiveEntryNeverClaimsProvenance — absence from the
// current-state projection is what a MERGED account and a lake-coverage gap
// BOTH look like (r1's projection holds no `removed` row below ledger
// 38,000,000 at all, so an account merged before that is simply missing). The
// read path may therefore neither upgrade a persisted reading to `live` nor
// conclude `last_known_before_removal` on its own: it leaves the flags alone
// and says nothing about them. Only the drain, which reads an actual
// `removed` row and its removal ledger, may write the historical label.
func TestHandleIssuer_NoLiveEntryNeverClaimsProvenance(t *testing.T) {
	persisted := true
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:      provenanceIssuer,
		AuthRequired: &persisted,
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: false}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "" {
		t.Errorf("auth_flags_source = %q, want empty — no live entry resolved, so nothing is known about provenance",
			env.Data.AuthFlagsSource)
	}
	if env.Data.AuthFlagsAsOfLedger != nil {
		t.Errorf("auth_flags_as_of_ledger = %v, want absent", env.Data.AuthFlagsAsOfLedger)
	}
	if env.Data.AuthRequired == nil || !*env.Data.AuthRequired {
		t.Errorf("auth_required = %v, want the persisted true left untouched", env.Data.AuthRequired)
	}
}

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
