package ingest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestParseSnapScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantAll   bool
		wantStore bool
		wantErr   bool
	}{
		{"contracts", false, false, false},
		{"all", true, false, false},
		{"storage", false, true, false},
		{"", false, false, true},
		{"nonsense", false, false, true},
	}
	for _, c := range cases {
		sc, err := parseSnapScope(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSnapScope(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSnapScope(%q): unexpected error %v", c.in, err)
			continue
		}
		if sc.all != c.wantAll || sc.storage != c.wantStore {
			t.Errorf("parseSnapScope(%q) = %+v, want {all:%v storage:%v}", c.in, sc, c.wantAll, c.wantStore)
		}
	}
}

// TestShouldCollect_ContractDataStorage is the regression guard for the
// 2026-07-06 dormant-current-state fill: contract_data STORAGE entries (SAC
// Balance / Blend reserve) must be collected under scope=storage but NOT under
// scope=contracts or scope=all — the exact gap that hid ~99% of PHO supply.
func TestShouldCollect_ContractDataStorage(t *testing.T) {
	t.Parallel()
	// (typ, isInstance) → collected? per scope.
	type key struct {
		typ        xdr.LedgerEntryType
		isInstance bool
	}
	cases := []struct {
		name                    string
		k                       key
		contracts, all, storage bool
	}{
		{"contract_data storage", key{xdr.LedgerEntryTypeContractData, false}, false, false, true},
		{"contract_data instance", key{xdr.LedgerEntryTypeContractData, true}, true, true, true},
		{"contract_code", key{xdr.LedgerEntryTypeContractCode, false}, true, true, true},
		{"liquidity_pool", key{xdr.LedgerEntryTypeLiquidityPool, false}, false, true, true},
		{"account", key{xdr.LedgerEntryTypeAccount, false}, false, true, false},
		{"trustline", key{xdr.LedgerEntryTypeTrustline, false}, false, true, false},
		{"ttl (never)", key{xdr.LedgerEntryTypeTtl, false}, false, false, false},
	}
	scopes := map[string]snapScope{
		"contracts": {},
		"all":       {all: true},
		"storage":   {storage: true},
	}
	for _, c := range cases {
		want := map[string]bool{"contracts": c.contracts, "all": c.all, "storage": c.storage}
		for sname, sc := range scopes {
			tally := &snapTally{scope: sc}
			got := tally.shouldCollect(c.k.typ, c.k.isInstance)
			if got != want[sname] {
				t.Errorf("%s under scope=%s: shouldCollect=%v, want %v", c.name, sname, got, want[sname])
			}
		}
	}
}

// TestShouldCollectDoc_DoesNotCiteUnrelatedPR is the regression for RSWP-011:
// shouldCollect's doc comment cited "#30" as the LP-scope reader, but PR #30
// is "Remove TradingView attribution logo from charts" (merged 2026-07-21) —
// unrelated to state-snapshot or LP reserves. A reader following that
// reference lands on the wrong PR entirely. The doc must instead name the
// actual ADR-0039 native liquidity-pool reserve reader.
func TestShouldCollectDoc_DoesNotCiteUnrelatedPR(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "state_snapshot.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse state_snapshot.go: %v", err)
	}

	var doc *ast.CommentGroup
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "shouldCollect" {
			continue
		}
		doc = fd.Doc
	}
	if doc == nil {
		t.Fatal("shouldCollect is gone from state_snapshot.go (or lost its doc comment) — this guard has moved")
	}
	text := doc.Text()

	if strings.Contains(text, "#30") {
		t.Errorf("shouldCollect doc comment still cites \"#30\" as the LP-scope reader — PR #30 is "+
			"\"Remove TradingView attribution logo from charts\" (merged 2026-07-21), unrelated: %q", text)
	}
	if !strings.Contains(text, "liquidity_pool_state_reader.go") {
		t.Errorf("shouldCollect doc comment does not name internal/storage/clickhouse/liquidity_pool_state_reader.go "+
			"— a reader would not know which reader actually consumes the LP scope=all/storage rows: %q", text)
	}
}

// TestResolveArchiveTarget_WriteRefusesUnresolvedConfig is the regression for
// T240: -write inserts the checkpoint straight into the target ClickHouse's
// ledger_entry_changes with no cross-check against the network that
// ClickHouse instance actually tracks. On a config load failure,
// resolveArchiveTarget used to log one stderr line and silently fall back to
// the public pubnet archive/passphrase — so a stale or missing -config could
// make -write insert mainnet's checkpoint into a testnet ledger (or vice
// versa) with no abort and no operator-visible failure. For write=true, an
// unresolved config must return an error, not a fallback.
func TestResolveArchiveTarget_WriteRefusesUnresolvedConfig(t *testing.T) {
	t.Parallel()
	const missingCfg = "/nonexistent/stellarindex-config-does-not-exist.toml"

	url, passphrase, err := resolveArchiveTarget(missingCfg, "", true)
	if err == nil {
		t.Fatalf("resolveArchiveTarget(missing config, write=true) = (%q, %q, nil), want an error — "+
			"a write must never silently fall back to the public archive", url, passphrase)
	}
	if url != "" || passphrase != "" {
		t.Errorf("resolveArchiveTarget(missing config, write=true) returned non-empty (%q, %q) alongside an error", url, passphrase)
	}

	// Even an explicit -archive override cannot be trusted for a write when the
	// config (and so the passphrase) failed to resolve.
	url, passphrase, err = resolveArchiveTarget(missingCfg, "https://history.stellar.org/prd/core-live/core_live_001", true)
	if err == nil {
		t.Fatalf("resolveArchiveTarget(missing config, override, write=true) = (%q, %q, nil), want an error", url, passphrase)
	}
}

// TestResolveArchiveTarget_ReadStillFallsBackOnUnresolvedConfig pins the
// read-only behaviour the fix must preserve: state-snapshot's read path is
// documented to work without a config file, so write=false keeps falling
// back to the public archive rather than erroring.
func TestResolveArchiveTarget_ReadStillFallsBackOnUnresolvedConfig(t *testing.T) {
	t.Parallel()
	const missingCfg = "/nonexistent/stellarindex-config-does-not-exist.toml"

	url, passphrase, err := resolveArchiveTarget(missingCfg, "", false)
	if err != nil {
		t.Fatalf("resolveArchiveTarget(missing config, write=false) unexpected error: %v", err)
	}
	if url != defaultPubnetArchive {
		t.Errorf("resolveArchiveTarget(missing config, write=false) url = %q, want defaultPubnetArchive %q", url, defaultPubnetArchive)
	}
	if passphrase != defaultPubnetPassphrase {
		t.Errorf("resolveArchiveTarget(missing config, write=false) passphrase = %q, want defaultPubnetPassphrase %q", passphrase, defaultPubnetPassphrase)
	}
}

func TestWithinModWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		maxMod    uint32
		ledgerSeq uint32
		want      bool
	}{
		{0, 55_000_000, true},           // no bound → always in
		{0, 63_000_000, true},           // no bound → always in
		{62_000_000, 55_000_000, true},  // dormant tail → in
		{62_000_000, 62_000_000, false}, // at the floor → out (strictly below)
		{62_000_000, 63_000_000, false}, // above floor → out (already captured live)
	}
	for _, c := range cases {
		tally := &snapTally{maxModLedger: c.maxMod}
		if got := tally.withinModWindow(c.ledgerSeq); got != c.want {
			t.Errorf("withinModWindow(maxMod=%d, ls=%d) = %v, want %v", c.maxMod, c.ledgerSeq, got, c.want)
		}
	}
}
