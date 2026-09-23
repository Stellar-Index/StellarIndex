package main

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Pubnet ids, copied from the deployed [supply.sac_wrappers] template.
const (
	usdcIssuerAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSACContract   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	blndIssuerAccount = "GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	blndSACContract   = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
)

func crossCheckTestConfig(wrappers map[string]string) config.Config {
	cfg := config.Config{}
	cfg.Stellar.Network = "pubnet"
	cfg.Supply.WatchedClassicAssets = []string{"USDC-" + usdcIssuerAccount, "BLND-" + blndIssuerAccount}
	cfg.Supply.WatchedSEP41Contracts = []string{usdcSACContract, blndSACContract}
	cfg.Supply.SACWrappers = wrappers
	return cfg
}

// A dash-form sac_wrappers value names the same asset as the colon form
// the classic snapshot is keyed on; it must still produce a pair rather
// than silently leaving the cross-check unwired.
func TestBuildCrossCheckRefresher_DashFormWrapperValueIsCrossChecked(t *testing.T) {
	cfg := crossCheckTestConfig(map[string]string{usdcSACContract: "USDC-" + usdcIssuerAccount})

	// store is only wrapped, never dereferenced, before the function returns.
	r, err := buildCrossCheckRefresher(cfg, nil, discardLogger())
	if err != nil {
		t.Fatalf("buildCrossCheckRefresher: %v", err)
	}
	if r == nil {
		t.Fatal("buildCrossCheckRefresher returned no refresher: the dash-form sac_wrappers value was dropped and USDC is never cross-checked")
	}

	pairs, err := crossCheckPairs(cfg.Supply, cfg.Stellar.Passphrase(), discardLogger())
	if err != nil {
		t.Fatalf("crossCheckPairs: %v", err)
	}
	want := []supply.CrossCheckPair{{
		ClassicKey: "USDC:" + usdcIssuerAccount,
		SACKey:     usdcSACContract,
		WrapClass:  supply.WrapClassPartial,
	}}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs = %+v, want %+v (ClassicKey must be the colon-form snapshot key)", pairs, want)
	}
}

// A wrapper contract that is not the classic asset's SAC would compare
// two unrelated tokens under a 1-stroop tolerance; refuse at startup.
func TestBuildCrossCheckRefresher_RejectsWrapperThatIsNotTheAssetsSAC(t *testing.T) {
	cfg := crossCheckTestConfig(map[string]string{blndSACContract: "USDC:" + usdcIssuerAccount})

	r, err := buildCrossCheckRefresher(cfg, nil, discardLogger())
	if err == nil {
		t.Fatalf("buildCrossCheckRefresher accepted BLND's SAC as USDC's wrapper (refresher=%v)", r)
	}
	if !strings.Contains(err.Error(), usdcSACContract) {
		t.Fatalf("error %q should name the derived SAC %s", err, usdcSACContract)
	}
}

func TestCrossCheckPairs_ColonFormFullWrapAndSorted(t *testing.T) {
	cfg := crossCheckTestConfig(map[string]string{
		usdcSACContract: "USDC:" + usdcIssuerAccount,
		blndSACContract: "BLND-" + blndIssuerAccount,
	})
	cfg.Supply.FullyWrappedSACs = []string{usdcSACContract}

	pairs, err := crossCheckPairs(cfg.Supply, cfg.Stellar.Passphrase(), discardLogger())
	if err != nil {
		t.Fatalf("crossCheckPairs: %v", err)
	}
	want := []supply.CrossCheckPair{
		{ClassicKey: "USDC:" + usdcIssuerAccount, SACKey: usdcSACContract, WrapClass: supply.WrapClassFull},
		{ClassicKey: "BLND:" + blndIssuerAccount, SACKey: blndSACContract, WrapClass: supply.WrapClassPartial},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs = %+v, want %+v", pairs, want)
	}
}

// An entry missing from a watched set is skipped (not a startup error:
// sac_wrappers also feeds aliasing and the SAC observer), but loudly;
// a pure SEP-41 self-map has no classic side and is skipped quietly.
func TestCrossCheckPairs_UnwatchedEntryIsLoggedNotDropped(t *testing.T) {
	cfg := crossCheckTestConfig(map[string]string{
		usdcSACContract: "USDC-" + usdcIssuerAccount,
		blndSACContract: blndSACContract,
	})
	cfg.Supply.WatchedSEP41Contracts = []string{blndSACContract}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pairs, err := crossCheckPairs(cfg.Supply, cfg.Stellar.Passphrase(), logger)
	if err != nil {
		t.Fatalf("crossCheckPairs: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("pairs = %+v, want none", pairs)
	}
	logs := buf.String()
	if !strings.Contains(logs, "sac_id="+usdcSACContract) || !strings.Contains(logs, "in_watched_sep41_contracts=false") {
		t.Fatalf("skipped USDC wrapper not warned about; logs:\n%s", logs)
	}
	if strings.Contains(logs, "sac_id="+blndSACContract) {
		t.Fatalf("pure SEP-41 self-map should be skipped without a warning; logs:\n%s", logs)
	}
}

// The production emitter must remove the series, not zero it: a 0
// reads as "checked, agreed".
func TestObsCrossCheckEmitter_ClearDivergenceRemovesSeries(t *testing.T) {
	const classicKey = "CLEARTEST:" + usdcIssuerAccount
	e := obsCrossCheckEmitter{}
	e.Divergence(classicKey, supply.WrapClassPartial, 0)
	e.ClearDivergence(classicKey, supply.WrapClassPartial)
	if obs.SupplyCrossCheckDivergenceStroops.DeleteLabelValues(classicKey, string(supply.WrapClassPartial)) {
		t.Fatal("ClearDivergence left the gauge series in place; it would be re-exported on every scrape")
	}
}
