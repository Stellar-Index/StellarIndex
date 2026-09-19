//go:build rsecv1evidence

package v1_test

// The read-path leg of RSEC-V1 / RLT-470, shipped RED as evidence and as the
// acceptance test for the follow-up.
//
// `enrichIssuerFromAccountState` skips the lake read when a row already
// carries flags AND a home_domain. That is exactly the population the finding
// is about: a drained, live-sourced row holding a domain the anchor no longer
// declares. So the live-beats-stored precedence the writers now implement
// never fires for the rows it exists for, and the response serves the lapsed
// name until the drain's nightly chain re-check corrects the column.
//
// WHY IT IS NOT FIXED HERE. Removing the skip with the seam as it stands costs
// a measured +0.47s per cold issuer detail (api.stellarindex.io, 2026-09-19:
// /v1/issuers/{g} answers in 0.20s; the same account's state read is 0.67s
// cold, 0.20s warm behind the 30s TTL) on 44,247 of 49,002 resolved r1 rows —
// a 3.4x regression on a long-tail page most views arrive cold at. The cost is
// an artefact of the seam rather than of the read: AccountStateCached fans out
// to trustlines and offers, and this function wants none of them. The narrow
// reader the drain already uses — clickhouse.ExplorerReader.BulkAccountAuthFlags,
// a key_xdr point lookup measured at 0.028s — returns precisely the four
// flags, the home_domain and the as-of ledger.
//
// The follow-up therefore needs three files this unit's fence does not carry:
//
//   - internal/api/v1/server.go — put the narrow reader on the ExplorerReader
//     seam so the enrich can consult the chain for ~0.03s;
//   - internal/api/v1/issuers.go — drop the `HomeDomain != ""` arm of the
//     skip (in fence, but inert without the seam above);
//   - internal/api/v1/issuers_persisted_provenance_test.go — its
//     TestHandleIssuer_PersistedLiveReadingStillSkipsTheLakeRead pins the
//     skip's current expectation and has to be re-pinned to the new contract,
//     deliberately and in the same change rather than quietly.
//
// Run: go test -tags rsecv1evidence ./internal/api/v1/ -run TestRSECV1 -v

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRSECV1_DrainedRowServesTheChainsHomeDomain — the row is the exact shape
// the drain leaves behind (flags resolved, source `live`, a home_domain that
// was true when it was written), and the account has since declared a
// different one on-chain. The response must carry the chain's answer.
//
// The existing TestIssuerGet_LiveOnChainBeatsAStoredHomeDomain passes today
// only because its fixture leaves AuthRequired nil, which is not a shape the
// drain produces: the skip never fires for it.
func TestRSECV1_DrainedRowServesTheChainsHomeDomain(t *testing.T) {
	const anchor = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	required := true
	asOf := uint32(64100000)
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             anchor,
		HomeDomain:          "lapsed-former.example",
		AuthRequired:        &required,
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: &asOf,
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		HomeDomain:         "ultracapital.xyz",
		LastModifiedLedger: 64228661,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+anchor)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain != "ultracapital.xyz" {
		t.Errorf("home_domain = %q, want ultracapital.xyz — the account's own current entry outranks "+
			"a stored copy of an older reading of the same field, and a drained row is the only "+
			"shape that can hold a lapsed one", env.Data.HomeDomain)
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64228661 — a reading served from the live entry "+
			"must be stamped with the ledger it was taken at", env.Data.AuthFlagsAsOfLedger)
	}
}

// TestRSECV1_DrainedRowKeepsItsDomainWhenTheEntryDeclaresNone keeps the
// acceptance criterion from over-reaching (u-K033): consulting the live entry
// must not turn an entry that declares NO domain into a retraction. This one
// passes today and must still pass after the skip goes.
func TestRSECV1_DrainedRowKeepsItsDomainWhenTheEntryDeclaresNone(t *testing.T) {
	const anchor = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	required := true
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:         anchor,
		HomeDomain:      "still-declared.example",
		AuthRequired:    &required,
		AuthFlagsSource: string(clickhouse.AuthFlagsSourceLive),
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		LastModifiedLedger: 64228661,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+anchor)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain != "still-declared.example" {
		t.Errorf("home_domain = %q, want still-declared.example — an entry that declares no domain "+
			"is not a retraction of the one on record", env.Data.HomeDomain)
	}
}
