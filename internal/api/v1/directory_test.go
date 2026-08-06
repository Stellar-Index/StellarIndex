package v1_test

// GET /v1/directory + the account/contract `directory` enrichment —
// curated third-party address labels (stellar-expert/public-directory
// via account_directory, migration 0136).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubDirectoryReader struct {
	entries map[string]timescale.DirectoryEntry
	err     error
}

func (s *stubDirectoryReader) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	if s.err != nil {
		return timescale.DirectoryEntry{}, false, s.err
	}
	e, ok := s.entries[address]
	return e, ok, nil
}

func (s *stubDirectoryReader) DirectoryEntriesByAddresses(_ context.Context, addresses []string) (map[string]timescale.DirectoryEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]timescale.DirectoryEntry{}
	for _, a := range addresses {
		if e, ok := s.entries[a]; ok {
			out[a] = e
		}
	}
	return out, nil
}

const (
	testDirSDF  = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	testDirPool = "CA242XKXANKC46P53M355OPYWMHWPPTKQM5T5DNMOBWJMHOWDLNPJTN4"
)

func testDirectoryStub() *stubDirectoryReader {
	return &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
		testDirSDF: {
			Address: testDirSDF, Name: "SDF Growth 3", Domain: "stellar.org",
			Tags: []string{"sdf", "custodian"}, Source: "stellar-expert",
		},
		testDirPool: {
			Address: testDirPool, Name: "Aquarius Pool", Domain: "aqua.network",
			Tags: []string{"defi"}, Source: "stellar-expert",
		},
	}}
}

func TestDirectoryLookup_BatchResolvesListedAddressesOnly(t *testing.T) {
	srv := v1.New(v1.Options{Explorer: &stubExplorerReader{}, Directory: testDirectoryStub()})
	ts := startHTTPTest(t, srv.Handler())

	// One listed G, one listed C, one valid-but-unlisted G.
	resp := mustGet(t, ts.URL+"/v1/directory?addresses="+testDirSDF+","+testDirPool+
		",GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.DirectoryLookupView `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(env.Data.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (unlisted address must be absent, not null): %s", len(env.Data.Entries), body)
	}
	sdf := env.Data.Entries[testDirSDF]
	if sdf.Name != "SDF Growth 3" || sdf.Source != "stellar-expert" {
		t.Errorf("SDF entry = %+v", sdf)
	}
	if got := env.Data.Entries[testDirPool].Tags; len(got) != 1 || got[0] != "defi" {
		t.Errorf("pool tags = %v, want [defi]", got)
	}
}

func TestDirectoryLookup_RejectsInvalidAddress(t *testing.T) {
	srv := v1.New(v1.Options{Explorer: &stubExplorerReader{}, Directory: testDirectoryStub()})
	ts := startHTTPTest(t, srv.Handler())

	for _, q := range []string{
		"",                                     // missing
		"addresses=",                           // empty
		"addresses=GNOTAREALKEY",               // malformed
		"addresses=" + testDirSDF + ",notakey", // one bad in a list
	} {
		resp := mustGet(t, ts.URL+"/v1/directory?"+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400", q, resp.StatusCode)
		}
		_, _ = readAll(resp)
	}
}

func TestDirectoryLookup_NoReaderIs503(t *testing.T) {
	srv := v1.New(v1.Options{Explorer: &stubExplorerReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/directory?addresses="+testDirSDF)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no directory reader is wired", resp.StatusCode)
	}
	_, _ = readAll(resp)
}

// TestAccountState_CarriesDirectoryLabel — the account view embeds the
// label; a directory read failure must degrade to omission, never fail
// the account response.
func TestAccountState_CarriesDirectoryLabel(t *testing.T) {
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: true, Balance: 42, HomeDomain: "stellar.org"},
	}
	srv := v1.New(v1.Options{Explorer: explorer, Directory: testDirectoryStub()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/accounts/"+testDirSDF)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.AccountStateView `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.Directory == nil || env.Data.Directory.Name != "SDF Growth 3" {
		t.Fatalf("directory = %+v, want SDF Growth 3", env.Data.Directory)
	}
	if got := env.Data.Directory.Tags; len(got) != 2 || got[0] != "sdf" {
		t.Errorf("tags = %v, want [sdf custodian]", got)
	}
}

func TestAccountState_DirectoryReadFailureDegradesToOmission(t *testing.T) {
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: true, Balance: 42},
	}
	srv := v1.New(v1.Options{
		Explorer:  explorer,
		Directory: &stubDirectoryReader{err: context.DeadlineExceeded},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/accounts/"+testDirSDF)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (directory outage must not fail the account view)", resp.StatusCode)
	}
	var env struct {
		Data v1.AccountStateView `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Directory != nil {
		t.Errorf("directory = %+v, want omitted on read failure", env.Data.Directory)
	}
	if env.Data.Balance != "42" {
		t.Errorf("balance = %q — account payload must be intact", env.Data.Balance)
	}
}
