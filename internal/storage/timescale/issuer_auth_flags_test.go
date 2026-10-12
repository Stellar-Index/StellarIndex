package timescale

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Provenance guards on the issuer auth-flag persist path.

// A merged-away issuer whose pre-image still declares a home domain. A Stellar G-strkey is a
// PUBLIC account address, not a credential; it is hoisted to a constant so `GStrkey:` never
// sits beside a high-entropy literal, which gitleaks' generic-api-key rule keys on.
const mergedIssuer = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"

// TestIssuerAuthFlagsValidate_RefusesADeadAccountsDomain: a merged account's home_domain cannot be checked
// against SEP-1 any more, so persisting it creates an impersonation surface. The lake reader blanks it;
// this is the second guard at the storage boundary.
func TestIssuerAuthFlagsValidate_RefusesADeadAccountsDomain(t *testing.T) {
	asOf := uint32(54564588)
	f := IssuerAuthFlags{
		GStrkey:    mergedIssuer,
		Source:     AuthFlagsSourceLastKnownBeforeRemoval,
		AsOfLedger: &asOf,
		HomeDomain: "stellarbrunch.com",
	}
	err := f.validate()
	if err == nil {
		t.Fatal("validate() = nil, want a refusal — a merged account's self-declared domain is not persistable")
	}
	if !strings.Contains(err.Error(), "stellarbrunch.com") {
		t.Errorf("err = %v, want it to name the offending domain", err)
	}
}

// TestIssuerAuthFlagsValidate_RefusesAHistoricalReadingWithNoAsOf: "these flags are old" is only actionable
// with "as of when". Mirrors migration 0153's second CHECK so the failure names the invariant.
func TestIssuerAuthFlagsValidate_RefusesAHistoricalReadingWithNoAsOf(t *testing.T) {
	f := IssuerAuthFlags{GStrkey: "GA2P", Source: AuthFlagsSourceLastKnownBeforeRemoval}
	if err := f.validate(); err == nil {
		t.Fatal("validate() = nil, want a refusal — a last-known reading with no as-of ledger is unlabelled where it matters")
	}
}

// TestIssuerAuthFlagsValidate_RefusesAnUnknownSource — the enum is closed.
// An unrecognised label would reach the database and be rejected there
// anyway; refusing it here names which issuer and which value.
func TestIssuerAuthFlagsValidate_RefusesAnUnknownSource(t *testing.T) {
	f := IssuerAuthFlags{GStrkey: "GA2P", Source: "guessed"}
	err := f.validate()
	if err == nil {
		t.Fatal("validate() = nil, want a refusal for an unknown source")
	}
	if !strings.Contains(err.Error(), "guessed") {
		t.Errorf("err = %v, want it to name the rejected value", err)
	}
}

// TestIssuerAuthFlagsValidate_RefusesAnAsOfWithNoSource: the two provenance columns move together, and
// PersistIssuerAuthFlags writes neither when Source is empty; an ignored as-of ledger would look recorded.
func TestIssuerAuthFlagsValidate_RefusesAnAsOfWithNoSource(t *testing.T) {
	asOf := uint32(64228661)
	f := IssuerAuthFlags{GStrkey: "GA2P", AsOfLedger: &asOf}
	if err := f.validate(); err == nil {
		t.Fatal("validate() = nil, want a refusal — an as-of ledger with no source would be silently dropped")
	}
}

// TestIssuerAuthFlagsValidate_AcceptsTheThreeLegitimateShapes — a live
// reading with or without an as-of ledger, an unlabelled row (which leaves
// the persisted provenance untouched), and a well-formed historical one.
func TestIssuerAuthFlagsValidate_AcceptsTheThreeLegitimateShapes(t *testing.T) {
	asOf := uint32(64228661)
	for _, tc := range []struct {
		name string
		f    IssuerAuthFlags
	}{
		{"live with as-of", IssuerAuthFlags{Source: AuthFlagsSourceLive, AsOfLedger: &asOf, HomeDomain: "congress-card.org"}},
		{"live without as-of", IssuerAuthFlags{Source: AuthFlagsSourceLive}},
		{"unlabelled", IssuerAuthFlags{HomeDomain: "centre.io"}},
		{"last-known", IssuerAuthFlags{Source: AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: &asOf}},
	} {
		if err := tc.f.validate(); err != nil {
			t.Errorf("%s: validate() = %v, want nil", tc.name, err)
		}
	}
}

// TestIssuerAuthFlagsSourceMirrorsTheMigrationCheck: auth_flags_source has copies that are never colocated,
// so a member missing from one compiles but fails inserts. This pins the store's copy against the migration
// CHECK.
func TestIssuerAuthFlagsSourceMirrorsTheMigrationCheck(t *testing.T) {
	want := []string{AuthFlagsSourceLastKnownBeforeRemoval, AuthFlagsSourceLive}
	sort.Strings(want)

	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"migrations", "0153_issuers_auth_flags_provenance.up.sql")
	mig, err := os.ReadFile(path) //nolint:gosec // path derived from this file's own location
	if err != nil {
		t.Fatalf("read migration 0153: %v", err)
	}
	m := regexp.MustCompile(`auth_flags_source IN \(([^)]+)\)`).FindSubmatch(mig)
	if m == nil {
		t.Fatal("migration 0153 declares no auth_flags_source CHECK — an unlabelled value could be persisted")
	}
	got := []string{}
	for _, p := range strings.Split(strings.ReplaceAll(string(m[1]), "'", ""), ",") {
		if v := strings.TrimSpace(p); v != "" {
			got = append(got, v)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("migration 0153 CHECK = %v, but timescale's constants are %v", got, want)
	}
}
