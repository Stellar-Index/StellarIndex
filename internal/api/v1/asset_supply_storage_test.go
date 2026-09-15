package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The lake reader MUST satisfy the seam. A supply source that is silently not
// wired reports zero rather than failing, which is the whole bug class this
// endpoint's storage fallback exists to close — so the assertion is
// compile-time rather than a runtime nil check.
var _ ContractStorageSupplyReader = (*clickhouse.ExplorerReader)(nil)

// storageDealContractID and storageDealTotal are the private-credit deal token
// measured on r1 2026-09-15: six balance entries summing to this figure, which
// equals the contract's own declared TotalSupply and matches its declared
// HolderCount of six.
const (
	storageDealContractID = "CAOCXWNXCG63U43DCJSS52FDAFT2ZXY2T2UZF3ZYVZ4LLLFVFRF2GM6Z"
	storageDealTotal      = "344995973100000"
)

type fakeStorageSupply struct {
	calls int
	out   clickhouse.ContractStorageSupply
	err   error
}

func (f *fakeStorageSupply) ContractStorageSupply(_ context.Context, contractID string) (clickhouse.ContractStorageSupply, error) {
	f.calls++
	if f.err != nil {
		return clickhouse.ContractStorageSupply{}, f.err
	}
	out := f.out
	out.ContractID = contractID
	return out, nil
}

func dealStorageSupply() clickhouse.ContractStorageSupply {
	total, _ := new(big.Int).SetString(storageDealTotal, 10)
	holders := uint32(6)
	return clickhouse.ContractStorageSupply{
		Total:           total,
		Decimals:        7,
		DecimalsFound:   true,
		BalanceEntries:  6,
		DeclaredTotal:   new(big.Int).Set(total),
		DeclaredHolders: &holders,
		AsOfLedger:      64165090,
	}
}

func serveSupplyWithStorage(t *testing.T, flows clickhouse.TokenSupply, st ContractStorageSupplyReader, assetID string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{
		tokenSupply:   &fakeTokenSupply{supply: flows},
		storageSupply: st,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/assets/{asset_id}/supply", srv.handleAssetSupply)
	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/supply", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// zeroFlows is what supply_flows returns for a contract it has never seen: a
// clean scan to zeros, indistinguishable at the call site from a token that
// really was fully burned.
func zeroFlows() clickhouse.TokenSupply {
	return clickhouse.TokenSupply{
		Total: big.NewInt(0), Mint: big.NewInt(0),
		Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 0,
	}
}

// TestSupplyFallsBackToContractStorageWhenNoFlows is the test that would have
// caught the gap this endpoint shipped with.
//
// Twenty-four private-credit deal tokens emitted no SEP-41 events at all, so
// every one of them served a confident total_supply of "0" while holding a
// nine-figure supply in contract storage.
func TestSupplyFallsBackToContractStorageWhenNoFlows(t *testing.T) {
	st := &fakeStorageSupply{out: dealStorageSupply()}
	rec := serveSupplyWithStorage(t, zeroFlows(), st, storageDealContractID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
	}
	got := decodeSupply(t, rec.Body.Bytes())

	if got.TotalSupply != storageDealTotal {
		t.Errorf("total_supply = %q, want %q — a token with no events was served the zero that "+
			"supply_flows scans to", got.TotalSupply, storageDealTotal)
	}
	if got.Source != string(supply.BasisContractStorageBalances) {
		t.Errorf("source = %q, want %q; the basis must say which question was answered",
			got.Source, supply.BasisContractStorageBalances)
	}
	if !got.CirculatingSupplyLowerBound {
		t.Error("circulating_supply_lower_bound is false on a storage-summed figure; state expiry " +
			"can archive a real balance out of view, so the figure is a floor")
	}
	if got.BalanceEntries != 6 {
		t.Errorf("balance_entries = %d, want 6", got.BalanceEntries)
	}
	if got.SupplyConsistent == nil || !*got.SupplyConsistent {
		t.Errorf("supply_consistent = %v, want true", got.SupplyConsistent)
	}
	if got.Decimals == nil || *got.Decimals != 7 {
		t.Errorf("decimals = %v, want 7 read from the contract's own Config map", got.Decimals)
	}
	// The event-only fields must be ABSENT: this figure has no mint/burn basis,
	// and echoing zeros for them would imply one was measured.
	if got.MintTotal != nil || got.BurnTotal != nil || got.ClawbackTotal != nil {
		t.Error("mint/burn/clawback totals are present on a storage-derived figure; there is no " +
			"event basis for them and zeros would read as a measurement")
	}
}

// TestSupplyDoesNotConsultStorageWhenFlowsExist is the double-count guard.
//
// Event-derived and storage-derived supply measure the SAME tokens from
// opposite ends — issuance and distribution. Adding them, or preferring one
// after having summed both, would roughly double every figure that has both.
func TestSupplyDoesNotConsultStorageWhenFlowsExist(t *testing.T) {
	st := &fakeStorageSupply{out: dealStorageSupply()}
	flows := clickhouse.TokenSupply{
		Total: big.NewInt(28327867109034), Mint: big.NewInt(28327867109034),
		Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 19610,
	}
	rec := serveSupplyWithStorage(t, flows, st, storageDealContractID)

	if st.calls != 0 {
		t.Errorf("storage reader consulted %d times for a token that HAS flows; the two readings "+
			"must never both contribute to one figure", st.calls)
	}
	got := decodeSupply(t, rec.Body.Bytes())
	if got.Source != "mint_burn_flows" {
		t.Errorf("source = %q, want mint_burn_flows", got.Source)
	}
	if got.TotalSupply != "28327867109034" {
		t.Errorf("total_supply = %q, want the unmodified event-derived total", got.TotalSupply)
	}
	if got.CirculatingSupplyLowerBound {
		t.Error("lower-bound flag set on an event-derived total, which is not this kind of floor")
	}
}

// TestSupplyKeepsEventReadingWhenStorageDeclines pins that the fallback is a
// fallback: its refusals must not turn a 200 into an error.
func TestSupplyKeepsEventReadingWhenStorageDeclines(t *testing.T) {
	cases := []struct {
		name string
		st   ContractStorageSupplyReader
	}{
		{"not wired", nil},
		{"stellar asset contract", &fakeStorageSupply{err: clickhouse.ErrStorageSupplyIsStellarAsset}},
		{"too many entries", &fakeStorageSupply{err: clickhouse.ErrStorageSupplyTooManyEntries}},
		{"no instance entry captured", &fakeStorageSupply{err: clickhouse.ErrStorageSupplyNoInstance}},
		{"read failed", &fakeStorageSupply{err: errors.New("clickhouse down")}},
		{"contract holds no balances", &fakeStorageSupply{out: clickhouse.ContractStorageSupply{Total: big.NewInt(0)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveSupplyWithStorage(t, zeroFlows(), tc.st, storageDealContractID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — a declining optional source must not fail the "+
					"request (body=%s)", rec.Code, rec.Body)
			}
			got := decodeSupply(t, rec.Body.Bytes())
			if got.Source != "mint_burn_flows" {
				t.Errorf("source = %q, want mint_burn_flows", got.Source)
			}
			if got.CirculatingSupplyLowerBound || got.BalanceEntries != 0 || got.SupplyConsistent != nil {
				t.Error("storage-only fields leaked onto a response the storage reader did not fill")
			}
		})
	}
}

// TestSupplyStorageFallbackFlagsInconsistency pins that a contract whose own
// holder count exceeds what we can see is still SERVED, but marked.
//
// That gap is exactly what an archived contract-data entry looks like from
// here. Refusing would hide a real, mostly-correct figure; serving it silently
// would present a floor as a total.
func TestSupplyStorageFallbackFlagsInconsistency(t *testing.T) {
	out := dealStorageSupply()
	missing := uint32(8) // contract says eight holders; the lake shows six
	out.DeclaredHolders = &missing
	rec := serveSupplyWithStorage(t, zeroFlows(), &fakeStorageSupply{out: out}, storageDealContractID)

	got := decodeSupply(t, rec.Body.Bytes())
	if got.TotalSupply != storageDealTotal {
		t.Errorf("total_supply = %q, want the partial sum still served", got.TotalSupply)
	}
	if got.SupplyConsistent == nil || *got.SupplyConsistent {
		t.Errorf("supply_consistent = %v, want false — the contract declares more holders than "+
			"the lake can show, which is what an archived balance looks like", got.SupplyConsistent)
	}
	if !got.CirculatingSupplyLowerBound {
		t.Error("lower-bound flag cleared on a figure we know is incomplete")
	}
}

// TestSupplyStorageFallbackOmitsDecimalsWhenChainDeclaresNone pins the rule
// that a missing scale is never replaced with a default.
func TestSupplyStorageFallbackOmitsDecimalsWhenChainDeclaresNone(t *testing.T) {
	out := dealStorageSupply()
	out.Decimals, out.DecimalsFound = 0, false
	rec := serveSupplyWithStorage(t, zeroFlows(), &fakeStorageSupply{out: out}, storageDealContractID)

	got := decodeSupply(t, rec.Body.Bytes())
	if got.Decimals != nil {
		t.Errorf("decimals = %v on a contract that declares none; an invented exponent is a "+
			"published money figure wrong by a power of ten", *got.Decimals)
	}
	if got.TotalSupply != storageDealTotal {
		t.Errorf("total_supply = %q; the RAW figure is still defensible without a scale", got.TotalSupply)
	}
}
