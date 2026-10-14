// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// Every consumer.Event type the sink persists must be covered by a reconTarget or carry a noReconcile waiver with a
// reason. The guard is type-level because decoders emit some kinds dynamically;
// TestCatalogue_DeclaredKindsMatchDecoderOutput backs it.

type routeDisp int

const (
	// reconciledByKind: the sink routes this event type to `table`, and the projection axis re-derives it by EventKind().
	// So `table` must be a reconTarget with non-empty kinds, and an explicit `kind` must be one of them.
	reconciledByKind routeDisp = iota
	// reconciledByCensus: reconciled by the SDEX lake-ops re-derive (src.census) or the ContractCall census (src.callDec).
	// `table` MUST be a target of such a source; kinds may legitimately be nil.
	reconciledByCensus
	// noReconcile: deliberately NOT on the projection axis. `reason` is
	// mandatory and load-bearing — it is the declared waiver the invariant
	// demands instead of a silent gap.
	noReconcile
	// waivedInCatalogue: written by a catalogue source but deliberately not
	// counted; the waiver and its reason live on that source's runtime
	// `waived` list, which TestCatalogue_RuntimeWaiversMatchRoutes pins.
	waivedInCatalogue
)

// projRoute declares how one (consumer.Event type -> table) edge is covered by the projection reconcile axis. One event
// type may have several routes (sorocredit.Event fans to four credit_* tables; aquarius.ReservesEvent to two by Kind).
type projRoute struct {
	// typeName is "pkg.Type" exactly as pipeline.handleEvent's type switch spells it (the dir basename).
	// It is matched against that switch's AST, so a renamed or removed type breaks the test instead of rotting the list.
	typeName string
	// table is the served table this edge writes (documentary for
	// noReconcile; asserted present for the reconciled dispositions).
	table string
	// kind, when non-empty, is the EventKind() this edge reconciles; the reconTarget for `table` must list it. It is ""
	// where the kind is verified elsewhere (TestCatalogue_DeclaredKindsMatchDecoderOutput); table presence is still checked.
	kind string
	disp routeDisp
	// reason is REQUIRED for noReconcile: the declared {source/table, why}
	// waiver.
	reason string
}

// accountObservationWaiver covers account_observations: a LedgerEntry observation that is covered
// by its own observer watermark. Do not reuse it for a table without one; see observationWaiverNoAxis.
const accountObservationWaiver = "LedgerEntry observation (supply observer), not a soroban-event " +
	"projection — outside the EventKind re-derive; covered by the account observer watermark " +
	"(migrations/0144_account_observer_watermark)"

// observationWaiverNoAxis covers the four non-account supply observers (trustline,
// claimable-balance, liquidity-pool-reserve, SAC-balance). None has a completeness or freshness
// mechanism, so this waiver documents the gap rather than claiming coverage that does not exist.
const observationWaiverNoAxis = "LedgerEntry observation (supply observer), not a soroban-event " +
	"projection — outside the EventKind re-derive; NO observer watermark or other completeness " +
	"mechanism exists for this table (unlike account_observations / migration 0144) — a real gap, " +
	"tracked here rather than silently assumed covered"

// externalWaiver covers the external CEX/FX source: off-chain vendor data
// with no on-chain lake to re-derive against.
const externalWaiver = "off-chain external (CEX/FX) vendor data — no on-chain lake event to re-derive against"

// projRoutes declares coverage for every consumer.Event type the sink persists.
// checkCatalogueEveryPersistedEventTypeIsRoutedOrWaived proves it exhaustive against handleEvent, so a new persist arm
// without a row fails CI.
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
	// aquarius.ReservesEvent routes to two tables by its runtime Kind field; EventKind() branches on Kind too
	// ("aquarius.reserves" / "aquarius.reserves_sync"), so the tables are attributable by kind; fan-out alone keeps them off
	// the reconcile axis.
	{typeName: "aquarius.ReservesEvent", table: "aquarius_reserves", disp: waivedInCatalogue},
	{typeName: "aquarius.ReservesEvent", table: "aquarius_reserves_sync", disp: waivedInCatalogue},
	{typeName: "aquarius.LiquidityEvent", table: "aquarius_liquidity", disp: waivedInCatalogue},

	// ── phoenix ──
	{typeName: "phoenix.TradeEvent", table: "trades", kind: "phoenix.trade", disp: reconciledByKind},
	{typeName: "phoenix.LiquidityEvent", table: "phoenix_liquidity", kind: "phoenix.liquidity", disp: reconciledByKind},
	{typeName: "phoenix.StakeEvent", table: "phoenix_stake_events", kind: "phoenix.stake", disp: reconciledByKind},
	{typeName: "phoenix.InitializeEvent", table: "phoenix_initialize", kind: "phoenix.initialize", disp: reconciledByKind},
	{typeName: "phoenix.AdminEvent", table: "phoenix_admin_events", kind: "phoenix.admin", disp: reconciledByKind},

	// ── comet ──
	{typeName: "comet.TradeEvent", table: "trades", kind: "comet.trade", disp: reconciledByKind},
	{typeName: "comet.LiquidityEvent", table: "comet_liquidity", kind: "comet.liquidity", disp: reconciledByKind},

	// ── sushiswap_v3 ──
	// The pool lifecycle events (init, upgraded, migrated) are gated and
	// recognized but emit no consumer.Event, so they need no route.
	{typeName: "sushiswap_v3.TradeEvent", table: "trades", kind: "sushiswap_v3.trade", disp: reconciledByKind},
	{typeName: "sushiswap_v3.PositionEvent", table: "sushiswap_v3_position_events", kind: "sushiswap_v3.position", disp: reconciledByKind},

	// ── upshift ──
	// One consumer.Event carries all four decoded kinds into one table, one row each. The eight
	// recognised custody, governance and allowance events emit no consumer.Event, so need no route.
	{typeName: "upshift.Event", table: "upshift_vault_events", kind: "upshift.vault_event", disp: reconciledByKind},

	// ── spectra ──
	{typeName: "spectra.Event", table: "spectra_events", kind: "spectra.event", disp: reconciledByKind},

	// ── blend (five kinds across four tables) ──
	{typeName: "blend.NewAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.FillAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.DeleteAuctionEvent", table: "blend_auctions", disp: reconciledByKind},
	{typeName: "blend.PositionEvent", table: "blend_positions", disp: reconciledByKind},
	{typeName: "blend.EmissionEvent", table: "blend_emissions", disp: reconciledByKind},
	{typeName: "blend.AdminEvent", table: "blend_admin", disp: reconciledByKind},
	{typeName: "blend_backstop.Event", table: "blend_backstop_events", disp: reconciledByKind},
	// blend_emitter distribute and swap_config are 1:1 and reconcile per ledger; drop fans out (one event, N recipient rows)
	// and is waived. The reconTarget carves drop rows out of the served side (whereFilter `event_kind <> 'drop'`) and omits
	// the drop kind.
	{typeName: "blend_emitter.DistributeEvent", table: "blend_emitter_events", kind: "blend_emitter.distribute", disp: reconciledByKind},
	{typeName: "blend_emitter.DropEvent", table: "blend_emitter_events", disp: waivedInCatalogue},
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
	// because the DECODER emits one event per entry) ──
	{typeName: "defindex.Event", table: "defindex_flows", disp: reconciledByKind},
	{typeName: "defindex.VaultEvent", table: "defindex_flows", disp: reconciledByKind},
	{typeName: "defindex.DFeesEvent", table: "defindex_fees", kind: "defindex.vault.dfees", disp: reconciledByKind},
	{typeName: "defindex.AdminEvent", table: "defindex_admin_events", kind: "defindex.vault.admin", disp: reconciledByKind},

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
	{typeName: "accounts.Observation", table: "account_observations", disp: noReconcile, reason: accountObservationWaiver},
	{typeName: "trustlines.Observation", table: "trustline_observations", disp: noReconcile, reason: observationWaiverNoAxis},
	{typeName: "claimable_balances.Observation", table: "claimable_observations", disp: noReconcile, reason: observationWaiverNoAxis},
	{typeName: "liquidity_pools.Observation", table: "lp_reserve_observations", disp: noReconcile, reason: observationWaiverNoAxis},
	{typeName: "sac_balances.Observation", table: "sac_balance_observations", disp: noReconcile, reason: observationWaiverNoAxis},
}

// sinkHandleEventTypes parses pipeline.handleEvent and returns the "pkg.Type" names its type switch persists: the
// authoritative universe (CI-complete via TestLockstep_EveryConsumerEventHasSinkArm). Duplicated because pipeline's AST
// guard helpers sit in an unimportable _test.go.
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

// checkCatalogueEveryPersistedEventTypeIsRoutedOrWaived is the omission guard. Every persisted
// consumer.Event type must appear in projRoutes (routed or waived), and every projRoutes entry
// must match a real persist arm, so a new table needs a reconTarget or a written waiver.
func checkCatalogueEveryPersistedEventTypeIsRoutedOrWaived(t *testing.T) {
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

// TestCatalogue_ReconciledRoutesArePresent is the non-vacuity anchor: a reconciledByKind table
// must be a reconTarget with non-empty kinds (including the route's kind), and a
// reconciledByCensus table must sit on a census or ContractCall source.
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
		case noReconcile, waivedInCatalogue:
			// Coverage is not required; TestCatalogue_WaiversAreDeclared and
			// TestCatalogue_RuntimeWaiversMatchRoutes enforce the reason.
		}
	}
}

// TestCatalogue_RuntimeWaiversMatchRoutes: every waivedInCatalogue route has a runtime `waived` entry with a reason, and
// vice versa. What "reconciled" excludes is thus a runtime fact, not one only this test file knows.
func TestCatalogue_RuntimeWaiversMatchRoutes(t *testing.T) {
	runtime := map[string]bool{}
	for _, src := range builtCatalogue(t) {
		for _, w := range src.waived {
			if w.table == "" || w.reason == "" {
				t.Errorf("%s: waived entry %+v needs a table and a reason", src.name, w)
			}
			runtime[w.table] = true
		}
	}
	declared := map[string]bool{}
	for _, r := range projRoutes {
		if r.disp != waivedInCatalogue {
			continue
		}
		declared[r.table] = true
		if !runtime[r.table] {
			t.Errorf("%s → %s is waivedInCatalogue but no catalogue source lists it in `waived`", r.typeName, r.table)
		}
	}
	for table := range runtime {
		if !declared[table] {
			t.Errorf("catalogue waives %q but no projRoute declares it waivedInCatalogue", table)
		}
	}
	if len(runtime) == 0 {
		t.Fatal("no runtime waivers found — the aquarius fan-out and blend_emitter drop waivers must be in the catalogue")
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

// TestCatalogue_ObservationWaiversDoNotOverclaimCoverage pins that only account_observations may
// claim watermark coverage; the other four supply observers have no mechanism, and their waivers
// must say so rather than assert coverage that does not exist.
func TestCatalogue_ObservationWaiversDoNotOverclaimCoverage(t *testing.T) {
	const falseCoverageClaim = "covered by the observer's own coverage axis"
	nonAccountObservers := map[string]bool{
		"trustlines.Observation":         true,
		"claimable_balances.Observation": true,
		"liquidity_pools.Observation":    true,
		"sac_balances.Observation":       true,
	}
	for _, r := range projRoutes {
		if !nonAccountObservers[r.typeName] {
			continue
		}
		if strings.Contains(r.reason, falseCoverageClaim) {
			t.Errorf("%s → %s waiver claims %q, but no observer watermark or other completeness mechanism exists for this table (only account_observations has one, migration 0144) — the reason overclaims coverage", r.typeName, r.table, falseCoverageClaim)
		}
		if !strings.Contains(r.reason, "NO observer watermark") && !strings.Contains(r.reason, "no observer watermark") {
			t.Errorf("%s → %s waiver does not state that no completeness mechanism exists for this table", r.typeName, r.table)
		}
	}
	// accounts is the one observer that DOES have a mechanism; its waiver
	// must say so, not use the no-axis wording.
	for _, r := range projRoutes {
		if r.typeName != "accounts.Observation" {
			continue
		}
		if !strings.Contains(r.reason, "watermark") {
			t.Errorf("accounts.Observation waiver does not cite the account observer watermark (migration 0144) it is actually covered by: %q", r.reason)
		}
	}
}

// TestCatalogue_DeclaredKindsMatchDecoderOutput instantiates the concrete emitter events and
// asserts their real EventKind() equals the catalogue kind, including defindex per Direction,
// since the type-level guard cannot see a missing sub-kind on an already-routed type.
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
		// fan-out kind was waived — pinned so the catalogue kind
		// strings stay welded to the decoder that emits them.
		{blend_emitter.DistributeEvent{}, "blend_emitter.distribute", "blend_emitter_events"},
		{blend_emitter.SwapConfigEvent{}, "blend_emitter.swap_config", "blend_emitter_events"},
		// defindex, enumerated per Direction — the harvest-regression guard.
		// Both layers land in defindex_flows; strategy.harvest MUST be a
		// reconciled kind.
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionDeposit}}, "defindex.strategy.deposit", "defindex_flows"},
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionWithdraw}}, "defindex.strategy.withdraw", "defindex_flows"},
		{defindex.Event{Flow: defindex.StrategyFlow{Direction: defindex.DirectionHarvest}}, "defindex.strategy.harvest", "defindex_flows"},
		{defindex.VaultEvent{Flow: defindex.VaultFlow{Direction: defindex.DirectionDeposit}}, "defindex.vault.deposit", "defindex_flows"},
		{defindex.VaultEvent{Flow: defindex.VaultFlow{Direction: defindex.DirectionWithdraw}}, "defindex.vault.withdraw", "defindex_flows"},
		// dfees: per-asset fee distributions into their own
		// table — pinned so the catalogue kind string stays welded to
		// DFeesEvent.EventKind().
		{defindex.DFeesEvent{}, "defindex.vault.dfees", "defindex_fees"},
		{defindex.AdminEvent{}, "defindex.vault.admin", "defindex_admin_events"},
		{sushiswap_v3.TradeEvent{}, "sushiswap_v3.trade", "trades"},
		{sushiswap_v3.PositionEvent{}, "sushiswap_v3.position", "sushiswap_v3_position_events"},
		// upshift: one kind covers deposit / withdraw / share transfer /
		// deployed-assets change — the row's event_kind column
		// discriminates them, the wire EventKind does not.
		{upshift.Event{}, "upshift.vault_event", "upshift_vault_events"},
		{spectra.Event{}, spectra.EventKind, "spectra_events"},
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
