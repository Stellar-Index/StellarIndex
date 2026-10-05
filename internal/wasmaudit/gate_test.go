// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package wasmaudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type fakeHistory struct {
	byContract map[string][]clickhouse.ContractCodeVersion
	errs       map[string]error
	fallback   []clickhouse.ContractCodeVersion // any contract not listed
	asked      []string
}

func (f *fakeHistory) ReplayCodeHistory(_ context.Context, c string) ([]clickhouse.ContractCodeVersion, error) {
	f.asked = append(f.asked, c)
	if err, ok := f.errs[c]; ok {
		return nil, err
	}
	if vs, ok := f.byContract[c]; ok {
		return vs, nil
	}
	if f.fallback != nil {
		return f.fallback, nil
	}
	return nil, clickhouse.ErrContractWasmUnresolved
}

func v(ledger uint32, hash string) clickhouse.ContractCodeVersion {
	return clickhouse.ContractCodeVersion{Ledger: ledger, WasmHash: hash}
}

func mustLoad(t *testing.T) map[string]Entry {
	t.Helper()
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func hashFor(t *testing.T, m map[string]Entry, source string) string {
	t.Helper()
	for h, e := range m {
		if e.Covers(source) {
			return h
		}
	}
	t.Fatalf("no manifest hash for %s", source)
	return ""
}

const (
	hA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParse_DuplicateKeyRefused(t *testing.T) {
	e := `{"sources":["phoenix"],"role":"pool","audited":"2026-01-01","doc":"d.md"}`
	_, err := parse([]byte(fmt.Sprintf(`{"%s":%s,"%s":%s}`, hA, e, hA, e)))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate hash: err = %v, want duplicate refusal", err)
	}
	if _, err := parse([]byte(fmt.Sprintf(`{"%s":{"source":"phoenix","doc":"d.md"}}`, hA))); err == nil {
		t.Fatal("legacy single-source field accepted; want unknown-field refusal")
	}
	if _, err := parse([]byte(fmt.Sprintf(`{"%s":%s,"%s":%s}`, hA, e, hB, e))); err != nil {
		t.Fatalf("distinct hashes: %v", err)
	}
}

func TestLoad_EmbeddedManifest(t *testing.T) {
	m := mustLoad(t)
	for h, e := range m {
		if !strings.HasPrefix(e.Doc, "docs/operations/wasm-audits/") {
			t.Errorf("%s cites %q, want an audit doc", h, e.Doc)
		}
	}
}

func TestCheck_ReflectorDEXAndCEXShareAHash(t *testing.T) {
	m := mustLoad(t)
	const v2, v3 = "4a64c8c8502df326f4ce06d98998dc7d8a61575a11d6c0fbd4c60d10dfe28ffa", "df88820e231ad8f3027871e5dd3cf45491d7b7735e785731466bfc2946008608"
	h := &fakeHistory{fallback: []clickhouse.ContractCodeVersion{v(50_644_229, v2), v(51_656_692, v3)}}
	for _, src := range []string{"reflector-dex", "reflector-cex"} {
		if err := Check(context.Background(), h, m, src, []string{"C1"}, 50_000_000, 60_000_000); err != nil {
			t.Errorf("%s over both shared hashes: %v", src, err)
		}
	}
	// v2 never served FX: the shared entry must not leak to a third source.
	if err := Check(context.Background(), h, m, "reflector-fx", []string{"C1"}, 50_000_000, 60_000_000); err == nil || !strings.Contains(err.Error(), v2) {
		t.Errorf("reflector-fx over v2: err = %v, want refusal naming %s", err, v2)
	}
}

func TestCheck_UnauditedHashMidRangeRefused(t *testing.T) {
	m := map[string]Entry{hA: {Sources: []string{"phoenix"}, Doc: "d"}}
	h := &fakeHistory{byContract: map[string][]clickhouse.ContractCodeVersion{"CPOOL": {v(100, hA), v(200, hB)}}}
	err := Check(context.Background(), h, m, "phoenix", []string{"CPOOL"}, 50, 300)
	if err == nil || !strings.Contains(err.Error(), hB) || !strings.Contains(err.Error(), "CPOOL") || !strings.Contains(err.Error(), "ledger 200") {
		t.Fatalf("B set mid-range: err = %v, want refusal naming contract, hash and ledger", err)
	}
	if err := Check(context.Background(), h, m, "phoenix", []string{"CPOOL"}, 50, 199); err != nil {
		t.Fatalf("range ending before B: %v", err)
	}
	if err := Check(context.Background(), h, m, "phoenix", []string{"CPOOL"}, 250, 300); err == nil {
		t.Fatal("B already active at from: admitted")
	}
	if err := Check(context.Background(), h, m, "aquarius", []string{"CPOOL"}, 50, 150); err == nil {
		t.Fatal("hash attested to another source: admitted")
	}
}

func TestActiveVersions(t *testing.T) {
	vs := []clickhouse.ContractCodeVersion{v(100, "a"), v(200, "b"), v(300, "c")}
	for _, tc := range []struct {
		from, to uint32
		want     string
	}{
		{50, 99, ""}, {50, 150, "a"}, {150, 250, "ab"}, {200, 200, "b"}, {250, 400, "bc"}, {400, 500, "c"}, {100, 299, "ab"},
	} {
		var got string
		for _, x := range activeVersions(vs, tc.from, tc.to) {
			got += x.WasmHash
		}
		if got != tc.want {
			t.Errorf("[%d,%d] = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestCheck_IncompleteHistoryRefused(t *testing.T) {
	m := map[string]Entry{hA: {Sources: []string{"phoenix"}, Doc: "d"}}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"no genesis watermark": {clickhouse.ErrInstanceHistoryIncomplete, "ch-instance-backfill"},
		"no instance rows":     {clickhouse.ErrContractWasmUnresolved, "no instance history"},
		"truncated":            {clickhouse.ErrCodeHistoryTruncated, "truncated"},
		"lake error":           {errors.New("dial tcp: refused"), "refused"},
	} {
		h := &fakeHistory{errs: map[string]error{"C1": tc.err}}
		err := Check(context.Background(), h, m, "phoenix", []string{"C1"}, 1, 10)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want refusal containing %q", name, err, tc.want)
		}
	}
	if err := Check(context.Background(), &fakeHistory{}, m, "phoenix", nil, 1, 10); err == nil {
		t.Error("empty contract set admitted")
	}
	sac := &fakeHistory{errs: map[string]error{"CSAC": clickhouse.ErrContractIsSAC}, byContract: map[string][]clickhouse.ContractCodeVersion{"C1": {v(1, hA)}}}
	if err := Check(context.Background(), sac, m, "phoenix", []string{"CSAC", "C1"}, 1, 10); err != nil {
		t.Errorf("SAC contract not skipped: %v", err)
	}
}

func TestGate_SEP41ExemptAndPolicy(t *testing.T) {
	ctx := context.Background()
	if err := Gate(ctx, Deps{}, nil, nil, []string{"sep41_transfers", "sep41_supply", "sdex", "binance"}, 1, 10); err != nil {
		t.Fatalf("exempt/NoWASM sources gated: %v", err)
	}
	// GateReplay must not open the lake for them: this address is unroutable.
	if err := GateReplay(ctx, "127.0.0.1:1", config.OracleConfig{}, nil, []string{"sep41_supply"}, 1, 10); err != nil {
		t.Fatalf("sep41-only GateReplay: %v", err)
	}
	for _, s := range []string{"upshift", "not-a-source"} {
		if err := Gate(ctx, Deps{}, nil, nil, []string{s}, 1, 10); err == nil {
			t.Errorf("%s admitted", s)
		}
	}
}

func TestGate_ProtocolContractsAdmittedContractIsChecked(t *testing.T) {
	m := mustLoad(t)
	extra := soroswap_router.MainnetRouter // stands in for a protocol_contracts row
	h := &fakeHistory{
		fallback:   []clickhouse.ContractCodeVersion{v(1, hashFor(t, m, "comet"))},
		byContract: map[string][]clickhouse.ContractCodeVersion{extra: {v(1, hA)}},
	}
	d := Deps{ProtocolContracts: func(_ context.Context, source string) ([]string, error) {
		if source != "comet" {
			t.Errorf("protocol_contracts asked for %q", source)
		}
		return []string{extra}, nil
	}}
	// comet's gate is the production decoder built from pipeline's gated registry.
	err := Gate(context.Background(), d, h, m, []string{"comet"}, 1, 100)
	if err == nil || !strings.Contains(err.Error(), extra) {
		t.Fatalf("err = %v, want refusal naming the protocol_contracts row %s", err, extra)
	}
	d.ProtocolContracts = func(context.Context, string) ([]string, error) { return nil, nil }
	h.asked = nil
	if err := Gate(context.Background(), d, h, m, []string{"comet"}, 1, 100); err != nil {
		t.Fatalf("curated set only: %v", err)
	}
	if len(h.asked) == 0 {
		t.Fatal("no comet contract was checked")
	}
}

func TestGateReplay_UnreachableLakeRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := GateReplay(ctx, "127.0.0.1:1", config.OracleConfig{}, nil, []string{"phoenix"}, 1, 0); err == nil {
		t.Fatal("unreachable lake admitted a PerWASM replay")
	}
}

func TestContractSet_EveryPerWASMSourceResolves(t *testing.T) {
	oracle := config.OracleConfig{}
	oracle.Reflector.DEXContract, oracle.Reflector.CEXContract, oracle.Reflector.FXContract = "CD", "CC", "CF"
	oracle.Redstone.AdapterContract, oracle.Band.StandardReferenceContract = "CR", "CB"
	m := mustLoad(t)
	for _, s := range []string{"cctp", "rozo", "sorocredit", "blend_backstop", "soroswap-router", "reflector-dex", "reflector-cex", "reflector-fx", "redstone", "band", "comet", "blend_emitter", "upshift"} {
		got, err := ContractSet(context.Background(), Deps{Oracle: oracle}, s, 1)
		if err != nil || len(got) == 0 {
			t.Errorf("%s: %v contracts, err %v", s, got, err)
		}
	}
	for h, e := range m {
		for _, s := range e.Sources {
			if _, err := ContractSet(context.Background(), Deps{Oracle: oracle}, s, 1); err != nil && strings.Contains(err.Error(), "no contract resolver") {
				t.Errorf("manifest hash %s names %s, which has no resolver", h, s)
			}
		}
	}
}

// fakeStream serves evs (ledger-ordered) for any contract filter: the
// sorocredit child walk streams every emitter of its topics.
type fakeStream struct{ evs []events.Event }

func (f fakeStream) StreamContractEvents(_ context.Context, from, to uint32, _, _ []string, fn func(events.Event) error) error {
	for _, ev := range f.evs {
		if ev.Ledger >= from && ev.Ledger <= to {
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// Real mainnet frames (internal/sources/sorocredit/source_test.go goldens).
const (
	soroCreateTopic1 = "AAAAEgAAAAFvwu+BE6690V76q9574JuQfy8McX+YXK2gl/mCZ70Xpw=="
	soroCreateData   = "AAAAEAAAAAEAAAACAAAADgAAAC9Db2xsYXRlcmFsLTAzODU1MzRhLTczY2EtNDIyZi1iNDQ5LTU5YTExOTZhYWNiNgAAAAASAAAAAAAAAAB4poQ4eoY+oU3UIUJVTMJaJFQwlykKA/LmJa9ILTqSaQ=="
	soroWithdrawData = "AAAAEAAAAAEAAAADAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAABIAAAAAAAAAAIbxw2DJ/zT7LB/y7us78QoFnH2oRpR8ZWw85zd37rm3AAAACgAAAAAAAAAAAAAAAAFrKMA="
)

func soroEvent(contract string, ledger uint32, topics []string, data string) events.Event {
	return events.Event{
		Type: "contract", ContractID: contract, Ledger: ledger, LedgerClosedAt: "2026-07-06T15:25:16Z",
		TxHash: "6714f83ef3f94a76f0158ff4ee76a6a452cb5677cf6c10024583b2b5974ccf8a", Topic: topics, Value: data,
	}
}

// TestGate_SorocreditChildThatEmitsIsChecked: the sorocredit childgate
// honours events from any Collateral child the root announced, so a child
// that emits must have its WASM gated, not only the root's.
func TestGate_SorocreditChildThatEmitsIsChecked(t *testing.T) {
	m := mustLoad(t)
	ctx := context.Background()
	create := soroEvent(sorocredit.MainnetContract, sorocredit.GenesisLedger+10,
		[]string{scval.MustEncodeSymbol(sorocredit.TopicNewCollateralContract), soroCreateTopic1}, soroCreateData)
	outs, err := sorocredit.NewDecoder().Decode(create)
	if err != nil || len(outs) != 1 {
		t.Fatalf("fixture creation event: %v, %d outs", err, len(outs))
	}
	child := outs[0].(sorocredit.Event).CollateralContract
	withdraw := soroEvent(child, sorocredit.GenesisLedger+20,
		[]string{scval.MustEncodeSymbol(sorocredit.TopicWithdrawal), soroCreateTopic1}, soroWithdrawData)
	tip := sorocredit.GenesisLedger + 100
	h := &fakeHistory{
		byContract: map[string][]clickhouse.ContractCodeVersion{sorocredit.MainnetContract: {v(sorocredit.GenesisLedger, hashFor(t, m, "sorocredit"))}},
		fallback:   []clickhouse.ContractCodeVersion{v(sorocredit.GenesisLedger+10, hB)},
	}

	silent := Deps{Events: fakeStream{evs: []events.Event{create}}}
	if err := Gate(ctx, silent, h, m, []string{"sorocredit"}, sorocredit.GenesisLedger, tip); err != nil {
		t.Fatalf("silent children: %v", err)
	}
	if strings.Contains(strings.Join(h.asked, ","), child) {
		t.Errorf("a child that emits nothing was looked up: %v", h.asked)
	}

	emitting := Deps{Events: fakeStream{evs: []events.Event{create, withdraw}}}
	err = Gate(ctx, emitting, h, m, []string{"sorocredit"}, sorocredit.GenesisLedger, tip)
	if err == nil || !strings.Contains(err.Error(), child) || !strings.Contains(err.Error(), hB) {
		t.Fatalf("err = %v, want refusal naming child %s and its WASM %s", err, child, hB)
	}

	if err := Gate(ctx, Deps{Events: fakeStream{}}, h, m, []string{"sorocredit"}, sorocredit.GenesisLedger, tip); err == nil {
		t.Error("a child walk that saw no NewCollateralContract admitted the replay")
	}
	if err := Gate(ctx, Deps{}, h, m, []string{"sorocredit"}, sorocredit.GenesisLedger, tip); err == nil {
		t.Error("no event stream admitted the replay")
	}
}
