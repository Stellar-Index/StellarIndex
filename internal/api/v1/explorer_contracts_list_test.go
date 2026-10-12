package v1_test

import (
	"errors"
	"net/http"
	"strings"
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

// livenessTip is the lake watermark the liveness tests judge TTLs against.
const livenessTip = 64_277_149

// contractLivenessBody decodes only the liveness fields, so these tests pin
// the wire keys rather than the Go field names.
type contractLivenessBody struct {
	Data struct {
		Exists *bool `json:"exists"`
		TTL    *struct {
			LiveUntil  uint32 `json:"live_until"`
			State      string `json:"state"`
			AsOfLedger uint32 `json:"as_of_ledger"`
		} `json:"ttl"`
		SourceNote string `json:"source_note"`
	} `json:"data"`
}

func livenessServer(t *testing.T, r *stubExplorerReader) string {
	t.Helper()
	srv := v1.New(v1.Options{
		Explorer:      r,
		LakeWatermark: &wmStub{ledger: livenessTip, closedAt: time.Now().Add(-time.Minute)},
	})
	return httpTestServer(t, srv).URL
}

func getContractLiveness(t *testing.T, url string) (contractLivenessBody, *http.Response) {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	var body contractLivenessBody
	mustDecode(t, resp, &body)
	return body, resp
}

// A never-deployed strkey (no events, no activity, no instance evidence)
// must not be byte-identical to a real quiet contract.
func TestExplorer_ContractDetail_NeverDeployedIsExistsFalse(t *testing.T) {
	base := livenessServer(t, &stubExplorerReader{})
	body, _ := getContractLiveness(t, base+"/v1/contracts/"+wasmTestCID)
	if body.Data.Exists == nil || *body.Data.Exists {
		t.Fatalf("exists = %v, want explicit false for a contract with no lake evidence", body.Data.Exists)
	}
	if body.Data.TTL != nil {
		t.Errorf("ttl = %+v, want absent with no TTL row", body.Data.TTL)
	}
}

// A quiet contract whose instance entry the lake holds is not "never deployed".
func TestExplorer_ContractDetail_QuietKnownInstanceExists(t *testing.T) {
	base := livenessServer(t, &stubExplorerReader{instance: clickhouse.ContractInstanceState{Known: true}})
	body, _ := getContractLiveness(t, base+"/v1/contracts/"+wasmTestCID)
	if body.Data.Exists == nil || !*body.Data.Exists {
		t.Fatalf("exists = %v, want true for a captured instance", body.Data.Exists)
	}
}

// An instance read failure must not be reported as absence.
func TestExplorer_ContractDetail_InstanceReadFailureOmitsExists(t *testing.T) {
	base := livenessServer(t, &stubExplorerReader{instanceErr: errors.New("ch down")})
	body, _ := getContractLiveness(t, base+"/v1/contracts/"+wasmTestCID)
	if body.Data.Exists != nil {
		t.Fatalf("exists = %v, want absent when the instance read failed", *body.Data.Exists)
	}
}

// A body missing its liveness verdict must not be pinned by a shared cache.
func TestExplorer_ContractDetail_InstanceReadFailureIsUncacheable(t *testing.T) {
	healthy := livenessServer(t, &stubExplorerReader{})
	if _, resp := getContractLiveness(t, healthy+"/v1/contracts/"+wasmTestCID); !strings.HasPrefix(resp.Header.Get("Cache-Control"), "public") {
		t.Fatalf("healthy Cache-Control = %q, want the route's public band", resp.Header.Get("Cache-Control"))
	}
	// Exists is settled by the event, but TTL is not: the read still failed.
	failed := livenessServer(t, &stubExplorerReader{
		instanceErr: errors.New("ch down"),
		contractEvents: []clickhouse.ContractActivityRow{
			{Seq: 63_000_000, CloseTime: time.Unix(0, 0), TxHash: "ab", EventType: "contract", Topic0Sym: "transfer"},
		},
	})
	_, resp := getContractLiveness(t, failed+"/v1/contracts/"+wasmTestCID)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("degraded Cache-Control = %q, want no-store", cc)
	}
}

func TestExplorer_ContractDetail_TTLState(t *testing.T) {
	cases := []struct {
		name      string
		liveUntil uint32
		want      string
	}{
		{"archived since 2024-11", 54_400_000, "archived"},
		{"lapses one ledger before tip", livenessTip - 1, "archived"},
		{"live through the tip ledger", livenessTip, "live"},
		{"live well past tip", 70_000_000, "live"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubExplorerReader{
				instance: clickhouse.ContractInstanceState{Known: true, LiveUntil: tc.liveUntil},
				contractEvents: []clickhouse.ContractActivityRow{
					{Seq: 63_000_000, CloseTime: time.Unix(0, 0), TxHash: "ab", EventType: "contract", Topic0Sym: "transfer"},
				},
			}
			body, _ := getContractLiveness(t, livenessServer(t, reader)+"/v1/contracts/"+wasmTestCID)
			ttl := body.Data.TTL
			if ttl == nil {
				t.Fatal("ttl absent, want the instance's TTL verdict")
			}
			if ttl.State != tc.want || ttl.LiveUntil != tc.liveUntil || ttl.AsOfLedger != livenessTip {
				t.Fatalf("ttl = %+v, want {live_until:%d state:%s as_of_ledger:%d}", *ttl, tc.liveUntil, tc.want, livenessTip)
			}
			if body.Data.Exists == nil || !*body.Data.Exists {
				t.Errorf("exists = %v, want true", body.Data.Exists)
			}
		})
	}
}

// The wasm route must not describe an archived contract as plainly resolved
// current state, nor let a CDN pin that verdict for a day.
func TestExplorer_ContractWasm_ArchivedInstance(t *testing.T) {
	reader := &stubExplorerReader{
		wasm:     clickhouse.ContractWasmInfo{ContractID: wasmTestCID, WasmHash: "f89e", SizeBytes: 10},
		instance: clickhouse.ContractInstanceState{Known: true, LiveUntil: 54_400_000},
	}
	body, resp := getContractLiveness(t, livenessServer(t, reader)+"/v1/contracts/"+wasmTestCID+"/wasm")
	if body.Data.TTL == nil || body.Data.TTL.State != "archived" || body.Data.TTL.LiveUntil != 54_400_000 {
		t.Fatalf("ttl = %+v, want archived at 54400000", body.Data.TTL)
	}
	if !strings.HasPrefix(body.Data.SourceNote, "ARCHIVED:") || !strings.Contains(body.Data.SourceNote, "54400000") {
		t.Errorf("source_note = %q, want it to lead with the archival", body.Data.SourceNote)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want the short ttl-verdict cache", cc)
	}
}

func TestExplorer_ContractWasm_LiveInstanceNoteUnqualified(t *testing.T) {
	reader := &stubExplorerReader{
		wasm:     clickhouse.ContractWasmInfo{ContractID: wasmTestCID, WasmHash: "f89e", SizeBytes: 10},
		instance: clickhouse.ContractInstanceState{Known: true, LiveUntil: 70_000_000},
	}
	body, _ := getContractLiveness(t, livenessServer(t, reader)+"/v1/contracts/"+wasmTestCID+"/wasm")
	if body.Data.TTL == nil || body.Data.TTL.State != "live" {
		t.Fatalf("ttl = %+v, want live", body.Data.TTL)
	}
	if !strings.HasPrefix(body.Data.SourceNote, "wasm resolved from the certified ClickHouse lake") {
		t.Errorf("source_note = %q, want the unqualified provenance line for a live instance", body.Data.SourceNote)
	}
}

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
	// This URL is keyed on contract_id, not the wasm hash it
	// returns, so a day-long "immutable" cache served pre-upgrade
	// bytecode for up to 24h after an in-place upgrade. Bounded to the
	// standard catalogue band instead.
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=60, s-maxage=300" {
		t.Errorf("Cache-Control = %q, want the bounded catalogue band", cc)
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
