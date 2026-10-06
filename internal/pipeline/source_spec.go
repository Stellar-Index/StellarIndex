// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// BuildArgs carries the operator config a source's decoder is built from.
type BuildArgs struct {
	Oracle       config.OracleConfig
	WatchedSEP41 []string
	Gated        map[string][]contractid.Option
	SoroswapOpts []soroswap.DecoderOption
}

// SourceSpec is the one entry that wires an on-chain source into both
// writers (AGENTS.md invariant 7). BuildDispatcher, SorobanSourceNames,
// IsProjectedEvent, IsSoleWriterProjected, RegisterSupplyEventDecoders and
// projector.BuildRegistry all read it, so a source cannot be projected in
// one list and missing from another.
type SourceSpec struct {
	Name string
	// Events holds a zero value of every consumer.Event type the source
	// emits; it is what IsProjectedEvent and IsSoleWriterProjected match on.
	Events []consumer.Event
	// NewDecoder builds the event decoder. A nil decoder with a nil error
	// means there is nothing to run (sep41 with no watched contracts).
	NewDecoder func(BuildArgs) (dispatcher.Decoder, error)
	// Dispatch replaces the default dispatcher wiring (AddDecoder of
	// NewDecoder) for sources with an op or contract-call decoder.
	Dispatch func(*dispatcher.Dispatcher, BuildArgs) error
	// OnDispatch runs after BuildDispatcher registers the source.
	OnDispatch func()
	// Watched sources are built from supply.watched_sep41_contracts, never
	// from ingestion.enabled_sources: RegisterSupplyEventDecoders adds them
	// to the dispatcher and BuildRegistry projects them unconditionally.
	Watched bool
	// Classic sources decode classic operations, not soroban_events.
	Classic bool
	// Projector is non-nil exactly when the projector writes the source.
	Projector *ProjectorSpec
}

// ProjectorSpec is what projector.BuildRegistry needs beyond the decoder.
type ProjectorSpec struct {
	// NewDecoder overrides SourceSpec.NewDecoder for the projector's copy.
	NewDecoder        func(BuildArgs) (dispatcher.Decoder, error)
	Topic0Syms        []string
	ExcludeTopic0Syms []string
	// ContractIDs is a static contract-id prefilter.
	ContractIDs func(BuildArgs) []string
	// LiveContractIDs derives a prefilter re-read every cycle from the
	// built decoder, for gate sets that grow in-stream.
	LiveContractIDs     func(dispatcher.Decoder) func() []string
	NeedsStateWriteKeys bool
	Genesis             uint32
	// SoleWriter: the projector owns the write even in Phase-3 parallel
	// mode, so the dispatcher's events goroutine skips it. Set only once
	// the source's full-history re-derive has landed and it is in the
	// ADR-0033 reconcile catalogue (internal/ops/chops/reconciliation_catalogue.go).
	// VerifySoleWriterCAGGCoverage then gates the aggregates over its tables.
	SoleWriter bool
}

// firehoseExcludeSyms is the CAP-67 classic-token topic[0] set minus
// set_admin (blend dispatches on it). Every source using it was audited to
// consume none of these six, so excluding them from a catch-up window's
// lake scan is lossless. An exclude-list because several decoders match
// prefixed topic[0] symbols (phoenix "XYK Pool: …") an include-list would miss.
var firehoseExcludeSyms = []string{
	"transfer", "mint", "burn", "clawback", "approve", "set_authorized",
}

var sep41TransferSyms = []string{
	sep41_transfers.SymbolTransfer,
	sep41_transfers.SymbolApprove,
	sep41_transfers.SymbolSetAdmin,
	sep41_transfers.SymbolSetAuthorized,
}

// sep41SupplySyms keeps the sep41_supply catch-up window from streaming
// the whole CAP-67 firehose to find mint/burn/clawback rows.
var sep41SupplySyms = []string{
	sep41_supply.SymbolMint,
	sep41_supply.SymbolBurn,
	sep41_supply.SymbolClawback,
}

type gatedSetDecoder interface{ GatedContractSet() []string }

// liveGatedSet scopes a catch-up scan by the decoder's own identity gate.
// Used where the source's own symbols (mint, burn, transfer) overlap the
// firehose, so a topic exclusion would drop its events.
func liveGatedSet(d dispatcher.Decoder) func() []string {
	return d.(gatedSetDecoder).GatedContractSet
}

func decoderOf[D dispatcher.Decoder](mk func() D) func(BuildArgs) (dispatcher.Decoder, error) {
	return func(BuildArgs) (dispatcher.Decoder, error) { return mk(), nil }
}

func gatedDecoder[D dispatcher.Decoder](name string, mk func(...contractid.Option) D) func(BuildArgs) (dispatcher.Decoder, error) {
	return func(a BuildArgs) (dispatcher.Decoder, error) { return mk(a.Gated[name]...), nil }
}

func watchedDecoder[D dispatcher.Decoder](mk func([]string) (D, error)) func(BuildArgs) (dispatcher.Decoder, error) {
	return func(a BuildArgs) (dispatcher.Decoder, error) {
		if len(a.WatchedSEP41) == 0 {
			return nil, nil
		}
		dec, err := mk(a.WatchedSEP41)
		if err != nil {
			return nil, err
		}
		return dec, nil
	}
}

func missingOracle(name, key string) error {
	return fmt.Errorf("source %q enabled but oracle.%s is empty", name, key)
}

func reflectorSpec(name string, v reflector.Variant, key string, cfg func(config.OracleConfig) (string, uint8)) SourceSpec {
	contract := func(a BuildArgs) []string { c, _ := cfg(a.Oracle); return []string{c} }
	return SourceSpec{
		Name:   name,
		Events: []consumer.Event{reflector.UpdateEvent{}},
		NewDecoder: func(a BuildArgs) (dispatcher.Decoder, error) {
			c, decimals := cfg(a.Oracle)
			if c == "" {
				return nil, missingOracle(name, key)
			}
			return reflector.NewDecoder(v, c, reflector.WithDecoderDecimals(decimals)), nil
		},
		OnDispatch: func() { obs.DeclareOracleResolution(name, reflector.DefaultResolutionSeconds) },
		Projector:  &ProjectorSpec{ContractIDs: contract},
	}
}

func excludeFirehose() *ProjectorSpec { return &ProjectorSpec{ExcludeTopic0Syms: firehoseExcludeSyms} }

var specs = []SourceSpec{
	{
		Name:       soroswap.SourceName,
		Events:     []consumer.Event{soroswap.TradeEvent{}, soroswap.SkimEvent{}, soroswap.LiquidityEvent{}},
		NewDecoder: func(a BuildArgs) (dispatcher.Decoder, error) { return soroswap.NewDecoder(a.SoroswapOpts...), nil },
		Projector:  excludeFirehose(),
	},
	{
		Name: aquarius.SourceName,
		Events: []consumer.Event{
			aquarius.TradeEvent{},
			aquarius.ReservesEvent{},
			aquarius.LiquidityEvent{},
			aquarius.RewardsEvent{},
			aquarius.AdminEvent{},
			aquarius.FeeEvent{},
			aquarius.KillEvent{},
		},
		NewDecoder: gatedDecoder(aquarius.SourceName, aquarius.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		Name: phoenix.SourceName,
		Events: []consumer.Event{
			phoenix.TradeEvent{},
			phoenix.LiquidityEvent{},
			phoenix.StakeEvent{},
			phoenix.InitializeEvent{},
			phoenix.AdminEvent{},
		},
		NewDecoder: gatedDecoder(phoenix.SourceName, phoenix.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		Name:       comet.SourceName,
		Events:     []consumer.Event{comet.TradeEvent{}, comet.LiquidityEvent{}},
		NewDecoder: gatedDecoder(comet.SourceName, comet.NewDecoder),
		Projector: &ProjectorSpec{
			// The dispatcher always runs its own comet decoder too (Phase-3
			// double-write); without this both would bump the exploit counters.
			NewDecoder: func(a BuildArgs) (dispatcher.Decoder, error) {
				return comet.NewDecoder(a.Gated[comet.SourceName]...).WithoutMetrics(), nil
			},
			ExcludeTopic0Syms: firehoseExcludeSyms,
		},
	},
	{
		Name:       sushiswap_v3.SourceName,
		Events:     []consumer.Event{sushiswap_v3.TradeEvent{}, sushiswap_v3.PositionEvent{}},
		NewDecoder: gatedDecoder(sushiswap_v3.SourceName, sushiswap_v3.NewDecoder),
		Projector:  &ProjectorSpec{LiveContractIDs: liveGatedSet, Genesis: sushiswap_v3.FactoryGenesisLedger},
	},
	{
		Name:       upshift.SourceName,
		Events:     []consumer.Event{upshift.Event{}},
		NewDecoder: gatedDecoder(upshift.SourceName, upshift.NewDecoder),
		Projector:  &ProjectorSpec{LiveContractIDs: liveGatedSet, Genesis: upshift.GenesisLedger},
	},
	{
		// PT/YT transfer rows are kept, so no topic exclusion; the gate set
		// grows in-stream as pt_deployed / yt_deployed admit contracts.
		Name:       spectra.SourceName,
		Events:     []consumer.Event{spectra.Event{}},
		NewDecoder: gatedDecoder(spectra.SourceName, spectra.NewDecoder),
		Projector:  &ProjectorSpec{LiveContractIDs: liveGatedSet, Genesis: spectra.GenesisLedger},
	},
	reflectorSpec(reflector.SourceDEX, reflector.VariantDEX, "reflector.dex_contract",
		func(o config.OracleConfig) (string, uint8) { return o.Reflector.DEXContract, o.Reflector.DEXDecimals }),
	reflectorSpec(reflector.SourceCEX, reflector.VariantCEX, "reflector.cex_contract",
		func(o config.OracleConfig) (string, uint8) { return o.Reflector.CEXContract, o.Reflector.CEXDecimals }),
	reflectorSpec(reflector.SourceFX, reflector.VariantFX, "reflector.fx_contract",
		func(o config.OracleConfig) (string, uint8) { return o.Reflector.FXContract, o.Reflector.FXDecimals }),
	{
		Name:   redstone.SourceName,
		Events: []consumer.Event{redstone.UpdateEvent{}},
		NewDecoder: func(a BuildArgs) (dispatcher.Decoder, error) {
			if a.Oracle.Redstone.AdapterContract == "" {
				return nil, missingOracle(redstone.SourceName, "redstone.adapter_contract")
			}
			return redstone.NewDecoder(a.Oracle.Redstone.AdapterContract), nil
		},
		OnDispatch: func() { obs.DeclareOracleHeartbeat(redstone.SourceName, redstone.DefaultResolutionSeconds) },
		Projector: &ProjectorSpec{
			ContractIDs: func(a BuildArgs) []string { return []string{a.Oracle.Redstone.AdapterContract} },
			// write_prices stores each accepted feed under a per-feed key; the
			// decoder attributes freshness-filtered batches from those keys.
			NeedsStateWriteKeys: true,
		},
	},
	{
		// Band's contract emits no events; the relay() call is decoded instead.
		Name:   band.SourceName,
		Events: []consumer.Event{band.UpdateEvent{}},
		Dispatch: func(d *dispatcher.Dispatcher, a BuildArgs) error {
			if a.Oracle.Band.StandardReferenceContract == "" {
				return missingOracle(band.SourceName, "band.standard_reference_contract")
			}
			d.AddContractCallDecoder(band.NewDecoder(a.Oracle.Band.StandardReferenceContract))
			return nil
		},
		OnDispatch: func() { obs.DeclareOracleResolution(band.SourceName, band.DefaultResolutionSeconds) },
	},
	{
		// The router emits no events (the pairs do); its swap_* calls are decoded.
		Name:   soroswap_router.SourceName,
		Events: []consumer.Event{soroswap_router.Event{}},
		Dispatch: func(d *dispatcher.Dispatcher, _ BuildArgs) error {
			d.AddContractCallDecoder(soroswap_router.NewDecoder(soroswap_router.MainnetRouter))
			return nil
		},
	},
	{
		Name:       defindex.SourceName,
		Events:     []consumer.Event{defindex.Event{}, defindex.VaultEvent{}, defindex.DFeesEvent{}, defindex.AdminEvent{}},
		NewDecoder: gatedDecoder(defindex.SourceName, defindex.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		// Pool events are gated on factory-deployed pools warmed from
		// protocol_contracts; an unwarmed caller matches only factory deploys.
		Name: blend.SourceName,
		Events: []consumer.Event{
			blend.NewAuctionEvent{},
			blend.FillAuctionEvent{},
			blend.DeleteAuctionEvent{},
			blend.PositionEvent{},
			blend.EmissionEvent{},
			blend.AdminEvent{},
		},
		NewDecoder: gatedDecoder(blend.SourceName, blend.NewDecoder),
		Projector:  &ProjectorSpec{ExcludeTopic0Syms: firehoseExcludeSyms, Genesis: blend.FactoryGenesisLedger},
	},
	{
		Name:       blend_backstop.SourceName,
		Events:     []consumer.Event{blend_backstop.Event{}},
		NewDecoder: decoderOf(blend_backstop.NewDecoder),
		Projector:  &ProjectorSpec{ExcludeTopic0Syms: firehoseExcludeSyms, Genesis: blend_backstop.BackstopGenesisLedger},
	},
	{
		Name:       blend_emitter.SourceName,
		Events:     []consumer.Event{blend_emitter.DistributeEvent{}, blend_emitter.DropEvent{}, blend_emitter.SwapConfigEvent{}},
		NewDecoder: gatedDecoder(blend_emitter.SourceName, blend_emitter.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		Name:       cctp.SourceName,
		Events:     []consumer.Event{cctp.Event{}},
		NewDecoder: decoderOf(cctp.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		Name:       rozo.SourceName,
		Events:     []consumer.Event{rozo.Event{}},
		NewDecoder: decoderOf(rozo.NewDecoder),
		Projector:  excludeFirehose(),
	},
	{
		// Its topic[0] symbols are not in the firehose, so an include
		// prefilter is exact; the identity gate rejects look-alike emitters.
		Name:       sorocredit.SourceName,
		Events:     []consumer.Event{sorocredit.Event{}},
		NewDecoder: func(BuildArgs) (dispatcher.Decoder, error) { return sorocredit.NewDecoder(), nil },
		Projector:  &ProjectorSpec{Topic0Syms: sorocredit.EventSymbols(), Genesis: sorocredit.GenesisLedger},
	},
	{
		Name:    sdex.SourceName,
		Events:  []consumer.Event{sdex.TradeEvent{}},
		Classic: true,
		Dispatch: func(d *dispatcher.Dispatcher, _ BuildArgs) error {
			d.AddOpDecoder(sdex.NewDecoder())
			return nil
		},
	},
	// The sep41 pair decodes only the watched contracts, the same set for
	// both writers, so the projector reproduces the dispatcher's rows.
	{
		Name:       sep41_supply.SourceName,
		Events:     []consumer.Event{sep41_supply.Event{}},
		NewDecoder: watchedDecoder(sep41_supply.NewDecoder),
		Watched:    true,
		Projector:  &ProjectorSpec{Topic0Syms: sep41SupplySyms, SoleWriter: true},
	},
	{
		Name:       sep41_transfers.SourceName,
		Events:     []consumer.Event{sep41_transfers.Event{}},
		NewDecoder: watchedDecoder(sep41_transfers.NewDecoder),
		Watched:    true,
		Projector:  &ProjectorSpec{Topic0Syms: sep41TransferSyms, SoleWriter: true},
	},
}

// Specs returns every source spec. Callers must not modify it.
func Specs() []SourceSpec { return specs }

// SpecByName returns the spec named name, matched case- and space-insensitively.
func SpecByName(name string) (*SourceSpec, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range specs {
		if specs[i].Name == name {
			return &specs[i], true
		}
	}
	return nil, false
}

// SorobanSourceNames lists every name BuildDispatcher accepts whose events
// land in soroban_events (ADR-0029). internal/ops/ingest's resume-stalled
// gate picks soroban_events vs trades[source='sdex'] gap scans from it.
var SorobanSourceNames = sorobanSourceNames(specs)

func sorobanSourceNames(ss []SourceSpec) []string {
	var out []string
	for i := range ss {
		if !ss[i].Watched && !ss[i].Classic {
			out = append(out, ss[i].Name)
		}
	}
	return out
}

func (s *SourceSpec) addToDispatcher(d *dispatcher.Dispatcher, a BuildArgs) error {
	if s.Dispatch != nil {
		if err := s.Dispatch(d, a); err != nil {
			return err
		}
	} else {
		dec, err := s.NewDecoder(a)
		if err != nil {
			return err
		}
		d.AddDecoder(dec)
	}
	if s.OnDispatch != nil {
		s.OnDispatch()
	}
	return nil
}

type eventRole struct{ projected, soleWriter bool }

var eventRoles = indexEventRoles(specs)

// indexEventRoles maps each event type to its writer role. A type shared
// by several specs (reflector.UpdateEvent) must get the same role from
// each; a conflict is a wiring bug, so it panics at init.
func indexEventRoles(ss []SourceSpec) map[reflect.Type]eventRole {
	out := map[reflect.Type]eventRole{}
	for i := range ss {
		role := eventRole{}
		if p := ss[i].Projector; p != nil {
			role = eventRole{projected: true, soleWriter: p.SoleWriter}
		}
		for _, ev := range ss[i].Events {
			t := reflect.TypeOf(ev)
			if prev, seen := out[t]; seen && prev != role {
				panic(fmt.Sprintf("pipeline: %s gets conflicting writer roles from source specs (second: %s)", t, ss[i].Name))
			}
			out[t] = role
		}
	}
	return out
}
