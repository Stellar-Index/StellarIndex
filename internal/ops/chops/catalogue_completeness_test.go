// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// ─── Catalogue-completeness invariant (the "next omission" guard) ─────
//
// The projection reconcile axis (ADR-0033 Claim 2b) only checks the
// protocol tables listed as reconTargets in the catalogue. A source's
// decoder emits consumer.Event values that the sink (pipeline.handleEvent)
// routes to served tables; if a table is not a reconTarget it is covered
// ONLY by the coarse density gap-detector, never per-ledger count-verified.
// That is exactly how soroswap.liquidity, the aquarius reserves/rewards/
// admin/fee/kill family, and defindex.strategy.harvest each shipped a
// persisted-but-unreconciled table (or, for harvest, an unreconciled KIND).
//
// This file makes that class impossible to reintroduce SILENTLY. It
// enumerates EVERY consumer.Event type the sink persists (statically, from
// pipeline.handleEvent's real type switch — the CI-guaranteed-complete
// universe, per pipeline/lockstep_ast_test.go) and asserts each one is
// EITHER covered by a reconTarget in the built catalogue OR carries an
// explicit noReconcile waiver with a written reason. A new decoder event
// type that routes to a persisted table without being reconciled or waived
// fails this test, not production.
//
// Residual (documented, not hidden): several decoders emit their
// EventKind() DYNAMICALLY from wire data (defindex by Direction, sorocredit
// by EventType, aquarius reserves/liquidity by a coarse kind shared across
// tables). No decoder exports a static kind vocabulary, so a purely-static
// enumeration of every KIND is infeasible. The guard is therefore
// TYPE-level for the omission class (which catches every persisted-but-
// unreconciled TABLE), plus a decoder-truth cross-check
// (TestCatalogue_DeclaredKindsMatchDecoderOutput) that INSTANTIATES the
// concrete emitters — including defindex per-Direction, so the
// defindex.strategy.harvest KIND-omission regression is caught explicitly.

type routeDisp int

const (
	// reconciledByKind: the sink routes this event type to `table`, and the
	// projection axis re-derives it by EventKind() — so `table` MUST be a
	// reconTarget with a non-empty kinds list (and, when `kind` is spelled
	// out, that kind MUST be one of them).
	reconciledByKind routeDisp = iota
	// reconciledByCensus: reconciled, but not via soroban_events EventKind
	// re-derive — the SDEX LCM census (src.census) or the event-less
	// ContractCall census (src.callDec). `table` MUST be a target of such a
	// source (kinds may legitimately be nil there).
	reconciledByCensus
	// noReconcile: deliberately NOT on the projection axis. `reason` is
	// mandatory and load-bearing — it is the declared waiver the invariant
	// demands instead of a silent gap.
	noReconcile
)

// projRoute declares how ONE (consumer.Event type → persisted table) edge
// is covered by the projection reconcile axis. A single event type may have
// several routes (sorocredit.Event fans to four credit_* tables by
// EventType; aquarius.ReservesEvent lands in two tables by its Kind field).
type projRoute struct {
	// typeName is "pkg.Type" EXACTLY as pipeline.handleEvent's type switch
	// spells it (the import ident, which is the dir basename for every
	// source package). It is matched against the AST of that switch, so a
	// renamed/removed type breaks the test rather than rotting the list.
	typeName string
	// table is the served table this edge writes (documentary for
	// noReconcile; asserted present for the reconciled dispositions).
	table string
	// kind, when non-empty, is the EventKind() this edge reconciles; the
	// catalogue's reconTarget for `table` must list it. Left "" for
	// reconciledByKind routes whose kind is verified elsewhere
	// (TestCatalogue_DeclaredKindsMatchDecoderOutput) or is a stable
	// pre-existing entry — the table-presence check still applies.
	kind string
	disp routeDisp
	// reason is REQUIRED for noReconcile: the declared {source/table, why}
	// waiver.
	reason string
}

// fanoutWaiver is the shared reason for the aquarius reserve/liquidity
// family: the sink fans ONE decoder event out to N per-token-position rows
// (token_index is a PK component), so the projection axis's
// event-count-vs-served-row-count reconcile would false-flag nearly every
// ledger (lake-measured 2026-08-17: aquarius_reserves 843705 rows /
// 421793 events, aquarius_liquidity 12043/6021 — ratio ~2.0). Covered by
// the density gap-detector until a fan-out-aware (per-event-identity)
// reconcile lands.
const fanoutWaiver = "fan-out: one decoder event → N per-token-position rows " +
	"(token_index PK component), so event-count vs served-row-count would false-flag; " +
	"density gap-detector covers it pending a fan-out-aware reconcile"

// blendEmitterDropWaiver covers the blend_emitter `drop` kind: one decoder
// DropEvent carries N recipients and the sink writes one blend_emitter_events
// row per recipient (recipient_index is a PK component), so an
// event-count-vs-served-row-count reconcile false-flags the drop ledgers
// (r1 2026-08-18: ledger 51,499,914 = 13 rows / 1 event identity; 57,467,292 =
// 3 / 1; Σ|Δ|=14, data correct). Unlike the aquarius all-fan-out tables the
// whole table is NOT waived — the reconTarget carves the drop rows out of the
// served side (whereFilter `event_kind <> 'drop'`) and omits the drop kind, so
// the 1:1 distribute/swap_config rows still reconcile per-ledger; the density
// gap-detector covers drop.
const blendEmitterDropWaiver = "fan-out: one drop event → N recipient rows " +
	"(recipient_index PK component), so event-count vs served-row-count false-flags " +
	"the drop ledgers; the blend_emitter_events reconTarget excludes drop rows " +
	"(whereFilter event_kind <> 'drop') and omits the drop kind so the 1:1 " +
	"distribute/swap_config rows still reconcile per-ledger; density gap-detector covers drop"

// observationWaiver covers the five supply observers: LedgerEntry
// observations, not soroban-event projections — they never flow through the
// EventKind re-derive and are covered by their own observer coverage axis.
const observationWaiver = "LedgerEntry observation (supply observer), not a soroban-event " +
	"projection — outside the EventKind re-derive; covered by the observer's own coverage axis"

// externalWaiver covers the external CEX/FX source: off-chain vendor data
// with no on-chain lake to re-derive against.
const externalWaiver = "off-chain external (CEX/FX) vendor data — no on-chain lake event to re-derive against"

// projRoutes is the declared coverage for every consumer.Event type the
// sink persists. TestCatalogue_EveryPersistedEventTypeIsRoutedOrWaived
// proves it is exhaustive against pipeline.handleEvent — so this is not a
// hand-maintained list that can silently fall behind: adding a persist arm
// without a row here fails CI.
var projRoutes = []projRoute{
	// ── soroswap ──
	{typeName: "soroswap.TradeEvent", table: "trades", kind: "soroswap.trade", disp: reconciledByKind},
	{typeName: "soroswap.SkimEvent", table: "soroswap_skim_events", kind: "soroswap.skim", disp: reconciledByKind},
	{typeName: "soroswap.LiquidityEvent", table: "soroswap_liquidity", kind: "soroswap.liquidity", disp: reconciledByKind},

	// ── aquarius ──
	{typeName: "aquarius.TradeEvent", table: "trades", kind: "aquarius.trade", disp: reconciledByKind},
	{typeName: "aquarius.RewardsEvent", table: "aquarius_rewards_events", kind: "aquarius.rewards", disp: reconciledByKind},
	{typeName: "aquarius.AdminEvent", table: "aquarius_admin", kind: "aquarius.admin", disp: reconciledByKind},
	{typeName: "aquarius.FeeEvent", table: "aquarius_protocol_fee", kind: "aquarius.fee", disp: reconciledByKind},
	{typeName: "aquarius.KillEvent", table: "aquarius_kill_switches", kind: "aquarius.kill", disp: reconciledByKind},
	// aquarius.ReservesEvent routes to TWO tables by its runtime Kind field,
	// both fan-out AND both sharing the single coarse "aquarius.reserves"
	// EventKind() — un-attributable and un-countable by the current axis.
	{typeName: "aquarius.ReservesEvent", table: "aquarius_reserves", disp: noReconcile, reason: fanoutWaiver + "; also shares the coarse aquarius.reserves EventKind with aquarius_reserves_sync (sink routes on ReservesEvent.Kind, invisible to the by-EventKind expected side)"},
	{typeName: "aquarius.ReservesEvent", table: "aquarius_reserves_sync", disp: noReconcile, reason: fanoutWaiver + "; also shares the coarse aquarius.reserves EventKind with aquarius_reserves"},
	{typeName: "aquarius.LiquidityEvent", table: "aquarius_liquidity", disp: noReconcile, reason: fanoutWaiver},

	// ── phoenix ──
	{typeName: "phoenix.TradeEvent", table: "trades", kind: "phoenix.trade", disp: reconciledByKind},
	{typeName: "phoenix.LiquidityEvent", table: "phoenix_liquidity", kind: "phoenix.liquidity", disp: reconciledByKind},
	{typeName: "phoenix.StakeEvent", table: "phoenix_stake_events", kind: "phoenix.stake", disp: reconciledByKind},
	{typeName: "phoenix.InitializeEvent", table: "phoenix_initialize", kind: "phoenix.initialize", disp: reconciledByKind},
	{typeName: "phoenix.AdminEvent", table: "phoenix_admin_events", kind: "phoenix.admin", disp: reconciledByKind},

	// ── comet ──
	{typeName: "comet.TradeEvent", table: "trades", kind: "comet.trade", disp: reconciledByKind},
	{typeName: "comet.LiquidityEvent", table: "comet_liquidity", kind: "comet.liquidity", disp: reconciledByKind},

	// ── blend (five kinds across four tables) ──
	{typeName: "blend.NewAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.FillAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.DeleteAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.PositionEvent", table: "blend_positions", disp: reconciledByKind},
	{typeName: "blend.EmissionEvent", table: "blend_emissions", disp: reconciledByKind},
	{typeName: "blend.AdminEvent", table: "blend_admin", disp: reconciledByKind},
	{typeName: "blend_backstop.Event", table: "blend_backstop_events", disp: reconciledByKind},
	// blend_emitter: distribute + swap_config are 1:1 (one event → one row) and
	// reconcile per-ledger; drop FANS OUT (one event → N recipient rows) and is
	// waived — the reconTarget carves drop rows out of the served side
	// (whereFilter `event_kind <> 'drop'`) and omits the drop kind.
	{typeName: "blend_emitter.DistributeEvent", table: "blend_emitter_events", kind: "blend_emitter.distribute", disp: reconciledByKind},
	{typeName: "blend_emitter.DropEvent", table: "blend_emitter_events", disp: noReconcile, reason: blendEmitterDropWaiver},
	{typeName: "blend_emitter.SwapConfigEvent", table: "blend_emitter_events", kind: "blend_emitter.swap_config", disp: reconciledByKind},

	// ── cctp / rozo / sorocredit ──
	{typeName: "cctp.Event", table: "cctp_events", disp: reconciledByKind},
	{typeName: "rozo.Event", table: "rozo_events", disp: reconciledByKind},
	{typeName: "sorocredit.Event", table: "credit_positions", disp: reconciledByKind},
	{typeName: "sorocredit.Event", table: "credit_statements", disp: reconciledByKind},
	{typeName: "sorocredit.Event", table: "credit_settlements", disp: reconciledByKind},
	{typeName: "sorocredit.Event", table: "credit_events", disp: reconciledByKind},

	// ── defindex (both flow layers land in defindex_flows; dfees
	// fans out per distributed_fees entry into defindex_fees — 1:1
	// because the DECODER emits one event per entry, W5.2) ──
	{typeName: "defindex.Event", table: "defindex_flows", disp: reconciledByKind},
	{typeName: "defindex.VaultEvent", table: "defindex_flows", disp: reconciledByKind},
	{typeName: "defindex.DFeesEvent", table: "defindex_fees", kind: "defindex.vault.dfees", disp: reconciledByKind},

	// ── oracles ──
	{typeName: "reflector.UpdateEvent", table: "oracle_updates", kind: "reflector.update", disp: reconciledByKind},
	{typeName: "redstone.UpdateEvent", table: "oracle_updates", kind: "redstone.update", disp: reconciledByKind},
	{typeName: "band.UpdateEvent", table: "oracle_updates", disp: reconciledByCensus},

	// ── census / ContractCall census ──
	{typeName: "sdex.TradeEvent", table: "trades", disp: reconciledByCensus},
	{typeName: "soroswap_router.Event", table: "soroswap_router_swaps", disp: reconciledByCensus},

	// ── sep41 (config-gated; promoted into the catalogue when watched) ──
	{typeName: "sep41_supply.Event", table: "sep41_supply_events", disp: reconciledByKind},
	{typeName: "sep41_transfers.Event", table: "sep41_transfers", disp: reconciledByKind},

	// ── deliberately off the projection axis ──
	{typeName: "external.TradeEvent", table: "trades", disp: noReconcile, reason: externalWaiver},
	{typeName: "external.UpdateEvent", table: "oracle_updates", disp: noReconcile, reason: externalWaiver},
	{typeName: "accounts.Observation", table: "account_observations", disp: noReconcile, reason: observationWaiver},
	{typeName: "trustlines.Observation", table: "trustline_observations", disp: noReconcile, reason: observationWaiver},
	{typeName: "claimable_balances.Observation", table: "claimable_observations", disp: noReconcile, reason: observationWaiver},
	{typeName: "liquidity_pools.Observation", table: "lp_reserve_observations", disp: noReconcile, reason: observationWaiver},
	{typeName: "sac_balances.Observation", table: "sac_balance_observations", disp: noReconcile, reason: observationWaiver},
}

// sinkHandleEventTypes parses pipeline.handleEvent and returns the set of
// "pkg.Type" names its dispatch type-switch persists — the authoritative,
// CI-complete (TestLockstep_EveryConsumerEventHasSinkArm) universe of
// persisted event types. Duplicated minimally from pipeline's own AST guard
// because that guard's helpers live in an unimportable _test.go.
func sinkHandleEventTypes(t *testing.T) map[string]bool {
	t.Helper()
	const sinkPath = "../../pipeline/sink.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, sinkPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", sinkPath, err)
	}
	var handle *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleEvent" && fd.Recv == nil {
			handle = fd
			break
		}
	}
	if handle == nil {
		t.Fatalf("handleEvent not found in %s", sinkPath)
	}
	out := map[string]bool{}
	ast.Inspect(handle, func(n ast.Node) bool {
		sw, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				if sel, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						out[pkg.Name+"."+sel.Sel.Name] = true
					}
				}
			}
		}
		return false // first (dispatch) type-switch only
	})
	if len(out) == 0 {
		t.Fatalf("no type-switch cases found in handleEvent — AST walker broken?")
	}
	return out
}

// builtCatalogue builds the catalogue with every config-gated source
// enabled (oracles + band) AND a watched SEP-41 set, so the invariant sees
// the FULL reconTarget surface the routes reference.
func builtCatalogue(t *testing.T) []reconSource {
	t.Helper()
	cfg := testConfigWithAllSources()
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41
	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	return cat
}

// reconTargetsByTable indexes the catalogue as table → the reconTargets that
// write it, plus whether the owning source is a census/ContractCall source.
type tableCoverage struct {
	kinds     []string
	census    bool // src.census || src.callDec != nil
	seenTable bool
}

func catalogueCoverage(cat []reconSource) map[string]*tableCoverage {
	out := map[string]*tableCoverage{}
	for _, src := range cat {
		isCensus := src.census || src.callDec != nil
		for _, tgt := range src.targets {
			tc := out[tgt.table]
			if tc == nil {
				tc = &tableCoverage{}
				out[tgt.table] = tc
			}
			tc.seenTable = true
			tc.kinds = append(tc.kinds, tgt.kinds...)
			if isCensus {
				tc.census = true
			}
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func sortedTypeSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCatalogue_EveryPersistedEventTypeIsRoutedOrWaived is THE durable
// omission guard. Every consumer.Event type the sink persists must appear
// in projRoutes (routed to a reconTarget or explicitly waived), and every
// projRoutes entry must correspond to a real persist arm (no stale rows).
//
// This is the mechanism that would have failed CI the day soroswap.liquidity
// / the aquarius reserves/rewards/admin/fee/kill types / defindex's vault
// event first gained a persist arm without a matching reconcile decision —
// forcing the author to add a reconTarget or a written waiver, never a
// silent density-only table.
func TestCatalogue_EveryPersistedEventTypeIsRoutedOrWaived(t *testing.T) {
	persisted := sinkHandleEventTypes(t)

	routed := map[string]bool{}
	for _, r := range projRoutes {
		routed[r.typeName] = true
	}

	// Forward: every persisted type must be declared.
	for _, typ := range sortedTypeSet(persisted) {
		if !routed[typ] {
			t.Errorf("%s has a pipeline.handleEvent persist arm but no projRoute — it may write a served table that the projection reconcile never count-verifies (the soroswap.liquidity / aquarius-blind-spot class). Add a reconTarget for its table (reconciledByKind) or a noReconcile waiver with a reason.", typ)
		}
	}
	// Reverse: no stale projRoute referencing a type the sink no longer
	// persists (a rename would otherwise leave the guard asserting nothing).
	for typ := range routed {
		if !persisted[typ] {
			t.Errorf("projRoute references %s but pipeline.handleEvent has no persist arm for it — stale entry (renamed/removed type). Update projRoutes.", typ)
		}
	}
}

// TestCatalogue_ReconciledRoutesArePresent asserts every reconciled route is
// actually covered by the built catalogue: a reconciledByKind table exists
// as a reconTarget with a NON-EMPTY kinds list (and, when the route spells
// its kind, that kind is present); a reconciledByCensus table exists on a
// census/ContractCall source. This is the non-vacuity anchor — deleting a
// reconTarget (or emptying its kinds) fails here.
func TestCatalogue_ReconciledRoutesArePresent(t *testing.T) {
	cov := catalogueCoverage(builtCatalogue(t))
	for _, r := range projRoutes {
		switch r.disp {
		case reconciledByKind:
			tc := cov[r.table]
			if tc == nil || !tc.seenTable {
				t.Errorf("%s: table %q is declared reconciledByKind but is NOT a reconTarget in the catalogue — it would be a persisted-but-unreconciled blind spot", r.typeName, r.table)
				continue
			}
			if len(tc.kinds) == 0 {
				t.Errorf("%s: reconTarget %q has an EMPTY kinds list — the by-EventKind reconcile would expect zero rows and false-flag every populated ledger", r.typeName, r.table)
			}
			if r.kind != "" && !contains(tc.kinds, r.kind) {
				t.Errorf("%s: kind %q is not among reconTarget %q kinds %v — the reconcile would undercount the expected side (the defindex.strategy.harvest class)", r.typeName, r.kind, r.table, tc.kinds)
			}
		case reconciledByCensus:
			tc := cov[r.table]
			if tc == nil || !tc.census {
				t.Errorf("%s: table %q is declared reconciledByCensus but no census/ContractCall source in the catalogue targets it", r.typeName, r.table)
			}
		case noReconcile:
			// Coverage is not required; TestCatalogue_WaiversAreDeclared
			// enforces the written reason.
		}
	}
}

// TestCatalogue_WaiversAreDeclared: every noReconcile route carries a
// non-empty reason. A waiver is a design decision on the record, never a
// silent omission.
func TestCatalogue_WaiversAreDeclared(t *testing.T) {
	for _, r := range projRoutes {
		if r.disp != noReconcile {
			continue
		}
		if r.reason == "" {
			t.Errorf("%s → %s is noReconcile but has no reason — a projection-axis waiver must state why (fan-out / census / off-chain / observation)", r.typeName, r.table)
		}
	}
}

// TestCatalogue_DeclaredKindsMatchDecoderOutput is the decoder-truth
// cross-check: it INSTANTIATES the concrete emitter events and asserts their
// real EventKind() equals the kind the catalogue reconciles them under — so
// the catalogue's kind strings can never silently drift from the decoders,
// AND it enumerates defindex per-Direction so the defindex.strategy.harvest
// KIND-omission regression is caught explicitly (the type-level guard above
// cannot see a missing sub-kind on an already-routed type).
func TestCatalogue_DeclaredKindsMatchDecoderOutput(t *testing.T) {
	cov := catalogueCoverage(builtCatalogue(t))

	// emitters pairs a REAL decoder-output value with the (kind, table) the
	// catalogue must reconcile it under. Instances of the actual event types
	// (not string duplicates), so a renamed EventKind() breaks this test.
	emitters := []struct {
		ev    consumer.Event
		kind  string
		table string
	}{
		// The six 1:1 additions this change makes — pinned so the catalogue
		// kind strings stay welded to the decoders that emit them.
		{soroswap.LiquidityEvent{}, "soroswap.liquidity", "soroswap_liquidity"},
		{aquarius.RewardsEvent{}, "aquarius.rewards", "aquarius_rewards_events"},
		{aquarius.AdminEvent{}, "aquarius.admin", "aquarius_admin"},
		{aquarius.FeeEvent{}, "aquarius.fee", "aquarius_protocol_fee"},
		{aquarius.KillEvent{}, "aquarius.kill", "aquarius_kill_switches"},
		{phoenix.InitializeEvent{}, "phoenix.initialize", "phoenix_initialize"},
		{phoenix.AdminEvent{}, "phoenix.admin", "phoenix_admin_events"},
		// blend_emitter: the two 1:1 kinds that stay reconciled after the drop
		// fan-out kind was waived (2026-08-18) — pinned so the catalogue kind
		// strings stay welded to the decoder that emits them.
		{blend_emitter.DistributeEvent{}, "blend_emitter.distribute", "blend_emitter_events"},
		{blend_emitter.SwapConfigEvent{}, "blend_emitter.swap_config", "blend_emitter_events"},
		// defindex, enumerated per Direction — the harvest-regression guard.
		// Both layers land in defindex_flows; strategy.harvest MUST be a
		// reconciled kind (audit 2026-08-04 finding 4).
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionDeposit}}, "defindex.strategy.deposit", "defindex_flows"},
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionWithdraw}}, "defindex.strategy.withdraw", "defindex_flows"},
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionHarvest}}, "defindex.strategy.harvest", "defindex_flows"},
		{defindex.VaultEvent{Flow: defindex.VaultFlow{Direction: defindex.DirectionDeposit}}, "defindex.vault.deposit", "defindex_flows"},
		{defindex.VaultEvent{Flow: defindex.VaultFlow{Direction: defindex.DirectionWithdraw}}, "defindex.vault.withdraw", "defindex_flows"},
		// dfees (W5.2): per-asset fee distributions into their own
		// table — pinned so the catalogue kind string stays welded to
		// DFeesEvent.EventKind().
		{defindex.DFeesEvent{}, "defindex.vault.dfees", "defindex_fees"},
	}

	for _, e := range emitters {
		if got := e.ev.EventKind(); got != e.kind {
			t.Errorf("decoder %T emits EventKind()=%q but the catalogue reconciles it as %q — a drift that would make the reconcile expect zero and false-flag", e.ev, got, e.kind)
		}
		tc := cov[e.table]
		if tc == nil || !contains(tc.kinds, e.kind) {
			var kinds []string
			if tc != nil {
				kinds = tc.kinds
			}
			t.Errorf("kind %q (emitted by %T) is not reconciled by reconTarget %q (kinds=%v) — a persisted-but-unreconciled KIND (the defindex.strategy.harvest class)", e.kind, e.ev, e.table, kinds)
		}
	}
}
