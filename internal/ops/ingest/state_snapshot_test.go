package ingest

import (
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
// dormant-current-state fill: contract_data STORAGE entries (SAC
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

// TestResolveArchiveTarget_WriteRefusesUnresolvedConfig is the regression test:
// -write inserts the checkpoint straight into the target ClickHouse's
// ledger_entry_changes with no cross-check against the network that
// ClickHouse instance actually tracks. On a config load failure,
// resolveArchiveTarget must not log one stderr line and silently fall back to
// the public pubnet archive/passphrase — a stale or missing -config could
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

// A bare run is the bounded read-only tally; an explicit -dry-run still
// collects and prints the write set; -write wins over -dry-run.
func TestParseStateSnapshotFlags_Mode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args           []string
		write, collect bool
	}{
		{nil, false, false},
		{[]string{"-dry-run"}, false, true},
		{[]string{"-write"}, true, true},
		{[]string{"-write", "-dry-run"}, true, true},
	} {
		o, err := parseStateSnapshotFlags(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if o.write != tc.write || o.collect != tc.collect {
			t.Errorf("%v: write=%v collect=%v, want write=%v collect=%v", tc.args, o.write, o.collect, tc.write, tc.collect)
		}
	}
}
