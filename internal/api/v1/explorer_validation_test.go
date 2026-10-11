package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// The explorer lake reads share a ClickHouse pool; a malformed id that reaches ClickHouse still
// drives a FINAL scan (AssetHolders runs two) before any 400. These tests pin the up-front
// validation: a malformed asset_id / contract_id gets 400 BEFORE the reader is consulted. The
// stub reader succeeds for every method, so a 400 proves the guard fires first.

func TestExplorer_AssetHolders_RejectsMalformedAsset(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{
		// A non-nil holder set so an unvalidated handler would 200.
		holderCount: 1,
	})

	// "USDC" alone (no issuer) is not a canonical asset_id.
	resp := mustGet(t, base+"/v1/assets/USDC/holders")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-asset-id" {
		t.Errorf("Type = %q, want invalid-asset-id", p.Type)
	}
}

func TestExplorer_ContractInteractions_RejectsMalformedContract(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})

	resp := mustGet(t, base+"/v1/contracts/notacontract/interactions")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-contract-id" {
		t.Errorf("Type = %q, want invalid-contract-id", p.Type)
	}
}

func TestExplorer_ContractCodeHistory_RejectsMalformedContract(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})

	resp := mustGet(t, base+"/v1/contracts/notacontract/code-history")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/invalid-contract-id" {
		t.Errorf("Type = %q, want invalid-contract-id", p.Type)
	}
}
