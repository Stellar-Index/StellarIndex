package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestExplorer_ContractsDirectory(t *testing.T) {
	now := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	reader := &stubExplorerReader{
		ledgers: []clickhouse.LedgerHeader{{Seq: 1_000_000, CloseTime: now}},
		directory: []clickhouse.ContractDirectoryRow{
			{ContractID: "CBLEND", Events: 9001, LastLedger: 999_999, LastSeen: now},
			{ContractID: "CUNKNOWN", Events: 12, LastLedger: 999_000, LastSeen: now},
		},
	}
	// Attribution wired so CBLEND tags as blend, CUNKNOWN stays untagged.
	srv := v1.New(v1.Options{
		Explorer:          reader,
		ProtocolContracts: &stubProtocolContractsReader{contractIndex: map[string]string{"CBLEND": "blend"}},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/contracts?days=30&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.ContractsDirectoryView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Contracts) != 2 {
		t.Fatalf("want 2 contracts, got %d", len(body.Data.Contracts))
	}
	if body.Data.Contracts[0].ContractID != "CBLEND" || body.Data.Contracts[0].Protocol != "blend" {
		t.Errorf("first row = %+v, want CBLEND attributed to blend", body.Data.Contracts[0])
	}
	if body.Data.Contracts[1].Protocol != "" {
		t.Errorf("CUNKNOWN should be unattributed, got %q", body.Data.Contracts[1].Protocol)
	}
	// Window floor: tip 1_000_000 − 30·17280 = 481_600.
	if body.Data.SinceLedger != 1_000_000-30*17_280 {
		t.Errorf("since_ledger = %d, want %d", body.Data.SinceLedger, 1_000_000-30*17_280)
	}
}

func TestExplorer_ContractsDirectory_RejectsBadDays(t *testing.T) {
	reader := &stubExplorerReader{ledgers: []clickhouse.LedgerHeader{{Seq: 100}}}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/contracts?days=0")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("days=0 status = %d, want 400", resp.StatusCode)
	}
}

func TestExplorer_ContractInteractions(t *testing.T) {
	now := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	reader := &stubExplorerReader{
		ledgers: []clickhouse.LedgerHeader{{Seq: 2_000_000, CloseTime: now}},
		interactions: []clickhouse.ContractEdgeRow{
			{ContractID: "CORACLE", SharedTxs: 42},
			{ContractID: "CTOKEN", SharedTxs: 17},
		},
	}
	srv := v1.New(v1.Options{
		Explorer:          reader,
		ProtocolContracts: &stubProtocolContractsReader{contractIndex: map[string]string{"CORACLE": "reflector-dex"}},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/contracts/"+wasmTestCID+"/interactions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.ContractInteractionsView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.ContractID != wasmTestCID {
		t.Errorf("contract_id = %q, want %q", body.Data.ContractID, wasmTestCID)
	}
	if len(body.Data.Interactions) != 2 {
		t.Fatalf("want 2 edges, got %d", len(body.Data.Interactions))
	}
	if body.Data.Interactions[0].ContractID != "CORACLE" || body.Data.Interactions[0].Protocol != "reflector-dex" {
		t.Errorf("first edge = %+v, want CORACLE attributed to reflector-dex", body.Data.Interactions[0])
	}
	if body.Data.Interactions[0].SharedTxs != 42 {
		t.Errorf("shared_txs = %d, want 42", body.Data.Interactions[0].SharedTxs)
	}
}

func TestExplorer_ContractCodeHistory(t *testing.T) {
	now := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	reader := &stubExplorerReader{codeHistory: []clickhouse.ContractCodeVersion{
		{Ledger: 51000000, CloseTime: now, WasmHash: "aaaa"},
		{Ledger: 52000000, CloseTime: now, WasmHash: "bbbb"},
	}}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/contracts/"+wasmTestCID+"/code-history")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.ContractCodeHistoryView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.ContractID != wasmTestCID || len(body.Data.Versions) != 2 {
		t.Fatalf("got %+v", body.Data)
	}
	if body.Data.Versions[0].WasmHash != "aaaa" || body.Data.Versions[1].Ledger != 52000000 {
		t.Errorf("versions = %+v", body.Data.Versions)
	}
}
