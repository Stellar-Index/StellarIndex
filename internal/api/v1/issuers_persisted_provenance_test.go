package v1_test

// The PERSISTED half of auth-flag provenance on /v1/issuers/{g_strkey} (#374).
//
// issuers_provenance_test.go pins what the read path CONCLUDES from a live
// AccountEntry. These pin what it does with what the drain already wrote —
// the half that was latent until handleIssuer carried IssuerRow's provenance
// into the response. Until it did, `AuthFlagsSource` was always "" at the
// point enrichIssuerFromAccountState reads it, so the skip-gate that exists
// to keep a historical reading from freezing in place could never fire.

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
// account which no longer exists, which is a quieter defect than the
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

// TestHandleIssuer_PersistedLastKnownIsReofferedToTheLiveReader — the
// skip-gate. handleIssuer skips the lake read when a row already has flags
// AND a home_domain; a `last_known_before_removal` row must be EXEMPT from
// that skip, because it is the one reading nothing else can ever correct:
// the drain's primary queue is `auth_required IS NULL`, so it never revisits
// a row it has filled.
//
// The row here is shaped to trip the skip-gate — flags set, home_domain
// non-empty (from the SEP-1 resolver, which is a separate and better-sourced
// path than the AccountEntry). Only the provenance label distinguishes it,
// so this fails outright unless handleIssuer carries that label in.
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

// TestHandleIssuer_PersistedLiveReadingStillSkipsTheLakeRead — the exemption
// above must stay narrow. `live` rows with a home_domain keep their cheap
// early return; on r1 that is 44,247 of the 49,002 resolved rows, including
// USDC's, and each avoided AccountStateCached call fans out to entry +
// trustlines + offers reads on the hottest issuer pages in the set.
func TestHandleIssuer_PersistedLiveReadingStillSkipsTheLakeRead(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "centre.io",
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
	}}
	// If the handler consults this, it would overwrite the persisted values.
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

	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64100000 {
		t.Errorf("auth_flags_as_of_ledger = %v, want the persisted 64100000 — a live row must keep its early return",
			env.Data.AuthFlagsAsOfLedger)
	}
	if env.Data.AuthRequired == nil || !*env.Data.AuthRequired {
		t.Errorf("auth_required = %v, want the persisted true", env.Data.AuthRequired)
	}
}
