package clickhouse

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/cometlp"
)

const (
	testBLNTUSDCPool = "CDQMIKIHKA5UY3FQJ2UDTLNX3GOPD3GPNZWSPTOBVOHEUMQJQXOPC7Y3"
	testBLNTXLMPool  = "CA5HX3D47PQZWC2E7VWXCKH4534P2NOJEOIXAWU47MI4QV2FQUYY3FLC"
)

type cometFixtureEntry struct {
	Pool     string `json:"pool"`
	Kind     string `json:"kind"`
	Holder   string `json:"holder"`
	Ledger   uint32 `json:"ledger_seq"`
	KeyXDR   string `json:"key_xdr"`
	EntryXDR string `json:"entry_xdr"`
}

func cometFixture(t *testing.T) []cometFixtureEntry {
	t.Helper()
	raw, err := os.ReadFile("../../../test/fixtures/comet_storage/pool_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []cometFixtureEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each captured Comet key is an exact key of its own kind, built by the same helpers; LP holder balances, the token list
// and the same key shape on another contract are never captured.
func TestLendingPrefixes_CometCapturesExactKeys(t *testing.T) {
	var reg []LendingContract
	for _, id := range cometlp.Pools() {
		reg = append(reg, LendingContract{ID: id, Protocol: LendingComet})
	}
	ps, err := LendingPrefixes(reg)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range cometFixture(t) {
		got := matchingKeys(ps, e.KeyXDR)
		if e.Kind == "Balance" {
			if len(got) != 0 {
				t.Errorf("LP Balance(%s) of %s matched %v", e.Holder, e.Pool, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != e.Pool+"/"+e.Kind {
			t.Errorf("real %s key of %s matched %v", e.Kind, e.Pool, got)
		}
		seen[e.Pool+"/"+e.Kind] = true
	}
	if len(seen) != 9 {
		t.Errorf("fixture covered %d pool keys, want 9", len(seen))
	}
	for name, key := range map[string]string{
		"AllTokenVec": testDataKey(t, testBLNTUSDCPool, testUnitKey("AllTokenVec")),
		"foreign":     testDataKey(t, "CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D", testUnitKey(cometlp.KeyAllRecordData)),
	} {
		if got := matchingKeys(ps, key); len(got) != 0 {
			t.Errorf("%s matched %v", name, got)
		}
	}
}

func cometRow(t *testing.T, key, entry string, ledger uint32, at time.Time) stateRow {
	t.Helper()
	return stateRow{key: key, protocol: LendingComet, ledger: ledger, close: at, entry: entry}
}

// The fold carries each key forward hour by hour, reports hours before the pool's first full state, decodes the real
// entries, and refuses an entry of another contract filed under the pool.
func TestFoldCometHours(t *testing.T) {
	pool := testBLNTUSDCPool
	keys, err := cometHistoryKeys(pool)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]cometFixtureEntry{}
	for _, e := range cometFixture(t) {
		if e.Pool == pool {
			byKind[e.Kind] = e
		}
	}
	for i, kind := range []string{cometlp.KeyAllRecordData, cometlp.KeyTotalShares, "Instance"} {
		if byKind[kind].KeyXDR != keys[i] {
			t.Fatalf("built %s key %s, real %s", kind, keys[i], byKind[kind].KeyXDR)
		}
	}
	h0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	row := func(kind string, ledger uint32, at time.Time) stateRow {
		return cometRow(t, byKind[kind].KeyXDR, byKind[kind].EntryXDR, ledger, at)
	}
	seed := []stateRow{row("Instance", 100, h0.Add(-time.Hour)), row(cometlp.KeyTotalShares, 101, h0.Add(-time.Minute))}
	rng := []stateRow{row(cometlp.KeyAllRecordData, 110, h0.Add(time.Hour+30*time.Minute))}
	hs, err := foldCometHours(pool, keys, h0, 4, seed, rng)
	if err != nil {
		t.Fatal(err)
	}
	if hs[0].State != nil || hs[0].Missing != cometlp.KeyAllRecordData+" not recorded" {
		t.Errorf("hour 0 = %+v, want no state yet", hs[0])
	}
	for _, h := range hs[1:] {
		if h.State == nil || h.Ledger != 110 || h.State.Records[1].Balance.String() != "38919029596" {
			t.Errorf("hour %s = %+v, want the record at ledger 110 carried forward", h.Hour, h)
		}
	}
	removed, err := foldCometHours(pool, keys, h0, 2, append(seed, row(cometlp.KeyAllRecordData, 102, h0.Add(-time.Minute))),
		[]stateRow{cometRow(t, keys[0], "", 111, h0.Add(time.Hour))})
	if err != nil || removed[0].State == nil || removed[1].State != nil || removed[1].Missing != cometlp.KeyAllRecordData+" removed" {
		t.Errorf("removal: %+v (%v)", removed, err)
	}
	foreign := byKind[cometlp.KeyAllRecordData]
	other := cometFixtureEntryOf(t, testBLNTXLMPool, cometlp.KeyAllRecordData)
	_, err = foldCometHours(pool, keys, h0, 1, append(seed, cometRow(t, foreign.KeyXDR, other.EntryXDR, 103, h0)), nil)
	if !errors.Is(err, cometlp.ErrNotGatedPool) {
		t.Errorf("another pool's entry under this pool's key: err = %v, want ErrNotGatedPool", err)
	}
}

func cometFixtureEntryOf(t *testing.T, pool, kind string) cometFixtureEntry {
	t.Helper()
	for _, e := range cometFixture(t) {
		if e.Pool == pool && e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s %s fixture", pool, kind)
	return cometFixtureEntry{}
}

func TestCometEntryVal_RejectsNonContractData(t *testing.T) {
	if _, err := cometEntryVal(testBLNTUSDCPool, "AAAA"); !errors.Is(err, cometlp.ErrMalformedStorage) {
		t.Errorf("garbage entry: err = %v", err)
	}
	acct, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeTtl, Ttl: &xdr.TtlEntry{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cometEntryVal(testBLNTUSDCPool, acct); !errors.Is(err, cometlp.ErrMalformedStorage) {
		t.Errorf("ttl entry: err = %v", err)
	}
}
