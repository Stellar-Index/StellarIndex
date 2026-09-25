package ingest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A keyed stellar-rpc provider carries its API key in the URL path
// (e.g. .../v2/<KEY>), the same shape RLT-441 found printed raw to
// stderr. logSeedStart must not repeat it.
func TestLogSeedStartRedactsKeyedRPCEndpoint(t *testing.T) {
	var buf bytes.Buffer
	const pathCredentialFragment = "provider-cred-9f2c7a1b4e6d8091"
	logSeedStart(&buf, "CFACTORYCONTRACTSTRKEY", "https://rpc.example-provider.com/v2/"+pathCredentialFragment)

	out := buf.String()
	if strings.Contains(out, pathCredentialFragment) {
		t.Fatalf("RPC endpoint secret leaked into log line: %q", out)
	}
	if !strings.Contains(out, "factory=CFACTORYCONTRACTSTRKEY") {
		t.Fatalf("factory contract missing from log line: %q", out)
	}
	const wantRedacted = "rpc=https://rpc.example-provider.com/<redacted>"
	if !strings.Contains(out, wantRedacted) {
		t.Fatalf("expected redacted endpoint form %q, got: %q", wantRedacted, out)
	}
}

type fakeSoroswapPairStore struct {
	rows     []timescale.SoroswapPair
	inserts  []timescale.SoroswapPair
	lostRace map[string]bool
}

func (f *fakeSoroswapPairStore) LoadSoroswapPairRegistry(context.Context) ([]timescale.SoroswapPair, error) {
	return f.rows, nil
}

func (f *fakeSoroswapPairStore) InsertSoroswapPairIfAbsent(_ context.Context, pair, t0, t1 string) (bool, error) {
	if f.lostRace[pair] {
		return false, nil
	}
	f.inserts = append(f.inserts, timescale.SoroswapPair{PairStrkey: pair, Token0Strkey: t0, Token1Strkey: t1})
	return true, nil
}

// An RPC answer that contradicts a registered pair (here: token0/token1
// swapped) must never reach the store; the run fails and names the pair.
// A pair the registry lacks is inserted.
func TestSoroswapPairSeederNeverRewritesRegisteredPair(t *testing.T) {
	store := &fakeSoroswapPairStore{rows: []timescale.SoroswapPair{
		{PairStrkey: "PAIR_P", Token0Strkey: "TOKEN_A", Token1Strkey: "TOKEN_B"},
		{PairStrkey: "PAIR_S", Token0Strkey: "TOKEN_A", Token1Strkey: "TOKEN_C"},
	}}
	var out bytes.Buffer
	s, err := newSoroswapPairSeeder(context.Background(), &out, store, true)
	if err != nil {
		t.Fatal(err)
	}
	s.seed("PAIR_P", "TOKEN_B", "TOKEN_A")
	s.seed("PAIR_S", "TOKEN_A", "TOKEN_C")
	s.seed("PAIR_N", "TOKEN_B", "TOKEN_C")

	want := timescale.SoroswapPair{PairStrkey: "PAIR_N", Token0Strkey: "TOKEN_B", Token1Strkey: "TOKEN_C"}
	if len(store.inserts) != 1 || store.inserts[0] != want {
		t.Fatalf("store writes = %#v, want only %#v", store.inserts, want)
	}
	if !strings.Contains(out.String(), "CONFLICT pair PAIR_P") {
		t.Errorf("output does not report the PAIR_P conflict:\n%s", out.String())
	}
	err = s.finish()
	if err == nil || !strings.Contains(err.Error(), "1 pairs disagree") {
		t.Fatalf("finish() = %v, want the 1-conflict failure", err)
	}
	if got := [3]int64{s.inserted.Load(), s.unchanged.Load(), s.conflicts.Load()}; got != [3]int64{1, 1, 1} {
		t.Errorf("inserted/unchanged/conflicts = %v, want [1 1 1]", got)
	}
}

func TestSoroswapPairSeederDryRunWritesNothing(t *testing.T) {
	store := &fakeSoroswapPairStore{}
	s, err := newSoroswapPairSeeder(context.Background(), &bytes.Buffer{}, store, false)
	if err != nil {
		t.Fatal(err)
	}
	s.seed("PAIR_N", "TOKEN_A", "TOKEN_B")
	if len(store.inserts) != 0 {
		t.Fatalf("dry run wrote %#v", store.inserts)
	}
	if err := s.finish(); err != nil || s.inserted.Load() != 1 {
		t.Fatalf("finish() = %v, would-insert = %d; want nil, 1", err, s.inserted.Load())
	}
}

// The indexer can register a pair between the snapshot and the insert;
// the store keeps the indexer's row and the run still succeeds.
func TestSoroswapPairSeederKeepsRowRegisteredDuringSweep(t *testing.T) {
	store := &fakeSoroswapPairStore{lostRace: map[string]bool{"PAIR_N": true}}
	s, err := newSoroswapPairSeeder(context.Background(), &bytes.Buffer{}, store, true)
	if err != nil {
		t.Fatal(err)
	}
	s.seed("PAIR_N", "TOKEN_A", "TOKEN_B")
	if err := s.finish(); err != nil || s.inserted.Load() != 0 || s.unchanged.Load() != 1 {
		t.Fatalf("finish() = %v, inserted=%d unchanged=%d; want nil, 0, 1",
			err, s.inserted.Load(), s.unchanged.Load())
	}
}
