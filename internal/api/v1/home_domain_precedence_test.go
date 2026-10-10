package v1_test

// Home-domain identity precedence. Founding case: the
// ex-apay ETH issuer GBFXOHVAS… — its on-chain home_domain has said
// ultracapital.xyz since Ultra Stellar acquired apay.io's wrapped
// assets, but /v1/assets/ETH-GBFXOHVAS… rendered "apay.io" (and
// SEP-1-"verified" against it) because the hand-curated knownIssuers
// map outranked the account's own signed field. Precedence is now:
// storage row → live on-chain account state → curated map.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestIssuerGet_OnChainBeatsCuratedMap — same precedence on the
// issuer card: an empty DB row must fill from on-chain account
// state, not from a (possibly stale) curated entry.
func TestIssuerGet_OnChainBeatsCuratedMap(t *testing.T) {
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{GStrkey: testUSDCIssuer},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			HomeDomain: "live-onchain.example",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+testUSDCIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.HomeDomain != "live-onchain.example" {
		t.Errorf("HomeDomain = %q, want live-onchain.example (on-chain must beat the curated map)",
			env.Data.HomeDomain)
	}
	// The curated map still supplies what chain can't: the org name.
	if env.Data.OrgName != "Circle" {
		t.Errorf("OrgName = %q, want Circle (curated fills fields chain doesn't carry)",
			env.Data.OrgName)
	}
}

// TestIssuerGet_ScamSuppressionSurvivesAccountState — for a
// flagged, unverified issuer, the self-declared identity IS the
// impersonation. The suppression must not run
// ahead of the account-state enrich, which would refill the cleared
// home_domain straight from the scammer's own on-chain field.
func TestIssuerGet_ScamSuppressionSurvivesAccountState(t *testing.T) {
	const scam = "GA2XZLXNLAL26VBCA2OESAIMXTRH5GXKLHYZMDGNCR2SYS5QZWWNBLCK"
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{GStrkey: scam},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			Flags:      0x2, // auth_revocable — objective state, must survive
			HomeDomain: "lobstr.co",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+scam)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.ScamReason == "" {
		t.Fatalf("ScamReason empty — test issuer %s no longer in the scam list; pick another", scam)
	}
	if env.Data.HomeDomain != "" {
		t.Errorf("HomeDomain = %q, want \"\" (suppressed identity must not be refilled from on-chain state)",
			env.Data.HomeDomain)
	}
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("AuthRevocable = %v, want true (auth flags are objective state, not identity)",
			env.Data.AuthRevocable)
	}
}

// TestIssuerGet_LiveOnChainBeatsAStoredHomeDomain.
//
// The stored column is a COPY of the AccountEntry field, and between its
// two writers it was write-once, so a value in it is at best an older
// reading of the entry the handler is holding right now. The founding case
// is an anchor that moved domain with SetOptions and let the old name
// lapse: the row keeps the lapsed name, the hourly SEP-1 refresh keeps
// fetching it, and whoever registers it next can publish a stellar.toml
// listing the anchor's issuer back and inherit its verified identity. The
// anchor's own on-chain change has to be able to correct that.
//
// The auth flags in the SAME function are already replaced by the same
// entry; this pins home_domain to the same rule.
func TestIssuerGet_LiveOnChainBeatsAStoredHomeDomain(t *testing.T) {
	const anchor = "GBFXOHVAS7DXHZPMPZL4HDPPMGSSJBWDGEOXSYHMPTSJKDFHPPFXFZ2K"
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{
			GStrkey: anchor,
			// What the write-once column froze: the domain the anchor
			// declared before it lapsed.
			HomeDomain: "lapsed-former.example",
		},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			HomeDomain: "current-anchor.example",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+anchor)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.HomeDomain != "current-anchor.example" {
		t.Errorf("HomeDomain = %q, want current-anchor.example — the account's own live entry "+
			"outranks a stored copy of an older reading of the same field",
			env.Data.HomeDomain)
	}
}

// TestIssuerGet_StoredHomeDomainSurvivesASilentLiveEntry keeps the rule
// above from over-reaching. An AccountEntry that resolves WITHOUT a domain
// is not a retraction: a merged account's reading is persisted without one
// on purpose (a dead account's self-declared identity is not persistable),
// and the lake can simply not carry the field. Only a non-empty live value
// replaces the stored one.
func TestIssuerGet_StoredHomeDomainSurvivesASilentLiveEntry(t *testing.T) {
	const anchor = "GBFXOHVAS7DXHZPMPZL4HDPPMGSSJBWDGEOXSYHMPTSJKDFHPPFXFZ2K"
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{GStrkey: anchor, HomeDomain: "stored.example"},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: true, HomeDomain: ""},
	}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+anchor)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.HomeDomain != "stored.example" {
		t.Errorf("HomeDomain = %q, want stored.example — an entry that declares no domain "+
			"is not a retraction of the one we hold", env.Data.HomeDomain)
	}
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
