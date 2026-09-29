package v1

import (
	"testing"
)

// A set mixing classic and contract-issued rows: by_issuer holds only the
// classic G-addresses — a contract row's empty Issuer is not a grouping key.
func TestRWAIdentity_ByIssuerSkipsContractRows(t *testing.T) {
	const (
		etherfuse = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		ondo      = "GAKE3L4PLQPE6BMUTPGXXDDEYRNQSMWQTMVZDSXTPXDOOMFDTVUOGCEX"
	)
	mcap := func(v string) RWAValuation { return RWAValuation{MarketCapUSD: &v} }
	assets := []RWAAsset{
		{AssetID: "USTRY-" + etherfuse, Code: "USTRY", Issuer: etherfuse, IssuerDirectoryName: "Etherfuse", HomeDomain: "etherfuse.com", Valuation: mcap("10.00")},
		{AssetID: "USDY-" + ondo, Code: "USDY", Issuer: ondo, IssuerDirectoryName: "Ondo", HomeDomain: "ondo.finance", Valuation: mcap("20.00")},
		{AssetID: "CA1", ContractID: "CA1", IssuerDirectoryName: "Entity One", IssuerDirectoryDomain: "one.example", Valuation: mcap("30.00")},
		{AssetID: "CA2", ContractID: "CA2", IssuerDirectoryName: "Entity Two", IssuerDirectoryDomain: "two.example", Valuation: mcap("40.00")},
	}

	got := rwaByIssuer(assets)
	if len(got) != 2 {
		t.Fatalf("by_issuer = %+v, want exactly the 2 classic issuers", got)
	}
	want := map[string]string{etherfuse: "10.00", ondo: "20.00"}
	for _, row := range got {
		if row.Issuer == "" {
			t.Errorf("by_issuer carries a blank issuer %+v merging contract rows of distinct entities", row)
			continue
		}
		if row.MarketCapUSD == nil || *row.MarketCapUSD != want[row.Issuer] || row.Assets != 1 {
			t.Errorf("by_issuer[%s] = %+v, want 1 asset at %s", row.Issuer, row, want[row.Issuer])
		}
	}
	if s := rwaSummarise(assets, rwaTruncation{}); s.Issuers != 4 || s.Assets != 4 {
		t.Errorf("summary assets/issuers = %d/%d, want 4/4 — each contract is its own issuer, never a shared blank one", s.Assets, s.Issuers)
	}
}

// A contract row's domain comes from the curated directory, not from an
// on-chain home_domain an attestation was fetched from, and it is served
// under the field that says so.
func TestRWAIdentity_ContractDirectoryDomainIsNotHomeDomain(t *testing.T) {
	const contract = "CBNG2BVXUXUM7OUJNU6SGW7KZPG7OBNRSSFA2DSVDNCOYBJI7SE5VBSO"
	rows := rwaContractAssetRows(
		[]rwaContractMember{{contractID: contract, basis: "sep1_anchor_declaration", dirName: "Entity", dirDomain: "entity.example"}},
		map[string]AssetDetail{contract: {AssetID: contract}},
	)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0].HomeDomain != "" {
		t.Errorf("home_domain = %q, want empty — no attestation was fetched for a contract", rows[0].HomeDomain)
	}
	if rows[0].IssuerDirectoryDomain != "entity.example" {
		t.Errorf("issuer_directory_domain = %q, want the directory's entity.example", rows[0].IssuerDirectoryDomain)
	}
}
