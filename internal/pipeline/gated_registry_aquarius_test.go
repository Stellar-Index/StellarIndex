package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeAquariusStore layers an in-memory aquarius_pools double over the protocol_contracts one.
type fakeAquariusStore struct {
	fakeProtocolContractStore
	pools   []timescale.AquariusPool
	written []timescale.AquariusPool
	loadErr error
}

func (f *fakeAquariusStore) LoadAquariusPools(context.Context) ([]timescale.AquariusPool, error) {
	return f.pools, f.loadErr
}

func (f *fakeAquariusStore) UpsertAquariusPool(_ context.Context, p timescale.AquariusPool) error {
	f.written = append(f.written, p)
	return nil
}

const unlistedAquariusPool = "CCPHUHQYFOJJ6WQUGUYHHPJYQGFLRQHJJTRJNWQG54MHCHPRFLWQI7SE"

// A pool warmed from protocol_contracts with no layout from an in-code list or aquarius_pools restarts with its
// deposits and withdraws refused; the warm must say so rather than leave only decode_error counts.
func TestGatedRegistryOptions_aquariusPoolWithoutLayoutWarns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []string
		table []timescale.AquariusPool
		want  bool
	}{
		{"unlisted pool", []string{aquarius.MainnetPools[0], unlistedAquariusPool}, nil, true},
		{"listed pools only", []string{aquarius.MainnetPools[0], aquarius.MainnetConcentratedPools[0]}, nil, false},
		{
			"unlisted pool typed by the table",
			[]string{unlistedAquariusPool},
			[]timescale.AquariusPool{{ContractID: unlistedAquariusPool, PoolType: "stable"}},
			false,
		},
		{
			"unlisted pool with an unmappable recorded type",
			[]string{unlistedAquariusPool},
			[]timescale.AquariusPool{{ContractID: unlistedAquariusPool, PoolType: "constant_product"}},
			true,
		},
	} {
		h := &capturingHandler{}
		store := &fakeAquariusStore{
			fakeProtocolContractStore: fakeProtocolContractStore{rows: map[string][]string{aquarius.SourceName: tc.rows}},
			pools:                     tc.table,
		}
		if _, err := gatedRegistryOptions(context.Background(), store, slog.New(h), context.Background(), false); err != nil {
			t.Fatalf("%s: gatedRegistryOptions: %v", tc.name, err)
		}
		got := false
		for _, r := range h.records {
			if r.level >= slog.LevelWarn && r.source == aquarius.SourceName && strings.Contains(r.msg, "NO liquidity layout") {
				got = true
			}
		}
		if got != tc.want {
			t.Errorf("%s: layout warning logged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A layout learned from a live add_pool is written to aquarius_pools and restored by the next boot's warm.
func TestGatedRegistryOptions_aquariusLayoutSurvivesRestart(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	clPool, classicPool := testContractStrkey(t, 0xA1), testContractStrkey(t, 0xA2)
	live := &fakeAquariusStore{fakeProtocolContractStore: fakeProtocolContractStore{rows: map[string][]string{}}}
	opts, err := gatedRegistryOptions(context.Background(), live, quiet, context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	dec := aquarius.NewDecoder(opts[aquarius.SourceName]...)
	for pool, typ := range map[string]string{clPool: "concentrated", classicPool: "constant"} {
		if _, err := dec.Decode(aquariusAddPool(t, pool, typ)); err != nil {
			t.Fatal(err)
		}
	}
	if len(live.written) != 2 {
		t.Fatalf("aquarius_pools writes = %+v, want 2", live.written)
	}

	restarted := &fakeAquariusStore{
		fakeProtocolContractStore: fakeProtocolContractStore{rows: map[string][]string{aquarius.SourceName: {clPool, classicPool}}},
		pools:                     live.written,
	}
	opts, err = gatedRegistryOptions(context.Background(), restarted, quiet, context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	dec = aquarius.NewDecoder(opts[aquarius.SourceName]...)
	for pool, wantCL := range map[string]bool{clPool: true, classicPool: false} {
		out, err := dec.Decode(aquariusDeposit(t, pool))
		if err != nil || len(out) != 1 {
			t.Fatalf("%s: Decode = %v, %v", pool, out, err)
		}
		lq, ok := out[0].(aquarius.LiquidityEvent)
		// body [7, 8, 9]: concentrated reads liquidity 7 and amounts [8 9]; shares-last reads amounts [7 8] and shares 9.
		if !ok || lq.Concentrated != wantCL || (wantCL && lq.Liquidity.String() != "7") || (!wantCL && lq.Shares.String() != "9") {
			t.Errorf("%s: decoded %+v, want concentrated=%v", pool, lq, wantCL)
		}
	}
}

func TestGatedRegistryOptions_aquariusPoolTableFailsClosed(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, store := range map[string]*fakeAquariusStore{
		"recorded type contradicts the in-code list": {
			pools: []timescale.AquariusPool{{ContractID: aquarius.MainnetPools[0], PoolType: "concentrated"}},
		},
		"table unreadable": {loadErr: errors.New("relation does not exist")},
	} {
		store.rows = map[string][]string{}
		if _, err := gatedRegistryOptions(context.Background(), store, quiet, context.Background(), false); err == nil {
			t.Errorf("%s: warm succeeded, want an error", name)
		}
	}
}

func testContractStrkey(t *testing.T, b byte) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = b
	s, err := strkey.Encode(strkey.VersionByteContract, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scB64(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	s, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scContract(t *testing.T, id string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, id)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}}
}

func scVec(vals ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(vals)
	p := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

func scI128(n uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(n)}}
}

func aquariusAddPool(t *testing.T, pool, poolType string) events.Event {
	t.Helper()
	sym := xdr.ScSymbol(poolType)
	return events.Event{
		ContractID: aquarius.MainnetRouter, Ledger: 66_000_000, LedgerClosedAt: "2026-11-26T03:09:59Z",
		Topic: []string{aquarius.TopicSymbolAddPool},
		Value: scB64(t, scVec(scContract(t, pool), xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})),
	}
}

func aquariusDeposit(t *testing.T, pool string) events.Event {
	t.Helper()
	return events.Event{
		ContractID: pool, Ledger: 66_000_001, LedgerClosedAt: "2026-11-26T03:10:04Z",
		Topic: []string{
			aquarius.TopicSymbolDepositLiquidity,
			scB64(t, scContract(t, testContractStrkey(t, 0xB1))), scB64(t, scContract(t, testContractStrkey(t, 0xB2))),
		},
		Value: scB64(t, scVec(scI128(7), scI128(8), scI128(9))),
	}
}
