// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// [Store.HasAsset] is the 404 gate on GET /v1/assets/{id}. Its non-classic arm must not be an
// UNBOUNDED existence scan of the `trades` hypertable:
//
//	SELECT EXISTS (SELECT 1 FROM trades
//	                WHERE base_asset = $1 OR quote_asset = $1 LIMIT 1)
//
// Over compressed historical chunks that shape blows the 15 s request budget while the classic
// arm answers in milliseconds. A seeded-row test cannot see this, since a defective query also
// returns true for a seeded row; the defect is in the STATEMENT SHAPE, so these tests pin it
// with the scripted driver that records the SQL and bound args.

// hasAssetStmt runs HasAsset against one canned boolean and returns the
// single statement the store issued plus the answer it decoded.
func hasAssetStmt(t *testing.T, a canonical.Asset, exists bool) (recordedStmt, bool) {
	t.Helper()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"exists"},
		rows: [][]driver.Value{{exists}},
	})
	got, err := store.HasAsset(context.Background(), a)
	if err != nil {
		t.Fatalf("HasAsset(%s): %v", a.String(), err)
	}
	return conn.only(t), got
}

// unwindowedTradesReads returns the `FROM trades` reads in q with no `ts >=` bound in their own
// subquery, i.e. the reads the planner cannot prune chunks for.
//
// Scoping to the subquery is deliberate: a statement bounding ONE trades read but leaving a
// sibling unbounded still walks every chunk, which a whole-statement check would call clean.
func unwindowedTradesReads(q string) []string {
	var out []string
	rest := q
	for {
		i := strings.Index(rest, "FROM trades")
		if i < 0 {
			return out
		}
		body := subqueryBody(rest[i:])
		if !strings.Contains(body, "ts >=") {
			out = append(out, strings.Join(strings.Fields(body), " "))
		}
		rest = rest[i+len("FROM trades"):]
	}
}

// subqueryBody returns the prefix of s up to the close paren that ends
// the subquery it starts in (or all of s when the read is top-level).
func subqueryBody(s string) string {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return s[:i]
			}
			depth--
		}
	}
	return s
}

// TestHasAsset_NativeIssuesNoUnboundedTradesScan is the regression this
// fix exists for: every `trades` read on the native path must carry the
// `ts >=` bound that lets the planner prune to the window's chunks. RED
// against an unbounded query, whose single read has no bound at all.
func TestHasAsset_ProbeIssuesNoUnboundedTradesScan(t *testing.T) {
	stmt, has := hasAssetStmt(t, probeAsset(t), true)

	if bad := unwindowedTradesReads(stmt.sql); len(bad) > 0 {
		t.Errorf(`HasAsset(probe asset) issues %d unbounded read(s) of the trades hypertable.

An existence read with no `+"`ts >=`"+` bound cannot be chunk-pruned, so the
planner appends every chunk in the hypertable — thousands of them on r1,
nearly all compressed — until a row turns up. That is what blew the 15 s
budget on GET /v1/assets/native (2026-09-09), and it is the shape the
standing no-unbounded-trade-scan rule forbids.

unbounded read(s):
%s

full SQL:
%s`, len(bad), strings.Join(bad, "\n"), indent(stmt.sql))
	}
	if !has {
		t.Error("HasAsset(probe asset) = false, want true (the scripted row says the asset exists)")
	}
}

// TestHasAsset_NativeWindowIsTheListingWindow pins the bound VALUE, not merely its presence: the
// floor is now - MarketsRecencyWindow in UTC, the window Store.DistinctAssets uses, so the detail
// route cannot 404 an asset the listing shows. Computing it Go-side (not `now() - INTERVAL`)
// makes it a constant the planner can prune chunks with at plan time.
func TestHasAsset_ProbeWindowIsTheListingWindow(t *testing.T) {
	before := time.Now().UTC()
	stmt, _ := hasAssetStmt(t, probeAsset(t), true)
	after := time.Now().UTC()

	since, ok := stmt.arg(t, 2).(time.Time)
	if !ok {
		t.Fatalf("$2 is %T, want time.Time (the window floor)", stmt.arg(t, 2))
	}
	if since.Location() != time.UTC {
		t.Errorf("window floor location = %s, want UTC", since.Location())
	}
	// The floor is now - MarketsRecencyWindow; `now` is read inside the
	// call, so the admissible answer is the interval the call spanned.
	lo, hi := before.Add(-MarketsRecencyWindow), after.Add(-MarketsRecencyWindow)
	if since.Before(lo) || since.After(hi) {
		t.Errorf(`window floor = %s, want now - MarketsRecencyWindow (%s), i.e. within [%s, %s].

MarketsRecencyWindow is the window Store.DistinctAssets bounds the
/v1/assets listing with. Binding a SHORTER one here makes the detail
route 404 assets the listing still shows; binding a longer one drags
compressed chunks back onto the request path.`,
			since, MarketsRecencyWindow, lo, hi)
	}

	if strings.Contains(stmt.sql, "now() - INTERVAL") || strings.Contains(stmt.sql, "now() - interval") {
		t.Errorf(`the window floor is computed in SQL with now() - INTERVAL.

Bind it Go-side instead: a constant timestamp parameter is what lets the
planner exclude chunks at PLAN time (the trick Store.DistinctAssets and
Store.RecentSorobanDEXTrades both document).

SQL:
%s`, indent(stmt.sql))
	}
}

// TestHasAsset_NonClassicProbesEveryAliasForm pins the XLM dual-form rule at this seam:
// `native`, `crypto:XLM` and the XLM SAC are one asset, so each spelling must probe all three,
// or a request under one spelling answers false while the asset is present.
func TestHasAsset_NonClassicProbesEveryAliasForm(t *testing.T) {
	xlmSAC, err := canonical.NewSorobanAsset(canonical.XLMSacContractID)
	if err != nil {
		t.Fatalf("NewSorobanAsset(XLM SAC): %v", err)
	}
	cryptoXLM, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}

	want := canonical.AssetAliasStrings(canonical.NativeAsset())
	if len(want) != 3 {
		t.Fatalf("XLM alias family has %d forms, want 3 (native, crypto:XLM, SAC): %v", len(want), want)
	}

	for _, spelling := range []canonical.Asset{cryptoXLM, xlmSAC} {
		t.Run(spelling.String(), func(t *testing.T) {
			stmt, _ := hasAssetStmt(t, spelling, true)

			forms, ok := stmt.arg(t, 1).([]string)
			if !ok {
				t.Fatalf("$1 is %T, want []string (the alias array bound for `= ANY($1)`)", stmt.arg(t, 1))
			}
			if len(forms) != len(want) {
				t.Fatalf("HasAsset(%s) bound %d alias form(s), want %d: got %v, want %v",
					spelling.String(), len(forms), len(want), forms, want)
			}
			missing := make([]string, 0, len(want))
			for _, w := range want {
				found := false
				for _, g := range forms {
					if g == w {
						found = true
						break
					}
				}
				if !found {
					missing = append(missing, w)
				}
			}
			if len(missing) > 0 {
				t.Errorf(`HasAsset(%s) never probes %v.

native / crypto:XLM / the XLM SAC are the same asset under three
canonical ids (ADR-0014 + the XLM dual-form rule). An existence read
keyed on one spelling reports the asset absent whenever the trades that
exist were written under another.

bound forms: %v`, spelling.String(), missing, forms)
			}
			if bad := unwindowedTradesReads(stmt.sql); len(bad) > 0 {
				t.Errorf("HasAsset(%s) issues %d unbounded trades read(s): %v",
					spelling.String(), len(bad), bad)
			}
		})
	}
}

// TestHasAsset_NonClassicAlwaysProbesItsOwnSpelling covers types with no second form (fiat, RWA,
// ordinary Soroban contracts). The bound array must still carry the caller's own asset_id: an
// empty `= ANY($1)` matches nothing and would 404 every asset the alias registry ignores.
func TestHasAsset_NonClassicAlwaysProbesItsOwnSpelling(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatalf("NewFiatAsset(EUR): %v", err)
	}
	rwa, err := canonical.NewRWAAsset("BENJI")
	if err != nil {
		t.Fatalf("NewRWAAsset(BENJI): %v", err)
	}
	contract, err := canonical.NewSorobanAsset("CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH")
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}

	for _, a := range []canonical.Asset{eur, rwa, contract} {
		t.Run(a.String(), func(t *testing.T) {
			stmt, _ := hasAssetStmt(t, a, false)
			forms, ok := stmt.arg(t, 1).([]string)
			if !ok {
				t.Fatalf("$1 is %T, want []string", stmt.arg(t, 1))
			}
			if len(forms) != 1 || forms[0] != a.String() {
				t.Errorf("HasAsset(%s) bound %v, want exactly [%q] — an asset with no "+
					"second canonical form must still probe its own spelling",
					a.String(), forms, a.String())
			}
		})
	}
}

// TestHasAsset_AbsentNonClassicAnswersWithoutFallback pins that a MISS is conclusive: a bounded
// probe that falls back to the unbounded scan on a miss would keep the timeout for every asset
// the index lacks, which is what a scraper hits. One statement, one answer.
//
// It also passes against an unbounded single-statement implementation: it is a forward guard
// against that ladder, not proof of the fix, which the three tests above provide.
func TestHasAsset_AbsentNonClassicAnswersWithoutFallback(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatalf("NewFiatAsset(EUR): %v", err)
	}
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"exists"},
		rows: [][]driver.Value{{false}},
	})
	has, err := store.HasAsset(context.Background(), eur)
	if err != nil {
		t.Fatalf("HasAsset(fiat:EUR): %v", err)
	}
	if has {
		t.Error("HasAsset(fiat:EUR) = true, want false (the scripted row says the asset is absent)")
	}
	if n := len(conn.stmts); n != 1 {
		t.Fatalf(`a HasAsset miss issued %d statements, want 1.

A second statement here is a full-history fallback: the bounded probe
would be decoration, and every unknown asset would still pay the
unbounded scan. Statements:
%v`, n, conn.statements())
	}
}

// TestHasAsset_ClassicStillBypassesTrades guards the dispatch: the classic arm is fast because
// it never touches the hypertable, so routing it through the bounded probe would regress it.
// Like the test above, this is a forward guard.
func TestHasAsset_ClassicStillBypassesTrades(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	stmt, has := hasAssetStmt(t, usdc, true)

	if strings.Contains(stmt.sql, "FROM trades") {
		t.Errorf(`the classic arm reads the trades hypertable.

classic_assets is a strict superset of the traded classic assets
(registerClassicAssetSeen writes it on every trade insert), so the
registry answers on its own — that short-circuit is the F-0157 fix.

SQL:
%s`, indent(stmt.sql))
	}
	if !strings.Contains(stmt.sql, "FROM classic_assets") {
		t.Errorf("the classic arm no longer reads classic_assets:\n%s", indent(stmt.sql))
	}
	if !has {
		t.Error("HasAsset(USDC-GA5Z…) = false, want true")
	}
}

// TestHasAsset_NativeNeedsNoTradeEvidence pins that XLM resolves on a network with no XLM trades.
// The non-classic probe bounds existence on MarketsRecencyWindow, right for assets DISCOVERED by
// trading, but XLM exists from genesis on every network; routed through the probe it would 404 on
// a quiet network and 200 on a busy one. A seeded trade cannot see this, so the test asserts the
// store issues NO statement at all.
func TestHasAsset_NativeNeedsNoTradeEvidence(t *testing.T) {
	t.Parallel()

	store, script := newScriptedStore(t)
	ok, err := store.HasAsset(context.Background(), canonical.Asset{Type: canonical.AssetNative})
	if err != nil {
		t.Fatalf("HasAsset(native): %v", err)
	}
	if !ok {
		t.Error("native reported ABSENT — XLM exists on every Stellar network from genesis, " +
			"traded or not; a quiet network must not 404 its own native asset")
	}
	if n := len(script.statements()); n != 0 {
		t.Errorf("native issued %d statement(s), want 0 — it needs no evidence: %v",
			n, script.statements())
	}
}

// probeAsset returns a non-native, non-classic asset, one discovered by trade activity that
// genuinely goes through the bounded probe. Native cannot serve: HasAsset short-circuits it
// without a statement. The no-unbounded-scan property must hold for everything needing evidence.
func probeAsset(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(canonical.XLMSacContractID)
	if err != nil {
		t.Fatalf("NewSorobanAsset(XLM SAC): %v", err)
	}
	return a
}
