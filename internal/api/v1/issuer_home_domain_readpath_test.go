package v1_test

// The read path of /v1/issuers/{g_strkey} consults the issuer's live
// AccountEntry for every row, filled or not: a drained, live-sourced row is
// exactly the shape that can hold a home_domain the account no longer
// declares, and the SEP-1 identity stored beside it was fetched from that
// domain.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubIssuerAuthFlags is the narrow point-lookup seam; calls records every
// key it was asked for.
type stubIssuerAuthFlags struct {
	live  map[string]clickhouse.AccountAuthFlags
	calls []string
}

func (s *stubIssuerAuthFlags) BulkAccountAuthFlags(_ context.Context, keys []string) (map[string]clickhouse.AccountAuthFlags, error) {
	s.calls = append(s.calls, keys...)
	out := make(map[string]clickhouse.AccountAuthFlags, len(keys))
	for _, k := range keys {
		if f, ok := s.live[k]; ok {
			out[k] = f
		}
	}
	return out, nil
}

func getIssuer(t *testing.T, opts v1.Options, g string) v1.Issuer {
	t.Helper()
	ts := startHTTPTest(t, v1.New(opts).Handler())
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+g)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)
	return env.Data
}

// TestIssuerGet_DrainedRowServesTheChainsHomeDomain — the row is the exact
// shape the drain leaves behind (flags resolved, source `live`, a home_domain
// that was true when it was written), and the account has since declared a
// different one on-chain. The response must carry the chain's answer.
func TestIssuerGet_DrainedRowServesTheChainsHomeDomain(t *testing.T) {
	const anchor = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             anchor,
		HomeDomain:          "lapsed-former.example",
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		HomeDomain:         "ultracapital.xyz",
		LastModifiedLedger: 64228661,
	}}
	got := getIssuer(t, v1.Options{Issuers: reader, Explorer: explorer}, anchor)

	if got.HomeDomain != "ultracapital.xyz" {
		t.Errorf("home_domain = %q, want ultracapital.xyz — the account's own current entry outranks "+
			"a stored copy of an older reading of the same field", got.HomeDomain)
	}
	if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64228661 — a reading served from the live entry "+
			"must be stamped with the ledger it was taken at", got.AuthFlagsAsOfLedger)
	}
}

// TestIssuerGet_DrainedRowKeepsItsDomainWhenTheEntryDeclaresNone keeps the
// live read from over-reaching: an entry that declares NO domain is not a
// retraction of the one on record.
func TestIssuerGet_DrainedRowKeepsItsDomainWhenTheEntryDeclaresNone(t *testing.T) {
	const anchor = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:         anchor,
		HomeDomain:      "still-declared.example",
		AuthRequired:    boolPtr(true),
		AuthFlagsSource: string(clickhouse.AuthFlagsSourceLive),
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		LastModifiedLedger: 64228661,
	}}
	got := getIssuer(t, v1.Options{Issuers: reader, Explorer: explorer}, anchor)

	if got.HomeDomain != "still-declared.example" {
		t.Errorf("home_domain = %q, want still-declared.example — an entry that declares no domain "+
			"is not a retraction of the one on record", got.HomeDomain)
	}
}

// drainedVerifiedRow is a filled, live-sourced row whose SEP-1 identity was
// fetched from, and verified against, lapsed-former.example.
func drainedVerifiedRow() timescale.IssuerRow {
	resolved := "2026-09-01T00:00:00Z"
	return timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "lapsed-former.example",
		OrgName:             "Former Domain Org",
		OrgVerified:         true,
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
		SEP1ResolvedAt:      &resolved,
		SEP1Payload:         json.RawMessage(`{"OrgName":"Former Domain Org","OrgVerified":true}`),
	}
}

// TestIssuerGet_LiveDomainUnbindsTheStoredSEP1Identity — when the chain
// declares a different home_domain than the one the stored SEP-1 payload was
// fetched from, the response must not pair the new domain with the old
// domain's verified org identity. Read through the narrow point lookup.
func TestIssuerGet_LiveDomainUnbindsTheStoredSEP1Identity(t *testing.T) {
	flags := &stubIssuerAuthFlags{live: map[string]clickhouse.AccountAuthFlags{
		mergedIssuerG: {Required: true, HomeDomain: "current.example", Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}
	got := getIssuer(t, v1.Options{
		Issuers:         &stubIssuersReader{row: drainedVerifiedRow()},
		IssuerAuthFlags: flags,
	}, mergedIssuerG)

	if got.HomeDomain != "current.example" {
		t.Errorf("home_domain = %q, want current.example", got.HomeDomain)
	}
	if got.OrgVerified || got.OrgName != "" || got.SEP1Payload != nil || got.SEP1ResolvedAt != nil {
		t.Errorf("org_verified=%v org_name=%q sep1_payload=%s sep1_resolved_at=%v — the stored SEP-1 identity "+
			"belongs to the domain the account no longer declares", got.OrgVerified, got.OrgName, got.SEP1Payload, got.SEP1ResolvedAt)
	}
	if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64228661", got.AuthFlagsAsOfLedger)
	}
}

// TestIssuerGet_LiveDomainAgreeingKeepsTheSEP1Identity — the unbinding is
// scoped to a domain change: a live entry that still declares the stored
// domain leaves the verified identity in place.
func TestIssuerGet_LiveDomainAgreeingKeepsTheSEP1Identity(t *testing.T) {
	flags := &stubIssuerAuthFlags{live: map[string]clickhouse.AccountAuthFlags{
		mergedIssuerG: {Required: true, HomeDomain: "lapsed-former.example", Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}
	got := getIssuer(t, v1.Options{
		Issuers:         &stubIssuersReader{row: drainedVerifiedRow()},
		IssuerAuthFlags: flags,
	}, mergedIssuerG)

	if got.HomeDomain != "lapsed-former.example" || !got.OrgVerified || got.OrgName != "Former Domain Org" || got.SEP1Payload == nil {
		t.Errorf("home_domain=%q org_verified=%v org_name=%q sep1_payload=%s — an agreeing live entry must not unbind the identity",
			got.HomeDomain, got.OrgVerified, got.OrgName, got.SEP1Payload)
	}
}
