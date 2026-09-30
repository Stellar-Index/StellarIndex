// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type fakeDriftLake struct {
	events []events.Event
	hashes map[string]string
	errs   map[string]error
	asked  []string
}

func (f *fakeDriftLake) StreamContractEvents(_ context.Context, _, _ uint32, ids, _ []string, fn func(events.Event) error) error {
	for _, ev := range f.events {
		if slices.Contains(ids, ev.ContractID) {
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeDriftLake) ContractWasmHash(_ context.Context, c string) (string, error) {
	f.asked = append(f.asked, c)
	if err := f.errs[c]; err != nil {
		return "", err
	}
	if h, ok := f.hashes[c]; ok {
		return h, nil
	}
	return "", clickhouse.ErrContractWasmUnresolved
}

func driftContract(t *testing.T, tag byte) string {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = tag ^ byte(i)
	}
	s, err := strkey.Encode(strkey.VersionByteContract, seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// blendDeploy is a Pool Factory `deploy` event announcing pool, the shape
// the blend decoder registers a child from.
func blendDeploy(t *testing.T, factory, pool string) events.Event {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, pool)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	b, err := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{
		ContractID:     factory,
		Topic:          []string{blend.TopicSymbolDeploy},
		Value:          base64.StdEncoding.EncodeToString(b),
		Ledger:         blend.FactoryGenesisLedger + 10,
		TxHash:         "wasmdrifttx",
		LedgerClosedAt: "2026-04-29T12:00:00Z",
	}
}

func hash64(c byte) string { return strings.Repeat(string(c), 64) }

func verdicts(rep wasmDriftReport) map[string]string {
	out := make(map[string]string)
	for _, r := range rep.rows {
		out[r.contract] = r.verdict
	}
	return out
}

// A factory child running a hash no audit log names is drift; the audited
// child, the SAC and the unresolvable factory are reported but not drift.
func TestRunWasmDrift_flagsUnauditedHashOnFactoryChild(t *testing.T) {
	factories := blend.MainnetPoolFactories
	audited, drifted, sac := driftContract(t, 0x11), driftContract(t, 0x22), driftContract(t, 0x33)
	lake := &fakeDriftLake{
		events: []events.Event{
			blendDeploy(t, factories[0], audited),
			blendDeploy(t, factories[0], drifted),
			blendDeploy(t, factories[0], sac),
		},
		hashes: map[string]string{factories[0]: hash64('f'), audited: hash64('a'), drifted: hash64('d')},
		errs:   map[string]error{sac: clickhouse.ErrContractIsSAC},
	}
	manifest := map[string]auditedWasm{
		hash64('a'): {Source: blend.SourceName, Role: "pool"},
		hash64('f'): {Source: blend.SourceName, Role: "factory"},
	}

	rep, err := runWasmDrift(context.Background(), lake, manifest, []string{blend.SourceName}, blend.FactoryGenesisLedger+100)
	if err != nil {
		t.Fatal(err)
	}
	got := verdicts(rep)
	want := map[string]string{
		audited:      wasmDriftAudited,
		drifted:      wasmDriftDrift,
		sac:          wasmDriftSAC,
		factories[0]: wasmDriftAudited,
	}
	for c, v := range want {
		if got[c] != v {
			t.Errorf("%s: verdict %q, want %q", c, got[c], v)
		}
	}
	for _, f := range factories[1:] {
		if got[f] != wasmDriftUnresolved {
			t.Errorf("factory %s with no instance entry: verdict %q, want %q", f, got[f], wasmDriftUnresolved)
		}
	}
	if rep.drifting() != 1 {
		t.Errorf("drifting=%d, want 1", rep.drifting())
	}
	if rep.checked[blend.SourceName] != len(factories)+3 {
		t.Errorf("checked=%d, want %d", rep.checked[blend.SourceName], len(factories)+3)
	}
}

// A hash audited for a different source proves nothing about this one.
func TestRunWasmDrift_hashAuditedForAnotherSourceIsDrift(t *testing.T) {
	factories := blend.MainnetPoolFactories
	pool := driftContract(t, 0x44)
	hashes := map[string]string{pool: hash64('c')}
	for _, f := range factories {
		hashes[f] = hash64('f')
	}
	lake := &fakeDriftLake{events: []events.Event{blendDeploy(t, factories[0], pool)}, hashes: hashes}
	manifest := map[string]auditedWasm{
		hash64('c'): {Source: "comet", Role: "pool"},
		hash64('f'): {Source: blend.SourceName, Role: "factory"},
	}
	rep, err := runWasmDrift(context.Background(), lake, manifest, []string{blend.SourceName}, blend.FactoryGenesisLedger+100)
	if err != nil {
		t.Fatal(err)
	}
	if v := verdicts(rep)[pool]; v != wasmDriftDrift {
		t.Fatalf("pool on a comet-audited hash: verdict %q, want %q", v, wasmDriftDrift)
	}
}

// A gated source with no audit log is reported unaudited and none of its
// contracts is resolved or counted as drift.
func TestRunWasmDrift_sourceWithoutAuditLogIsUnauditedNotDrift(t *testing.T) {
	lake := &fakeDriftLake{}
	manifest := map[string]auditedWasm{hash64('a'): {Source: blend.SourceName}}
	rep, err := runWasmDrift(context.Background(), lake, manifest, []string{"sushiswap_v3", "upshift"}, 70_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.unaudited, []string{"sushiswap_v3", "upshift"}) {
		t.Errorf("unaudited=%v", rep.unaudited)
	}
	if len(rep.rows) != 0 || len(lake.asked) != 0 {
		t.Errorf("unaudited sources were checked: rows=%v asked=%v", rep.rows, lake.asked)
	}
}

// An empty or unreachable lake must fail the run, not pass it with only the
// factories checked.
func TestRunWasmDrift_emptyFactoryWalkFailsClosed(t *testing.T) {
	lake := &fakeDriftLake{}
	manifest := map[string]auditedWasm{hash64('a'): {Source: blend.SourceName}}
	if _, err := runWasmDrift(context.Background(), lake, manifest, []string{blend.SourceName}, blend.FactoryGenesisLedger+100); err == nil {
		t.Fatal("factory walk that found no children returned no error")
	}
}

func TestRunWasmDrift_lakeErrorAborts(t *testing.T) {
	boom := errors.New("ch down")
	lake := &fakeDriftLake{errs: map[string]error{}}
	meta, _ := pipeline.GatedMetaFor("comet")
	for _, c := range meta.CuratedSet {
		lake.errs[c] = boom
	}
	manifest := map[string]auditedWasm{hash64('a'): {Source: "comet"}}
	if _, err := runWasmDrift(context.Background(), lake, manifest, []string{"comet"}, 70_000_000); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want wrapped %v", err, boom)
	}
}

func TestWasmDriftSources(t *testing.T) {
	all, err := wasmDriftSources("")
	if err != nil {
		t.Fatal(err)
	}
	want := pipeline.GatedSourceNames()
	slices.Sort(want)
	if !slices.Equal(all, want) {
		t.Errorf("all=%v want %v", all, want)
	}
	one, err := wasmDriftSources("phoenix")
	if err != nil || !slices.Equal(one, []string{"phoenix"}) {
		t.Errorf("-source phoenix: %v %v", one, err)
	}
	if _, err := wasmDriftSources("soroswap"); err == nil {
		t.Error("-source soroswap (not gated) accepted")
	}
}

func TestWasmDriftExit(t *testing.T) {
	if err := wasmDriftExit(0); err != nil {
		t.Errorf("0 drifting: %v", err)
	}
	for n, want := range map[int]int{3: 3, 300: 255} {
		var ec *opsutil.ExitCodeError
		if err := wasmDriftExit(n); !errors.As(err, &ec) || ec.Code != want {
			t.Errorf("%d drifting: err=%v, want exit %d", n, err, want)
		}
	}
}

func TestRenderWasmDriftProm(t *testing.T) {
	rep := wasmDriftReport{
		rows: []wasmDriftRow{
			{source: "phoenix", contract: "CAAA", wasmHash: hash64('d'), verdict: wasmDriftDrift},
			{source: "phoenix", contract: "CBBB", wasmHash: hash64('a'), verdict: wasmDriftAudited},
			{source: "phoenix", contract: "CCCC", verdict: wasmDriftUnresolved},
			{source: "blend", contract: "CDDD", verdict: wasmDriftSAC},
		},
		checked:   map[string]int{"phoenix": 3, "blend": 1},
		unaudited: []string{"sushiswap_v3"},
	}
	got := renderWasmDriftProm(rep, time.Unix(1_700_000_000, 0))
	for _, line := range []string{
		`stellarindex_wasm_drift{source="phoenix",contract="CAAA",wasm_hash="` + hash64('d') + `"} 1`,
		`stellarindex_wasm_drift_contracts_checked{source="blend"} 1`,
		`stellarindex_wasm_drift_contracts_checked{source="phoenix"} 3`,
		`stellarindex_wasm_drift_contracts_unverifiable{source="blend",reason="sac"} 1`,
		`stellarindex_wasm_drift_contracts_unverifiable{source="phoenix",reason="unresolved"} 1`,
		`stellarindex_wasm_drift_source_unaudited{source="sushiswap_v3"} 1`,
		`stellarindex_wasm_drift_last_run_unix 1700000000`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	if strings.Contains(got, "CBBB") {
		t.Errorf("audited contract rendered as drift:\n%s", got)
	}
}

// Every manifest entry must be traceable to the audit log it cites: the full
// hash, or its first 8 hex chars + "…" as the logs write it.
func TestAuditedWasmManifestMatchesAuditLogs(t *testing.T) {
	m, err := loadAuditedWasm()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("empty manifest")
	}
	gated := pipeline.GatedSourceNames()
	root := filepath.Join("..", "..", "..")
	docs := make(map[string]string)
	for h, e := range m {
		if !slices.Contains(gated, e.Source) {
			t.Errorf("%s: source %q is not a gated source", h, e.Source)
		}
		if e.Role == "" {
			t.Errorf("%s: no role", h)
		}
		if _, err := time.Parse(time.DateOnly, e.Audited); err != nil {
			t.Errorf("%s: audited %q: %v", h, e.Audited, err)
		}
		if want := "docs/operations/wasm-audits/" + e.Source + ".md"; e.Doc != want {
			t.Errorf("%s: doc %q, want %q", h, e.Doc, want)
		}
		body, ok := docs[e.Doc]
		if !ok {
			b, err := os.ReadFile(filepath.Join(root, e.Doc))
			if err != nil {
				t.Fatalf("%s: %v", h, err)
			}
			body = string(b)
			docs[e.Doc] = body
		}
		if !strings.Contains(body, h) && !strings.Contains(body, h[:8]+"…") {
			t.Errorf("%s (%s %s) appears in %s neither in full nor as %s…", h, e.Source, e.Role, e.Doc, h[:8])
		}
	}
}

// A gated source with no manifest entry is skipped as unaudited, so none of
// its contracts would ever be checked for drift.
func TestAuditedWasmManifestCoversEveryGatedSource(t *testing.T) {
	m, err := loadAuditedWasm()
	if err != nil {
		t.Fatal(err)
	}
	audited := make(map[string]bool)
	for _, e := range m {
		audited[e.Source] = true
	}
	for _, s := range pipeline.GatedSourceNames() {
		if !audited[s] {
			t.Errorf("gated source %q has no hash in audited_wasm.json", s)
		}
	}
}
