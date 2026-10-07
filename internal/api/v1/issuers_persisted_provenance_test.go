package v1_test

// The PERSISTED half of auth-flag provenance on /v1/issuers/{g_strkey}.
//
// issuers_provenance_test.go pins what the read path CONCLUDES from a live
// AccountEntry. These pin what it does with what the drain already wrote —
// the half that was latent until handleIssuer carried IssuerRow's provenance
// into the response.

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A real r1 residue issuer: merged away at ledger 54,564,588, its pre-image
// still declaring `stellarbrunch.com`.
const mergedIssuerG = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"

func u32(v uint32) *uint32 { return &v }

func boolPtr(v bool) *bool { return &v }

// TestHandleIssuer_ServesThePersistedProvenance — a recovered reading is only
// safe to serve because it arrives LABELLED. If the handler drops the label,
// a client sees four auth flags with no way to tell that they describe an
// account which has been removed, which is a quieter defect than the
// unresolved row it replaced.
func TestHandleIssuer_ServesThePersistedProvenance(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		AuthRequired:        boolPtr(false),
		AuthRevocable:       boolPtr(true),
		AuthImmutable:       boolPtr(false),
		AuthClawback:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLastKnownBeforeRemoval),
		AuthFlagsAsOfLedger: u32(54564588),
	}}
	// The account really is gone, so the live reader resolves nothing and
	// the persisted reading must stand — labelled.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: false}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+mergedIssuerG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "last_known_before_removal" {
		t.Errorf("auth_flags_source = %q, want %q — the persisted label must reach the wire",
			env.Data.AuthFlagsSource, "last_known_before_removal")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 54564588 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 54564588 (its removal ledger)", env.Data.AuthFlagsAsOfLedger)
	}
	// The recovered VALUES still have to be right — mask 0xA.
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("auth_revocable = %v, want true", env.Data.AuthRevocable)
	}
	if env.Data.AuthClawback == nil || !*env.Data.AuthClawback {
		t.Errorf("auth_clawback = %v, want true", env.Data.AuthClawback)
	}
}

// TestHandleIssuer_PersistedLastKnownIsReofferedToTheLiveReader — a
// `last_known_before_removal` row stops being true the moment the account is
// re-created at the same address, and the drain's primary queue
// (`auth_required IS NULL`) never revisits a filled row, so the read path
// must re-offer it to the live reader.
func TestHandleIssuer_PersistedLastKnownIsReofferedToTheLiveReader(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "sep1-sourced.example",
		AuthRequired:        boolPtr(true),
		AuthRevocable:       boolPtr(true),
		AuthImmutable:       boolPtr(true),
		AuthClawback:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLastKnownBeforeRemoval),
		AuthFlagsAsOfLedger: u32(54564588),
	}}
	// Re-created at the same address, every flag cleared.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0,
		LastModifiedLedger: 64228661,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+mergedIssuerG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q — a last-known row must be re-offered to the live reader, not frozen",
			env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64228661 (the re-created entry's ledger)", env.Data.AuthFlagsAsOfLedger)
	}
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
			t.Errorf("%s = %v, want false — the re-created account's own entry outranks the pre-removal reading", f.name, f.got)
		}
	}
}

// TestHandleIssuer_PersistedLiveReadingIsReReadThroughThePointLookup — a
// filled `live` row is re-read too (it can hold a domain the account has since
// moved away from), and it is re-read through the narrow key_xdr point lookup
// rather than AccountStateCached, whose fan-out to trustlines and offers would
// put a cold lake read on every issuer page.
func TestHandleIssuer_PersistedLiveReadingIsReReadThroughThePointLookup(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "centre.io",
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
	}}
	flags := &stubIssuerAuthFlags{live: map[string]clickhouse.AccountAuthFlags{
		mergedIssuerG: {Required: false, HomeDomain: "centre.io", Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}
	// A different answer than the point lookup's, so the response shows which
	// reader it came from.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		LastModifiedLedger: 64999999,
	}}
	got := getIssuer(t, v1.Options{Issuers: reader, Explorer: explorer, IssuerAuthFlags: flags}, mergedIssuerG)

	if len(flags.calls) != 1 || flags.calls[0] != mergedIssuerG {
		t.Fatalf("point lookup calls = %v, want exactly [%s]", flags.calls, mergedIssuerG)
	}
	if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want the point lookup's 64228661", got.AuthFlagsAsOfLedger)
	}
	if got.AuthRequired == nil || *got.AuthRequired {
		t.Errorf("auth_required = %v, want the point lookup's false", got.AuthRequired)
	}
}
