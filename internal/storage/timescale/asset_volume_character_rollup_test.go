package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestAssetVolumeCharacterRollupSQL_Shape pins the all-asset roll to the
// SAME signal definitions the per-asset assetVolumeCharacterSQL uses, so a
// future edit can't silently let the rollup drift from the value the detail
// used to compute live (the oracle guards it end-to-end; this guards the
// SQL text cheaply).
func TestAssetVolumeCharacterRollupSQL_Shape(t *testing.T) {
	q := assetVolumeCharacterRollupSQLTemplate
	musts := []string{
		// Priced volume only, on BOTH sides (base + quote projections).
		"t.usd_volume IS NOT NULL",
		"LEFT JOIN alias_map bm ON bm.form = t.base_asset",
		"LEFT JOIN alias_map qm ON qm.form = t.quote_asset",
		"UNION ALL",
		// Trailing window as a literal so the planner excludes older chunks.
		"t.ts >= now() - interval '{{WINDOW}}'",
		// UNORDERED account pair so a round-trip folds to one pair.
		"GROUP BY asset_id, LEAST(COALESCE(maker, taker), taker), GREATEST(COALESCE(maker, taker), taker)",
		// Self-cross share.
		"a.maker IS NOT NULL AND a.maker = a.taker",
		// Issuer-side predicate, derived per canonical asset, no-op when ''.
		"a.issuer <> '' AND (a.maker = a.issuer OR a.taker = a.issuer)",
		// Issuer derivation: the G-strkey suffix of a classic id.
		"split_part(asset_id, '-', 2)",
		// Market-surface (real price surface) detection on the RAW counterpart.
		"a.counterpart = 'native'",
		"a.counterpart LIKE 'fiat:%'",
		"a.counterpart LIKE 'USDC-%'",
		// Exact NUMERIC volume for the stored column (ADR-0003).
		"SUM(a.v_num)::text",
		// Grouped per canonical asset.
		"GROUP BY a.asset_id",
	}
	for _, m := range musts {
		if !strings.Contains(q, m) {
			t.Errorf("assetVolumeCharacterRollupSQLTemplate missing %q", m)
		}
	}
	if strings.Contains(q, "::interval") {
		t.Errorf("template binds the window as a parameter; chunk exclusion needs a literal")
	}
	// The %-carrying LIKE patterns must survive templating untouched — the
	// sentinel is replaced by strings.Replace, never fmt.Sprintf.
	if strings.Contains(q, "%s") {
		t.Errorf("template still uses a %%s verb — Sprintf would mangle the LIKE %% patterns")
	}
}

// TestBuildAliasMapValues_XLMBaseline proves the alias-fold VALUES bind the
// compile-time baseline (crypto:XLM + the XLM SAC → native, crypto:EUROC →
// crypto:EURC) as ::text params starting at the given index, sorted by form.
func TestBuildAliasMapValues_XLMBaseline(t *testing.T) {
	canonical.InstallAliasRegistry(nil) // compile-time baseline

	valuesSQL, args := buildAliasMapValues(2)
	wantSQL := "($2::text, $3::text), ($4::text, $5::text), ($6::text, $7::text)"
	if valuesSQL != wantSQL {
		t.Errorf("buildAliasMapValues(2) placeholders = %q, want %q", valuesSQL, wantSQL)
	}
	want := []any{
		canonical.XLMSacContractID, "native",
		"crypto:EUROC", "crypto:EURC",
		"crypto:XLM", "native",
	}
	if len(args) != len(want) {
		t.Fatalf("buildAliasMapValues args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("alias arg[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

// TestAdjustedVolumeExpr_Wired proves the §4-B concentration-adjusted sort
// key is the SAME expression in all three places that must agree: the
// listing SELECT (as sort_vol_usd), assetsOrderBy, and
// assetsCursorPredicate — a drift would break keyset pagination.
func TestAdjustedVolumeExpr_Wired(t *testing.T) {
	if !strings.Contains(listAssetsBaseSelect, adjustedVolume24hExpr) {
		t.Error("listAssetsBaseSelect does not embed adjustedVolume24hExpr as sort_vol_usd")
	}
	if !strings.Contains(listAssetsBaseSelect, "AS sort_vol_usd") {
		t.Error("listAssetsBaseSelect missing the sort_vol_usd column")
	}
	if !strings.Contains(listAssetsBaseSelect, "avc.character") ||
		!strings.Contains(listAssetsBaseSelect, "AS volume_character") {
		t.Error("listAssetsBaseSelect missing the volume_character column")
	}
	if !strings.Contains(listAssetsBaseSelect, "LEFT JOIN asset_volume_character avc") {
		t.Error("listAssetsBaseSelect missing the asset_volume_character LEFT JOIN")
	}
	if ob := assetsOrderBy(AssetsOrderVolume24hUSDDesc); !strings.Contains(ob, adjustedVolume24hExpr) {
		t.Errorf("assetsOrderBy(volume) = %q, does not rank on the adjusted expr", ob)
	}
	if cp := assetsCursorPredicate(AssetsOrderVolume24hUSDDesc, 5); !strings.Contains(cp, adjustedVolume24hExpr) {
		t.Errorf("assetsCursorPredicate(volume) = %q, does not compare the adjusted expr", cp)
	}
	// The demotion only touches concentrated/operational; market/unrated
	// keep raw volume (ELSE 1).
	if !strings.Contains(adjustedVolume24hExpr, "avc.character IN ('concentrated', 'operational')") {
		t.Error("adjustedVolume24hExpr must demote only concentrated/operational")
	}
	if !strings.Contains(adjustedVolume24hExpr, "1::numeric - avc.top_account_pair_vol_share::numeric") {
		t.Error("adjustedVolume24hExpr must scale raw volume by (1 - top_account_pair_vol_share)")
	}
}

// TestRefreshAssetVolumeCharacter_zeroRowPassKeepsLastGood: a roll that
// returned no rows is an upstream fault, not every asset lapsing at once,
// so the pass runs the expiry prune rather than emptying the rollup.
func TestRefreshAssetVolumeCharacter_zeroRowPassKeepsLastGood(t *testing.T) {
	t.Parallel()

	store, script := newScriptedStore(t,
		scriptedResult{}, // SET max_parallel_workers_per_gather
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{"30000ms"}}}, // captured prior statement_timeout
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{""}}},        // captured prior application_name
		scriptedResult{}, // set application_name
		scriptedResult{}, // SET statement_timeout
		scriptedResult{cols: []string{
			"asset_id", "total", "total_numeric", "makers", "takers",
			"top_pair", "self_cross", "issuer_side", "market_styled",
		}}, // the roll: zero rows
		scriptedResult{}, // restore statement_timeout (set_config)
		scriptedResult{}, // restore application_name
		scriptedResult{}, // RESET max_parallel_workers_per_gather
		scriptedResult{}, // prune
	)
	if err := store.RefreshAssetVolumeCharacter(context.Background()); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}
	got := script.statements()
	if len(got) != 10 {
		t.Fatalf("expected 10 statements (no upsert batch for zero rows), got %d: %v", len(got), got)
	}
	if got[6] != `SELECT set_config('statement_timeout', $1, false)` {
		t.Errorf("statement_timeout restore = %q, want the captured-value set_config restore", got[6])
	}
	if got[9] != refreshAssetVolumeCharacterPruneExpired {
		t.Errorf("zero-row pass prune = %q, want %q", got[9], refreshAssetVolumeCharacterPruneExpired)
	}
	if !script.committed() {
		t.Error("zero-row pass must commit its expiry prune")
	}
}

// TestRollAssetVolumeCharacter_RestoresCapturedStatementTimeout proves the
// pinned connection's PRIOR statement_timeout (whatever an [OpenBackground]
// connector's session backstop set it to — REC-08) is captured before this
// call's own assetVolumeCharacterRollTimeout override and restored — via set_config with the
// captured value, not a bare RESET — before the connection goes back to
// the pool. Without the capture/restore, a later query landing on the
// same pooled connection would silently inherit this call's assetVolumeCharacterRollTimeout (45min) bound
// (or the server default) instead of the operator's configured backstop.
func TestRollAssetVolumeCharacter_RestoresCapturedStatementTimeout(t *testing.T) {
	t.Parallel()

	const priorBackstop = "2m" // distinct from both the server default and this call's own assetVolumeCharacterRollTimeout (45min)
	store, script := newScriptedStore(t,
		scriptedResult{}, // SET max_parallel_workers_per_gather
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{priorBackstop}}}, // captured
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{""}}},            // captured prior application_name
		scriptedResult{}, // set application_name
		scriptedResult{}, // SET statement_timeout = assetVolumeCharacterRollTimeout (45min)
		scriptedResult{cols: []string{
			"asset_id", "total", "total_numeric", "makers", "takers",
			"top_pair", "self_cross", "issuer_side", "market_styled",
		}}, // the roll: zero rows
		scriptedResult{}, // restore
		scriptedResult{}, // restore application_name
		scriptedResult{}, // RESET max_parallel_workers_per_gather
	)

	if _, err := store.rollAssetVolumeCharacter(context.Background()); err != nil {
		t.Fatalf("rollAssetVolumeCharacter: %v", err)
	}

	got := script.statements()
	if len(got) != 9 {
		t.Fatalf("expected 9 statements, got %d: %v", len(got), got)
	}
	if got[6] != `SELECT set_config('statement_timeout', $1, false)` {
		t.Fatalf("restore statement = %q, want the captured-value set_config restore", got[6])
	}
	if name := script.stmts[3].arg(t, 1); name != AssetVolumeCharacterRollApplicationName {
		t.Errorf("application_name = %v, want %q", name, AssetVolumeCharacterRollApplicationName)
	}
	restoreArg := script.stmts[6].arg(t, 1)
	if restoreArg != priorBackstop {
		t.Errorf("restore arg = %v, want the captured prior value %q (not a bare RESET to the server default)", restoreArg, priorBackstop)
	}
}

// TestRollAssetVolumeCharacter_FailedRestoreFailsTheCall proves a restore
// that fails is never silently swallowed while the connection is handed
// back to the pool: the pooled connection must never carry this call's
// statement_timeout override forward, so a failed restore fails the call.
func TestRollAssetVolumeCharacter_FailedRestoreFailsTheCall(t *testing.T) {
	t.Parallel()

	restoreErr := errors.New("connection reset by peer")
	store, _ := newScriptedStore(t,
		scriptedResult{}, // SET max_parallel_workers_per_gather
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{"2m"}}},
		scriptedResult{cols: []string{"current_setting"}, rows: [][]driver.Value{{""}}}, // captured prior application_name
		scriptedResult{}, // set application_name
		scriptedResult{}, // SET statement_timeout = assetVolumeCharacterRollTimeout (45min)
		scriptedResult{cols: []string{
			"asset_id", "total", "total_numeric", "makers", "takers",
			"top_pair", "self_cross", "issuer_side", "market_styled",
		}},
		scriptedResult{err: restoreErr}, // restore fails
	)

	_, err := store.rollAssetVolumeCharacter(context.Background())
	if err == nil {
		t.Fatal("rollAssetVolumeCharacter: want error when the statement_timeout restore fails, got nil")
	}
	if !strings.Contains(err.Error(), "restore statement_timeout") {
		t.Errorf("error = %q, want it to name the failed restore", err.Error())
	}
}

// TestAssetVolumeCharacterRollup_VolumeUSDExact: volume_usd is a NUMERIC
// money sum and must reach the caller as its own decimal text. A double
// hop cannot hold 90071992547409.93 (above 2^53 cents) and serves the
// wrong cents (ADR-0003).
func TestAssetVolumeCharacterRollup_VolumeUSDExact(t *testing.T) {
	t.Parallel()

	const exact = "90071992547409.93"
	store, script := newScriptedStore(t, scriptedResult{
		cols: []string{
			"window_days", "volume_usd", "distinct_makers", "distinct_takers",
			"top_account_pair_vol_share", "self_cross_share", "issuer_side_share",
			"market_styled_share", "is_market_styled", "character",
		},
		rows: [][]driver.Value{{
			int64(14), exact, int64(3), int64(2),
			0.5, 0.0, 0.0, 1.0, true, VolumeCharacterMarket,
		}},
	})
	vc, found, err := store.AssetVolumeCharacterRollup(context.Background(), "native")
	if err != nil || !found {
		t.Fatalf("AssetVolumeCharacterRollup: found=%v err=%v", found, err)
	}
	if got := fmt.Sprint(vc.VolumeUSD); got != exact {
		t.Errorf("VolumeUSD = %s, want the NUMERIC verbatim %s", got, exact)
	}
	if q := script.statements()[0]; strings.Contains(q, "double precision") {
		t.Errorf("rollup read casts volume_usd through double precision:\n%s", q)
	}
}
