package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rosterProjectionCap mirrors the LIMIT 5000 every enumerating roster query
// carries (timescale.ListSourceContractsFromProjection). A protocol with more
// contracts than this can never have its count derived from an enumeration.
const rosterProjectionCap = 5000

// countingContractsReader is a contracts reader that ALSO answers the
// count-without-enumeration capability, the way timescale.Store does:
// `projection` is what enumerating a source returns (already truncated at the
// cap, as the live query would be) and `counts` is the source's true total.
type countingContractsReader struct {
	projection map[string][]string
	counts     map[string]int64
	countErr   error
}

func (r *countingContractsReader) ListProtocolContracts(context.Context, string) ([]timescale.ProtocolContract, error) {
	return nil, nil
}

func (r *countingContractsReader) ListSourceContractsFromProjection(_ context.Context, source string) ([]string, error) {
	return r.projection[source], nil
}

func (r *countingContractsReader) ProtocolContractIndex(context.Context) (map[string]string, error) {
	return nil, nil
}

func (r *countingContractsReader) CountSourceContracts(_ context.Context, source string) (int64, bool, error) {
	if r.countErr != nil {
		return 0, false, r.countErr
	}
	n, ok := r.counts[source]
	return n, ok, nil
}

// directoryContractCount returns one protocol's contract_count from
// GET /v1/protocols, plus the directory's coverage_note. found=false when the
// source was omitted from the directory.
func directoryContractCount(t *testing.T, base, name string) (count int, note string, found bool) {
	t.Helper()
	resp := mustGet(t, base+"/v1/protocols")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/protocols status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.ProtocolsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	for _, row := range env.Data.Protocols {
		if row.Name == name {
			return row.ContractCount, env.Data.CoverageNote, true
		}
	}
	return 0, env.Data.CoverageNote, false
}

// protocolDetail fetches GET /v1/protocols/{name}.
func protocolDetail(t *testing.T, base, name string) v1.ProtocolDetailView {
	t.Helper()
	resp := mustGet(t, base+"/v1/protocols/"+name)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/protocols/%s status = %d, want 200", name, resp.StatusCode)
	}
	var env struct {
		Data v1.ProtocolDetailView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode %s detail: %v", name, err)
	}
	return env.Data
}

// TestProtocolContractCount_TrueTotalNotZeroNotCap covers the protocol whose
// contracts outnumber the enumerating roster's LIMIT 5000: sorocredit deploys
// one Collateral-<uuid> child contract per opened position (116,124 on r1,
// 2026-09-03). Neither enumeration outcome is a usable count — an absent
// projection path publishes 0, a capped one publishes 5,000 — so both the
// directory row and the detail view must publish the counted TRUE total.
func TestProtocolContractCount_TrueTotalNotZeroNotCap(t *testing.T) {
	const trueCount = 116_124

	// Truncated enumeration: what a projection roster for sorocredit COULD
	// return, capped exactly as the served-tier query caps it.
	capped := make([]string, 0, rosterProjectionCap)
	for i := 0; i < rosterProjectionCap; i++ {
		capped = append(capped, fmt.Sprintf("CCHILD%05d", i))
	}

	cases := []struct {
		name       string
		projection map[string][]string
	}{
		// Production shape: sorocredit has no enumerating roster path at all,
		// so the roster is empty and the count is the only honest number.
		{name: "no enumeration path", projection: nil},
		// And when an enumeration exists it must not win: its cap is a wrong
		// number, not a smaller-but-true one.
		{name: "enumeration truncated at the cap", projection: map[string][]string{"sorocredit": capped}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countingContractsReader{
				projection: tc.projection,
				counts:     map[string]int64{"sorocredit": trueCount},
			}
			ts := httpTestServer(t, v1.New(v1.Options{ProtocolContracts: reader}))

			count, note, found := directoryContractCount(t, ts.URL, "sorocredit")
			if !found {
				t.Fatalf("sorocredit omitted from the directory (coverage_note=%q), want a row", note)
			}
			if count != trueCount {
				t.Errorf("directory contract_count = %d, want %d (0 = never counted, %d = the enumeration cap)",
					count, trueCount, rosterProjectionCap)
			}

			d := protocolDetail(t, ts.URL, "sorocredit")
			if d.ContractCount != trueCount {
				t.Errorf("detail contract_count = %d, want %d", d.ContractCount, trueCount)
			}
			if len(d.Contracts) > rosterProjectionCap {
				t.Errorf("detail contracts = %d entries, want at most the %d cap", len(d.Contracts), rosterProjectionCap)
			}
		})
	}
}

// TestProtocolContractCount_UncountedSourceKeepsRoster pins the untouched
// half: a source the counter has no count for (ok=false) still reports the
// length of its enumerated roster.
func TestProtocolContractCount_UncountedSourceKeepsRoster(t *testing.T) {
	reader := &countingContractsReader{
		projection: map[string][]string{"defindex": {"CVAULT1", "CVAULT2", "CVAULT3"}},
		counts:     map[string]int64{"sorocredit": 116_124},
	}
	ts := httpTestServer(t, v1.New(v1.Options{ProtocolContracts: reader}))

	count, note, found := directoryContractCount(t, ts.URL, "defindex")
	if !found {
		t.Fatalf("defindex omitted from the directory (coverage_note=%q), want a row", note)
	}
	if count != 3 {
		t.Errorf("defindex contract_count = %d, want 3 (its roster length)", count)
	}
	if d := protocolDetail(t, ts.URL, "defindex"); d.ContractCount != 3 || len(d.Contracts) != 3 {
		t.Errorf("defindex detail count=%d contracts=%d, want 3/3", d.ContractCount, len(d.Contracts))
	}
}

// TestProtocolContractCount_FailedCountOmitsNotZeros keeps the counted path
// under the same honesty contract as a failed roster read: an unreadable count
// omits the source and names it in coverage_note, never a fabricated
// contract_count: 0.
func TestProtocolContractCount_FailedCountOmitsNotZeros(t *testing.T) {
	reader := &countingContractsReader{countErr: errors.New("served-tier count failed")}
	ts := httpTestServer(t, v1.New(v1.Options{ProtocolContracts: reader}))

	count, note, found := directoryContractCount(t, ts.URL, "sorocredit")
	if found {
		t.Fatalf("sorocredit published contract_count = %d after a failed count, want the source omitted", count)
	}
	if !strings.Contains(note, "sorocredit") {
		t.Errorf("coverage_note = %q, want it to name the omitted sorocredit", note)
	}
}
