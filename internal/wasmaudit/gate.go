// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package wasmaudit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// History is the per-contract WASM timeline the gate reads.
type History interface {
	ReplayCodeHistory(ctx context.Context, contractID string) ([]clickhouse.ContractCodeVersion, error)
}

// Lake is the ClickHouse surface of the production gate.
type Lake interface {
	completeness.EventStreamer
	History
}

// CHLake is the production Lake.
type CHLake struct {
	clickhouse.ReconcileEventStreamer
	*clickhouse.ExplorerReader
}

var (
	_ History = (*clickhouse.ExplorerReader)(nil)
	_ Lake    = CHLake{}
)

// NewLake opens the production lake reader at addr.
func NewLake(ctx context.Context, addr string) (CHLake, error) {
	r, err := clickhouse.NewExplorerReader(ctx, addr)
	if err != nil {
		return CHLake{}, err
	}
	return CHLake{ReconcileEventStreamer: clickhouse.ReconcileEventStreamer{Addr: addr}, ExplorerReader: r}, nil
}

// Deps resolves a source's contract set.
type Deps struct {
	Events completeness.EventStreamer
	// ProtocolContracts is the protocol_contracts warm the replay decoder's
	// gate is seeded from; nil reads none.
	ProtocolContracts func(ctx context.Context, source string) ([]string, error)
	Oracle            config.OracleConfig
}

// soroswapGenesis is the Soroswap factory deploy ledger (the reconciliation
// catalogue's floor), the lower bound of its pair walk.
const soroswapGenesis = 50_746_266

// GateReplay is the production replay gate: it opens the lake at chAddr and
// runs Gate. to == 0 means the lake tip. There is no skip: an unreachable lake refuses.
func GateReplay(ctx context.Context, chAddr string, oracle config.OracleConfig,
	protocolContracts func(ctx context.Context, source string) ([]string, error),
	sources []string, from, to uint32,
) error {
	perWASM, err := policyCheck(sources)
	if err != nil || len(perWASM) == 0 {
		return err
	}
	lake, err := NewLake(ctx, chAddr)
	if err != nil {
		return fmt.Errorf("wasm replay gate: open ClickHouse %s (the gate has no skip): %w", chAddr, err)
	}
	defer func() { _ = lake.Close() }()
	if to == 0 {
		if to, err = clickhouse.MaxLedger(ctx, chAddr); err != nil {
			return fmt.Errorf("wasm replay gate: lake tip: %w", err)
		}
	}
	manifest, err := Load()
	if err != nil {
		return err
	}
	return Gate(ctx, Deps{Events: lake, ProtocolContracts: protocolContracts, Oracle: oracle}, lake, manifest, perWASM, from, to)
}

// policyCheck refuses a BackfillUnsafe source and returns the PerWASM ones.
// sep41 (standard schema) and NoWASM sources need no per-hash check.
func policyCheck(sources []string) ([]string, error) {
	var perWASM, unsafe []string
	for _, s := range sources {
		if external.ReplayExempt(s) {
			continue
		}
		switch external.Lookup(external.ReplayAuditSubject(s)).Backfill {
		case external.BackfillNoWASM:
		case external.BackfillPerWASM:
			perWASM = append(perWASM, s)
		default:
			unsafe = append(unsafe, s)
		}
	}
	if len(unsafe) > 0 {
		return nil, fmt.Errorf("wasm replay gate: %v not replay-safe (Backfill policy Unsafe, or not a known source); "+
			"audit every WASM hash under docs/operations/wasm-audits/, add each to internal/wasmaudit/audited_wasm.json "+
			"and set Backfill: BackfillPerWASM in internal/sources/external/registry.go in the same PR", unsafe)
	}
	return perWASM, nil
}

// Gate refuses unless every WASM version active in [from, to] on every
// contract each source's replay decoder admits is attested for that source.
func Gate(ctx context.Context, d Deps, h History, manifest map[string]Entry, sources []string, from, to uint32) error {
	perWASM, err := policyCheck(sources)
	if err != nil {
		return err
	}
	for _, s := range perWASM {
		contracts, err := ContractSet(ctx, d, s, to)
		if err != nil {
			return fmt.Errorf("wasm replay gate: %s contract set: %w", s, err)
		}
		if err := Check(ctx, h, manifest, s, contracts, from, to); err != nil {
			return err
		}
	}
	return nil
}

// Check is Gate for one source over a resolved contract set.
func Check(ctx context.Context, h History, manifest map[string]Entry, source string, contracts []string, from, to uint32) error {
	if len(contracts) == 0 {
		return fmt.Errorf("wasm replay gate: %s resolves to no contracts; cannot prove which WASM ran", source)
	}
	subject := external.ReplayAuditSubject(source)
	var bad []string
	for _, c := range contracts {
		vs, err := h.ReplayCodeHistory(ctx, c)
		switch {
		case errors.Is(err, clickhouse.ErrContractIsSAC):
			continue // no WASM
		case errors.Is(err, clickhouse.ErrInstanceHistoryIncomplete):
			return fmt.Errorf("wasm replay gate: refusing %s: %w (landing precondition: the genesis watermark in stellar.entry_history_watermark)", source, err)
		case errors.Is(err, clickhouse.ErrContractWasmUnresolved):
			return fmt.Errorf("wasm replay gate: refusing %s: contract %s has no instance history in the lake, so the WASM that ran cannot be proven", source, c)
		case err != nil:
			return fmt.Errorf("wasm replay gate: refusing %s: contract %s history: %w", source, c, err)
		}
		for _, v := range activeVersions(vs, from, to) {
			if !manifest[v.WasmHash].Covers(subject) {
				bad = append(bad, fmt.Sprintf("contract %s ran WASM %s from ledger %d", c, v.WasmHash, v.Ledger))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("wasm replay gate: refusing %s over [%d,%d], unaudited for %s: %s. Audit each under docs/operations/wasm-audits/ "+
			"and add it to internal/wasmaudit/audited_wasm.json in the same PR", source, from, to, subject, strings.Join(bad, "; "))
	}
	return nil
}

// activeVersions returns the versions running at some ledger in [from, to]:
// the last one set at or before from, plus every one set inside the range.
// vs is ascending by Ledger.
func activeVersions(vs []clickhouse.ContractCodeVersion, from, to uint32) []clickhouse.ContractCodeVersion {
	var out []clickhouse.ContractCodeVersion
	for i, v := range vs {
		switch {
		case v.Ledger <= from:
			if i+1 == len(vs) || vs[i+1].Ledger > from {
				out = append(out, v)
			}
		case v.Ledger <= to:
			out = append(out, v)
		}
	}
	return out
}

// ContractSet is every contract the source's replay decoder admits by tip:
// its in-code set, the protocol_contracts warm, and every factory child.
func ContractSet(ctx context.Context, d Deps, source string, tip uint32) ([]string, error) {
	set := map[string]struct{}{}
	add := func(ids ...string) {
		for _, id := range ids {
			if id != "" {
				set[id] = struct{}{}
			}
		}
	}
	if meta, ok := pipeline.GatedMetaFor(source); ok {
		seed := append([]string(nil), meta.CuratedSet...)
		if d.ProtocolContracts != nil {
			ids, err := d.ProtocolContracts(ctx, source)
			if err != nil {
				return nil, fmt.Errorf("protocol_contracts: %w", err)
			}
			seed = append(seed, ids...)
		}
		add(seed...)
		add(meta.Factories...)
		dec := meta.NewDecoder(contractid.WithSeed(seed), contractid.WithHook(func(child, _ string, _ uint32) { add(child) }))
		if err := walkFactory(ctx, d.Events, source, dec, meta.Factories, meta.CreationSym, meta.Genesis, tip); err != nil {
			return nil, err
		}
		if g, ok := dec.(interface{ GatedContractSet() []string }); ok {
			add(g.GatedContractSet()...)
		}
		return sorted(set), nil
	}
	switch source {
	case soroswap.SourceName:
		dec := soroswap.NewDecoder()
		add(soroswap.MainnetFactories...)
		if err := walkFactory(ctx, d.Events, source, dec, soroswap.MainnetFactories, soroswap.PrefixFactory, soroswapGenesis, tip); err != nil {
			return nil, err
		}
		add(dec.GatedContractSet()...)
	case cctp.SourceName:
		add(cctp.MainnetContracts()...)
	case rozo.SourceName:
		add(rozo.MainnetPaymentContracts...)
	case sorocredit.SourceName:
		add(sorocredit.MainnetContract)
		children, err := sorocreditEmittingChildren(ctx, d.Events, tip)
		if err != nil {
			return nil, err
		}
		add(children...)
	case blend_backstop.SourceName:
		add(blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1)
	case soroswap_router.SourceName:
		add(soroswap_router.MainnetRouter)
	case reflector.SourceDEX:
		add(d.Oracle.Reflector.DEXContract)
	case reflector.SourceCEX:
		add(d.Oracle.Reflector.CEXContract)
	case reflector.SourceFX:
		add(d.Oracle.Reflector.FXContract)
	case redstone.SourceName:
		add(d.Oracle.Redstone.AdapterContract)
	case band.SourceName:
		add(d.Oracle.Band.StandardReferenceContract)
	default:
		return nil, fmt.Errorf("no contract resolver for %q", source)
	}
	return sorted(set), nil
}

// walkFactory streams the factories' creation events through dec so it
// registers every child announced by tip. A blind or empty walk refuses:
// a hidden child is exactly the contract this gate exists to see.
func walkFactory(ctx context.Context, es completeness.EventStreamer, source string, dec completeness.Decoder, factories []string, sym string, genesis, tip uint32) error {
	if len(factories) == 0 || sym == "" || genesis >= tip {
		return nil
	}
	if es == nil {
		return fmt.Errorf("%s factory walk: no event stream", source)
	}
	seen := 0
	blind := completeness.NewBlindTracker()
	err := es.StreamContractEvents(ctx, genesis, tip, factories, []string{sym}, func(ev events.Event) error {
		if perr := completeness.Guard(func() {
			if dec.Matches(ev) {
				if _, derr := dec.Decode(ev); derr == nil {
					seen++
				}
			}
		}); perr != nil {
			blind.Undecodable(ev.Ledger)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s factory walk: %w", source, err)
	}
	if b := blind.Result(); b.Any() {
		return fmt.Errorf("%s factory walk is blind: %s", source, b.Detail())
	}
	if seen == 0 {
		return fmt.Errorf("%s factory walk: 0 %q creation events in [%d,%d]", source, sym, genesis, tip)
	}
	return nil
}

// sorocreditEmittingChildren returns every Collateral child the sorocredit
// decoder admits an event from by tip. Its childgate honours any child the
// root announced, so a child that emits runs WASM the decoder reads; gating
// the ~139k silent children instead would cost one lake read each for no
// coverage. One ledger-ordered pass over every emitter registers each child
// before its own events, as a replay does.
func sorocreditEmittingChildren(ctx context.Context, es completeness.EventStreamer, tip uint32) ([]string, error) {
	if sorocredit.GenesisLedger >= tip {
		return nil, nil
	}
	if es == nil {
		return nil, fmt.Errorf("%s child walk: no event stream", sorocredit.SourceName)
	}
	dec := sorocredit.NewDecoder()
	emitters := map[string]struct{}{}
	created := 0
	blind := completeness.NewBlindTracker()
	err := es.StreamContractEvents(ctx, sorocredit.GenesisLedger, tip, nil, sorocredit.EventSymbols(), func(ev events.Event) error {
		if perr := completeness.Guard(func() {
			if !dec.Matches(ev) {
				return
			}
			if ev.ContractID != sorocredit.MainnetContract {
				emitters[ev.ContractID] = struct{}{}
			}
			outs, derr := dec.Decode(ev)
			if derr != nil {
				blind.Undecodable(ev.Ledger)
				return
			}
			for _, o := range outs {
				if e, ok := o.(sorocredit.Event); ok && e.EventType == sorocredit.TypeNewCollateralContract {
					created++
				}
			}
		}); perr != nil {
			blind.Undecodable(ev.Ledger)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s child walk: %w", sorocredit.SourceName, err)
	}
	if b := blind.Result(); b.Any() {
		return nil, fmt.Errorf("%s child walk is blind: %s", sorocredit.SourceName, b.Detail())
	}
	if created == 0 {
		return nil, fmt.Errorf("%s child walk: 0 %q events in [%d,%d]", sorocredit.SourceName, sorocredit.TopicNewCollateralContract, sorocredit.GenesisLedger, tip)
	}
	return sorted(emitters), nil
}

func sorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
