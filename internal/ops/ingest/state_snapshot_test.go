package ingest

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// TestShouldCollect_ContractDataStorage: contract_data STORAGE entries are collected under scope=storage but not contracts or all, the gap that hid ~99% of PHO supply.
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

// TestResolveArchiveTarget_WriteRefusesUnresolvedConfig: with write=true an unresolved config must error, not fall back to the pubnet archive and write the wrong network's checkpoint.
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

// TestResolveArchiveTarget_ReadStillFallsBackOnUnresolvedConfig: the read path works without a config file and still falls back to the public archive.
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

// TestParseStateSnapshotFlags_Mode: a bare run is the bounded read-only tally, -dry-run still collects, and -write wins over -dry-run.
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

// recordingInserter stands in for clickhouse.InsertEntryChanges.
type recordingInserter struct{ calls, rows int }

func (r *recordingInserter) insert(_ context.Context, _ string, rows []clickhouse.LedgerEntryChangeRow, _ time.Duration) (int, error) {
	r.calls++
	r.rows += len(rows)
	return len(rows), nil
}

// ledgerEpoch is the close time datedLedgers gives ledger 0; ledger N closes N*5s later.
var ledgerEpoch = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

// datedLedgers stands in for clickhouse.LedgerCloseTimes over a lake that holds only the given ledgers.
func datedLedgers(held ...uint32) closeTimeLookup {
	return func(_ context.Context, _ string, seqs []uint32) (map[uint32]time.Time, error) {
		out := map[uint32]time.Time{}
		for _, want := range seqs {
			for _, h := range held {
				if h == want {
					out[want] = ledgerEpoch.Add(time.Duration(want) * 5 * time.Second)
				}
			}
		}
		return out, nil
	}
}

// A seeded row belongs to the ledger that last modified its entry, and the explorer serves the row's close time as the
// entry's date. Every row must carry that ledger's own close time, not the moment the operator ran the command.
func TestWriteSnapshot_StampsEachRowWithItsLedgersCloseTime(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 3, rows: []clickhouse.LedgerEntryChangeRow{{LedgerSeq: 100}, {LedgerSeq: 900}, {LedgerSeq: 100}}}
	var ins recordingInserter
	if err := writeSnapshot(context.Background(), ins.insert, datedLedgers(100, 900), tally, 0, "all", "127.0.0.1:9300", 0); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	for i, row := range tally.rows {
		want := ledgerEpoch.Add(time.Duration(row.LedgerSeq) * 5 * time.Second)
		if !row.CloseTime.Equal(want) {
			t.Errorf("row %d (ledger %d) close_time = %s, want that ledger's %s", i, row.LedgerSeq, row.CloseTime, want)
		}
	}
	if ins.calls != 1 || ins.rows != 3 {
		t.Fatalf("inserter calls=%d rows=%d, want 1 call with 3 rows", ins.calls, ins.rows)
	}
}

// A ledger the lake cannot date stops the write: no row reaches ClickHouse with a made-up date.
func TestWriteSnapshot_RefusesAnUndatedLedger(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 2, rows: []clickhouse.LedgerEntryChangeRow{{LedgerSeq: 100}, {LedgerSeq: 900}}}
	var ins recordingInserter
	err := writeSnapshot(context.Background(), ins.insert, datedLedgers(900), tally, 0, "all", "127.0.0.1:9300", 0)
	if err == nil || !strings.Contains(err.Error(), "1 of the 2 ledgers") || !strings.Contains(err.Error(), "lowest 100") {
		t.Fatalf("err = %v, want a refusal naming the undated ledger", err)
	}
	if ins.calls != 0 {
		t.Fatalf("inserter called %d time(s) after an undated ledger; want none", ins.calls)
	}
}

// The tally collects rows with no close time at all, so nothing but the lake lookup can date them.
func TestSnapTally_CollectsRowsUndated(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeOf(snapTally{}).FieldByName("closeTime"); ok {
		t.Fatal("snapTally carries a run-wide closeTime again; rows must be dated per ledger by stampCloseTimes")
	}
}

// TestWriteSnapshot_RefusesLimitTruncatedRead: the default -limit stops at 2M of ~48M entries, so -write must refuse before any row reaches ClickHouse.
func TestWriteSnapshot_RefusesLimitTruncatedRead(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 2_000_000, partial: true, rows: make([]clickhouse.LedgerEntryChangeRow, 3)}
	var ins recordingInserter
	err := writeSnapshot(context.Background(), ins.insert, datedLedgers(), tally, 2_000_000, "all", "127.0.0.1:9300", 0)
	if err == nil {
		t.Fatal("writeSnapshot accepted a -limit-truncated read")
	}
	if ins.calls != 0 {
		t.Fatalf("inserter called %d time(s) with %d row(s) from a partial read; want none", ins.calls, ins.rows)
	}
	if !strings.Contains(err.Error(), "-limit 0") {
		t.Fatalf("err = %v, want the -limit 0 remedy named", err)
	}
}

func TestWriteSnapshot_WritesCompleteRead(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 3, rows: make([]clickhouse.LedgerEntryChangeRow, 3)}
	var ins recordingInserter
	if err := writeSnapshot(context.Background(), ins.insert, datedLedgers(0), tally, 0, "all", "127.0.0.1:9300", 0); err != nil {
		t.Fatalf("complete read refused: %v", err)
	}
	if ins.calls != 1 || ins.rows != 3 {
		t.Fatalf("inserter calls=%d rows=%d, want 1 call with 3 rows", ins.calls, ins.rows)
	}
}
