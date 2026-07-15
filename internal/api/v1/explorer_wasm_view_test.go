package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const wasmTestCID = "CAP6ZT7JC3ZCNELT4I7OJ6IBACRGRN2CWS5GBPCPYRLF3RLOTX33FAF6"

func TestExplorer_ContractWasm_OK(t *testing.T) {
	reader := &stubExplorerReader{wasm: clickhouse.ContractWasmInfo{
		ContractID: wasmTestCID,
		WasmHash:   "f89eb3cca7365ba80ab88ff1a03fcd06b9ebf58d85f727ab60c9d7e79db59b07",
		SizeBytes:  4641,
		Exports: []clickhouse.WasmExport{
			{Name: "register", Params: []string{"i64", "i64"}, Results: []string{"i64"}},
			{Name: "revoke", Params: nil, Results: []string{"i64"}},
		},
		Wat: "(module\n)",
	}}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/contracts/"+wasmTestCID+"/wasm")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want day-long immutable cache", cc)
	}
	var body struct {
		Data v1.ContractWasmView `json:"data"`
	}
	mustDecode(t, resp, &body)
	d := body.Data
	if d.ContractID != wasmTestCID || d.SizeBytes != 4641 {
		t.Fatalf("wasm view = %+v", d)
	}
	if len(d.Exports) != 2 || d.Exports[0].Name != "register" {
		t.Fatalf("exports = %+v", d.Exports)
	}
	// A no-param export must serialise as [] not null (nonNilStrings).
	if d.Exports[1].Params == nil {
		t.Errorf("nil params leaked to wire for %q", d.Exports[1].Name)
	}
	if d.Wat == "" || d.SourceNote == "" {
		t.Errorf("missing wat/source_note: %+v", d)
	}
}

func TestExplorer_ContractWasm_NotFound(t *testing.T) {
	// The lake couldn't assemble the wasm (instance/code entry outside the
	// captured window) -> clean 404, not 500.
	reader := &stubExplorerReader{wasmErr: clickhouse.ErrContractWasmUnresolved}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/contracts/"+wasmTestCID+"/wasm")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestExplorer_ContractWasm_InvalidID(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/contracts/notacontract/wasm"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExplorer_ContractWasm_Unavailable503(t *testing.T) {
	base := explorerTestServer(t, nil)
	if resp := mustGet(t, base+"/v1/contracts/"+wasmTestCID+"/wasm"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}
