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
