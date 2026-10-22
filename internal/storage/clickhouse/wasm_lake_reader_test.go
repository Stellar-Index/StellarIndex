package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Regression tests: ContractCodeHistory's read of stellar.ledger_entry_changes must be LIMITed, since an
// unbounded one decodes every change to a frequently rewritten instance key. stubConn asserts query shape
// only; TestContractCodeHistory_CollapsesToDistinctExecutables pins the decoded output.

// testContractID is a well-formed C-strkey (the same one the explorer handler
// tests use), so strkey.Decode succeeds and the reader reaches its query.
const testContractID = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"

// instanceEntryB64 builds the base64 LedgerEntry the lake stores for a
// contract-instance contract_data change pointing at wasmHash — the exact shape
// ContractCodeHistory decodes.
func instanceEntryB64(t *testing.T, wasmHash xdr.Hash) string {
	t.Helper()
	var cidRaw xdr.Hash
	copy(cidRaw[:], []byte("contract-id-32-bytes-padding----"))
	cid := xdr.ContractId(cidRaw)
	hash := wasmHash
	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{
			Type:     xdr.ContractExecutableTypeContractExecutableWasm,
			WasmHash: &hash,
		},
	}
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return b64
}

func wasmHashN(n byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = n
	}
	return h
}

// TestContractCodeHistory_BoundsTheScan pins the cap. Against the un-fixed
// reader the emitted SQL carries no LIMIT at all and the only bound arg is the
// key list, so both assertions below fail.
func TestContractCodeHistory_BoundsTheScan(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.contract_instance_changes") {
			// Index probe: no rows → unavailable → legacy scan path.
			return &stubRows{}, nil
		}
		if !strings.Contains(q, "FROM stellar.ledger_entry_changes") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	// Query 0 is the instance-index probe; the legacy scan is the last.
	q := conn.queries[len(conn.queries)-1]
	args := conn.args[len(conn.args)-1]
	if !strings.Contains(q, "FROM stellar.ledger_entry_changes") {
		t.Fatalf("last query = %q, want the legacy changes scan", q)
	}

	if !strings.Contains(q, "LIMIT ?") {
		t.Fatalf("query = %q, want a bound `LIMIT ?` — an unbounded instance-change "+
			"scan is exactly what C-F1 item 3 removed", q)
	}
	// The cap must be the one the code declares, passed as a bound arg after
	// the key list.
	if len(args) != 2 {
		t.Fatalf("query args = %v, want [keys, limit]", args)
	}
	got, ok := args[1].(int)
	if !ok || got != contractCodeHistoryMaxRows {
		t.Fatalf("limit arg = %v (%T), want %d", args[1], args[1], contractCodeHistoryMaxRows)
	}
	if contractCodeHistoryMaxRows <= 0 || contractCodeHistoryMaxRows > 100_000 {
		t.Fatalf("contractCodeHistoryMaxRows = %d, want a positive cap well under a "+
			"pathological scan", contractCodeHistoryMaxRows)
	}

	// Truncation direction matters: the cap is taken NEWEST-first and re-sorted
	// ascending for the caller, so a truncated timeline keeps the CURRENT
	// executable and drops the oldest tail — never the reverse.
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC, ingested_at DESC") {
		t.Fatalf("query = %q, want the capped inner select ordered newest-first", q)
	}
	inner := strings.Index(q, "ORDER BY ledger_seq DESC")
	outer := strings.Index(q, "ORDER BY ledger_seq ASC")
	if inner < 0 || outer < 0 || outer < inner {
		t.Fatalf("query = %q, want the newest-first LIMIT applied INSIDE and the "+
			"result re-sorted ascending outside", q)
	}
	if lim := strings.Index(q, "LIMIT ?"); lim < inner || lim > outer {
		t.Fatalf("query = %q, want the LIMIT inside the newest-first subquery", q)
	}
}

// TestContractCodeHistory_CollapsesToDistinctExecutables pins the decoded output: consecutive identical
// wasm hashes collapse to one version, a change back to an earlier hash is a NEW version, and each carries
// its change ledger + close time.
func TestContractCodeHistory_CollapsesToDistinctExecutables(t *testing.T) {
	hashA, hashB := wasmHashN(0xAA), wasmHashN(0xBB)
	entryA, entryB := instanceEntryB64(t, hashA), instanceEntryB64(t, hashB)
	base := time.Unix(1700000000, 0).UTC()

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "stellar.contract_instance_changes") {
			return &stubRows{}, nil // index unavailable → legacy path
		}
		// Ascending, as the query's outer ORDER BY delivers them:
		// A, A (no-op rewrite), B (upgrade), A (rollback).
		return &stubRows{data: [][]any{
			{uint32(100), base, entryA},
			{uint32(101), base.Add(5 * time.Second), entryA},
			{uint32(102), base.Add(10 * time.Second), entryB},
			{uint32(103), base.Add(15 * time.Second), entryA},
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []ContractCodeVersion{
		{Ledger: 100, CloseTime: base, WasmHash: hex.EncodeToString(hashA[:])},
		{Ledger: 102, CloseTime: base.Add(10 * time.Second), WasmHash: hex.EncodeToString(hashB[:])},
		{Ledger: 103, CloseTime: base.Add(15 * time.Second), WasmHash: hex.EncodeToString(hashA[:])},
	}
	if len(got) != len(want) {
		t.Fatalf("versions = %+v, want %d entries (%+v)", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestContractCodeHistory_IndexedPath pins the fast path: with a usable index the reader walks the keyed
// contract_instance_changes timeline (wasm-only, newest-first cap) and never touches the changes log.
func TestContractCodeHistory_IndexedPath(t *testing.T) {
	rawA, rawB := wasmHashN(0xAA), wasmHashN(0xBB)
	hashA := hex.EncodeToString(rawA[:])
	hashB := hex.EncodeToString(rawB[:])
	base := time.Unix(1700000000, 0).UTC()

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "FROM stellar.ledger_entry_changes") {
			t.Fatalf("indexed path must not touch the changes log: %s", q)
		}
		if strings.Contains(q, "LIMIT 1") { // probe: one row → usable
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{
			{uint32(100), base, hashA},
			{uint32(101), base.Add(5 * time.Second), hashA},
			{uint32(102), base.Add(10 * time.Second), hashB},
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	want := []ContractCodeVersion{
		{Ledger: 100, CloseTime: base, WasmHash: hashA},
		{Ledger: 102, CloseTime: base.Add(10 * time.Second), WasmHash: hashB},
	}
	if len(got) != len(want) {
		t.Fatalf("versions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	q := conn.queries[len(conn.queries)-1]
	for _, must := range []string{
		"FROM stellar.contract_instance_changes",
		"WHERE contract_hash = ?",
		"is_sac = 0",
		"ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC",
		"LIMIT ?",
	} {
		if !strings.Contains(q, must) {
			t.Fatalf("indexed query = %q, missing %q", q, must)
		}
	}
	args := conn.args[len(conn.args)-1]
	if len(args) != 2 {
		t.Fatalf("indexed query args = %v, want [contract_hash, limit]", args)
	}
	if lim, ok := args[1].(int); !ok || lim != contractCodeHistoryMaxRows {
		t.Fatalf("limit arg = %v, want %d", args[1], contractCodeHistoryMaxRows)
	}
}

// indexMissStub serves a usable instance index holding no timeline rows for
// this contract; legacy is what the changes-log scan returns.
func indexMissStub(legacy [][]any) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
			return &stubRows{data: legacy}, nil
		case strings.Contains(q, "contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		default: // the indexed timeline read
			return &stubRows{}, nil
		}
	}
	return conn
}

// TestContractCodeHistory_IndexMissFallsBackToLegacyScan pins that an index with no row for the contract is
// not proof it never upgraded (the probe is table-global), so the reader falls back to the changes-log scan.
func TestContractCodeHistory_IndexMissFallsBackToLegacyScan(t *testing.T) {
	conn := indexMissStub(nil)
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("versions = %+v, want none when both the index and the legacy scan are empty", got)
	}
	var sawTimeline, sawLegacy bool
	for _, q := range conn.queries {
		sawTimeline = sawTimeline || strings.Contains(q, "WHERE contract_hash = ? AND is_sac = 0")
		sawLegacy = sawLegacy || strings.Contains(q, "FROM stellar.ledger_entry_changes")
	}
	if !sawTimeline {
		t.Fatalf("queries = %q, want the indexed timeline read", conn.queries)
	}
	if !sawLegacy {
		t.Fatalf("queries = %q, want the legacy scan after an unproven index miss", conn.queries)
	}
}

// TestContractCodeHistory_SACInIndexSkipsLegacyScan: a SAC's index rows are all is_sac = 1, so its wasm
// timeline is empty yet authoritative on both key shapes; the changes-log scan it would fall into exceeds the
// read budget.
func TestContractCodeHistory_SACInIndexSkipsLegacyScan(t *testing.T) {
	noColumn := &clickhouse.Exception{
		Code: 47, Name: "UNKNOWN_IDENTIFIER",
		Message: "Missing columns: 'intra_ledger_seq' 'tx_hash'",
	}
	for name, keyShapeErr := range map[string]error{"tx-keyed": nil, "old-key": noColumn} {
		t.Run(name, func(t *testing.T) {
			var timelineArgs, presenceArgs []any
			conn := &stubConn{}
			conn.respond = func(q string) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
					t.Fatalf("a contract present in the index fell through to the legacy scan: %s", q)
					return nil, nil
				case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1"):
					if keyShapeErr != nil {
						return nil, keyShapeErr
					}
					return &stubRows{}, nil
				case strings.Contains(q, "contract_instance_changes LIMIT 1"): // availability probe
					return &stubRows{data: [][]any{{uint32(1)}}}, nil
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
					return &stubRows{}, nil // no wasm rows
				default: // per-contract presence read: the SAC's rows exist
					return &stubRows{data: [][]any{{uint8(1)}}}, nil
				}
			}
			r := &ExplorerReader{conn: conn}

			got, err := r.ContractCodeHistory(context.Background(), testContractID)
			if err != nil {
				t.Fatalf("ContractCodeHistory: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("versions = %+v, want none for a SAC", got)
			}
			for i, q := range conn.queries {
				switch {
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
					timelineArgs = conn.args[i]
				case strings.Contains(q, "WHERE contract_hash = ?"):
					presenceArgs = conn.args[i]
					if strings.Contains(q, "is_sac") || strings.Contains(q, "intra_ledger_seq") {
						t.Fatalf("presence read must name only contract_hash: %s", q)
					}
				}
			}
			if len(timelineArgs) == 0 || len(presenceArgs) != 1 || presenceArgs[0] != timelineArgs[0] {
				t.Fatalf("presence read args = %v, want [%v]", presenceArgs, timelineArgs)
			}
		})
	}
}

// TestContractCodeHistory_PartialIndexMissFallsBackToLegacy: an empty per-contract result from
// contract_instance_changes is not an authoritative "never upgraded", since the table-global LIMIT-1 probe
// cannot see partial backfill coverage. As in contractWasmHash, only a non-empty indexed result is trusted.
func TestContractCodeHistory_PartialIndexMissFallsBackToLegacy(t *testing.T) {
	wantHash := wasmHashN(0xEF)

	var legacyRead bool
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes"):
			// Key-shape probe: the tx-keyed table.
			return &stubRows{}, nil
		case strings.Contains(q, "contract_instance_changes") && strings.Contains(q, "SELECT ledger_seq FROM"):
			// Availability probe: the index EXISTS and is non-empty
			// (some other contract has been backfilled) -> "usable".
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("):
			// Per-contract lookup: THIS contract's instance history has not
			// been backfilled yet -> zero rows (a PARTIAL-coverage miss,
			// invisible to the probe).
			return &stubRows{}, nil
		case strings.Contains(q, "SELECT 1 FROM stellar.contract_instance_changes"):
			// Per-contract presence read: no row, the backfill has not
			// reached this contract.
			return &stubRows{}, nil
		case strings.Contains(q, "stellar.entry_history_watermark"):
			// No genesis watermark: the miss is unproven.
			return &stubRows{}, nil
		case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
			// Legacy changes-log scan resolves the real upgrade history.
			legacyRead = true
			return &stubRows{data: [][]any{{uint32(1), time.Unix(0, 0).UTC(), instanceEntryB64(t, wantHash)}}}, nil
		default:
			t.Fatalf("unexpected query: %s", q)
			return nil, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.ContractCodeHistory(context.Background(), testContractID)
	if err != nil {
		t.Fatalf("ContractCodeHistory returned error: %v", err)
	}
	if !legacyRead {
		t.Fatal("the legacy ledger_entry_changes read was never issued: the empty indexed " +
			"result was served as an authoritative 'never upgraded' instead of falling back")
	}
	if len(got) != 1 || got[0].WasmHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("history = %+v, want one version with hash %x from the legacy scan", got, wantHash)
	}
}

// TestContractCodeHistory_GenesisWatermark: an index miss skips the
// ledger_entry_changes scan only when a genesis watermark covers ledger 1.
func TestContractCodeHistory_GenesisWatermark(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wm       [][]any
		wantScan bool
	}{
		{"present and covering", [][]any{{uint32(500)}}, false},
		{"absent", nil, true},
		{"zero (below any ledger)", [][]any{{uint32(0)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scanned bool
			conn := &stubConn{}
			conn.respond = func(q string) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes"):
					return &stubRows{}, nil
				case strings.Contains(q, "SELECT ledger_seq FROM"):
					return &stubRows{data: [][]any{{uint32(1)}}}, nil
				case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash FROM ("),
					strings.Contains(q, "SELECT 1 FROM stellar.contract_instance_changes"):
					return &stubRows{}, nil
				case strings.Contains(q, "stellar.entry_history_watermark"):
					return &stubRows{data: tc.wm}, nil
				case strings.Contains(q, "FROM stellar.ledger_entry_changes"):
					scanned = true
					return &stubRows{}, nil
				default:
					t.Fatalf("unexpected query: %s", q)
					return nil, nil
				}
			}
			r := newExplorerReader(conn)
			if _, err := r.ContractCodeHistory(context.Background(), testContractID); err != nil {
				t.Fatal(err)
			}
			if scanned != tc.wantScan {
				t.Fatalf("legacy scan = %v, want %v", scanned, tc.wantScan)
			}
		})
	}
}

// TestContractWasmHash_PartialIndexMissFallsBackToLegacy: the instance-changes index probe is table-global,
// so a MISS for a not-yet-backfilled contract must fall through to the legacy current-state read, not a false
// 404. Only a positive index verdict short-circuits.
func TestContractWasmHash_PartialIndexMissFallsBackToLegacy(t *testing.T) {
	wantHash := wasmHashN(0xCD)

	var legacyRead bool
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contract_instance_changes") && strings.Contains(q, "SELECT ledger_seq"):
			// Availability probe: the index EXISTS and is non-empty
			// (some other contract has been backfilled) -> "usable".
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes"):
			// Key-shape probe: the tx-keyed table.
			return &stubRows{}, nil
		case strings.Contains(q, "contract_instance_changes") && strings.Contains(q, "is_sac"):
			// Per-contract lookup: THIS contract's instance write has not
			// been backfilled yet -> zero rows (a PARTIAL-coverage miss,
			// invisible to the probe).
			return &stubRows{}, nil
		case strings.Contains(q, "ledger_entries_current"):
			// Legacy current-state read resolves the executable.
			legacyRead = true
			return &stubRows{data: [][]any{{instanceEntryB64(t, wantHash)}}}, nil
		default:
			t.Fatalf("unexpected query: %s", q)
			return nil, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	var cid [32]byte
	copy(cid[:], []byte("contract-id-32-bytes-padding----"))

	got, ok, err := r.contractWasmHash(context.Background(), cid)
	if err != nil {
		t.Fatalf("contractWasmHash returned error: %v", err)
	}
	if !ok {
		t.Fatal("contractWasmHash returned ok=false on a partial-index miss: an empty " +
			"per-contract walk against an applied-but-still-backfilling index was trusted " +
			"as authoritative 'no wasm' (REC-04) instead of falling through to the legacy read")
	}
	if got != wantHash {
		t.Fatalf("resolved hash = %x, want %x (must come from the legacy current-state read)", got, wantHash)
	}
	if !legacyRead {
		t.Fatal("the legacy ledger_entries_current read was never issued: the index miss " +
			"was not treated as 'unknown, fall back'")
	}
}

func TestInstanceGenesisCovers_Below(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) { return &stubRows{data: [][]any{{uint32(100)}}}, nil }
	r := newExplorerReader(conn)
	if !r.instanceGenesisCovers(context.Background(), 100) {
		t.Error("ledger at watermark must be covered")
	}
	if r.instanceGenesisCovers(context.Background(), 101) {
		t.Error("ledger above watermark must not be covered")
	}
}

// TestContractWasmHash_IndexedResolvesPreCaptureContract pins hop 1 of the wasm lookup: a contract whose
// instance entry predates live capture resolves its executable (and SAC verdict) from the genesis-complete
// contract_instance_changes index, without touching ledger_entries_current.
func TestContractWasmHash_IndexedResolvesPreCaptureContract(t *testing.T) {
	wantHash := wasmHashN(0xCD)
	wantHex := hex.EncodeToString(wantHash[:])

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "ledger_entries_current"):
			t.Fatalf("indexed path must not read ledger_entries_current: %s", q)
			return nil, nil
		case strings.Contains(q, "contract_instance_changes LIMIT 1"): // probe
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		default: // indexed hash lookup
			return &stubRows{data: [][]any{{uint8(0), wantHex}}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	var cid xdr.Hash
	copy(cid[:], []byte("contract-id-32-bytes-padding----"))
	got, ok, err := r.contractWasmHash(context.Background(), cid)
	if err != nil || !ok {
		t.Fatalf("contractWasmHash: ok=%v err=%v", ok, err)
	}
	if got != wantHash {
		t.Fatalf("hash = %x, want %x", got, wantHash)
	}

	// SAC verdict is authoritative from the index (no legacy fallback).
	conn2 := &stubConn{}
	conn2.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "ledger_entries_current"):
			t.Fatalf("SAC verdict must not fall through to legacy: %s", q)
			return nil, nil
		case strings.Contains(q, "contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		default:
			return &stubRows{data: [][]any{{uint8(1), ""}}}, nil
		}
	}
	r2 := &ExplorerReader{conn: conn2}
	if _, _, err := r2.contractWasmHash(context.Background(), cid); !errors.Is(err, ErrContractIsSAC) {
		t.Fatalf("SAC row: err = %v, want ErrContractIsSAC", err)
	}
}

// replayStub answers ReplayCodeHistory's reads: the genesis watermark, the
// tx-key probe, the indexed timeline and the latest-instance lookup.
type replayStub struct {
	watermark    []any // nil = no row
	watermarkErr error
	timeline     [][]any
	latest       [][]any
}

func (s replayStub) conn(t *testing.T) *stubConn {
	t.Helper()
	c := &stubConn{}
	c.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "entry_history_watermark"):
			if s.watermarkErr != nil {
				return nil, s.watermarkErr
			}
			if s.watermark == nil {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{s.watermark}}, nil
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{"", uint32(0)}}}, nil
		case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash"):
			return &stubRows{data: s.timeline}, nil
		case strings.Contains(q, "SELECT is_sac, wasm_hash"):
			return &stubRows{data: s.latest}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	return c
}

func TestReplayCodeHistory(t *testing.T) {
	h := wasmHashN(0xAB)
	hashHex := hex.EncodeToString(h[:])
	base := time.Unix(1700000000, 0).UTC()
	oneVersion := [][]any{{uint32(100), base, hashHex}}

	t.Run("watermark 0 refuses", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(0)}, timeline: oneVersion}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrInstanceHistoryIncomplete) {
			t.Fatalf("err = %v, want ErrInstanceHistoryIncomplete", err)
		}
		for _, q := range c.queries {
			if strings.Contains(q, "wasm_hash") {
				t.Fatalf("read a timeline without the genesis watermark: %s", q)
			}
		}
	})
	t.Run("watermark read error refuses", func(t *testing.T) {
		boom := errors.New("boom")
		c := replayStub{watermarkErr: boom, timeline: oneVersion}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the watermark read error", err)
		}
	})
	t.Run("versions served", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}, timeline: oneVersion}.conn(t)
		got, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if err != nil || len(got) != 1 || got[0].WasmHash != hashHex || got[0].Ledger != 100 {
			t.Fatalf("got %+v, %v; want one version %s at 100", got, err, hashHex)
		}
	})
	t.Run("truncated refuses", func(t *testing.T) {
		rows := make([][]any, contractCodeHistoryMaxRows)
		for i := range rows {
			hi := wasmHashN(byte(i % 2))
			rows[i] = []any{uint32(i + 1), base, hex.EncodeToString(hi[:])}
		}
		c := replayStub{watermark: []any{uint32(60_000_000)}, timeline: rows}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrCodeHistoryTruncated) {
			t.Fatalf("err = %v, want ErrCodeHistoryTruncated", err)
		}
	})
	t.Run("no instance rows refuses", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrContractWasmUnresolved) {
			t.Fatalf("err = %v, want ErrContractWasmUnresolved", err)
		}
	})
	t.Run("SAC", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}, latest: [][]any{{uint8(1), ""}}}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrContractIsSAC) {
			t.Fatalf("err = %v, want ErrContractIsSAC", err)
		}
	})
}

// wasmFlightConn is a concurrency-safe stub serving the indexed contract→hash
// hop and the contract_code read, counting code reads and holding each one
// until release closes.
type wasmFlightConn struct {
	driver.Conn
	hashHex   string
	codeB64   string
	release   chan struct{}
	hashReads atomic.Int32
	codeReads atomic.Int32
}

func (c *wasmFlightConn) Query(ctx context.Context, q string, _ ...any) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "entry_type = 'contract_code'"):
		c.codeReads.Add(1)
		select {
		case <-c.release:
			return &stubRows{data: [][]any{{c.codeB64}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case strings.Contains(q, "ledger_entries_current"):
		return nil, errors.New("unexpected legacy hash read: " + q)
	case strings.Contains(q, "contract_instance_changes LIMIT 1"): // probe
		return &stubRows{data: [][]any{{uint32(1)}}}, nil
	default: // indexed contract→hash lookup
		c.hashReads.Add(1)
		return &stubRows{data: [][]any{{uint8(0), c.hashHex}}}, nil
	}
}

func newWasmFlightConn(t *testing.T) *wasmFlightConn {
	t.Helper()
	hash := wasmHashN(0x5A)
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: hash, Code: contractRegisterWasm},
	}}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal contract_code entry: %v", err)
	}
	return &wasmFlightConn{hashHex: hex.EncodeToString(hash[:]), codeB64: b64, release: make(chan struct{})}
}

// TestContractWasm_ConcurrentColdRequestsShareOneCodeRead: N concurrent cold
// requests for one wasm hash cost one lake blob read (and so one wabt run),
// and a later request costs none.
func TestContractWasm_ConcurrentColdRequestsShareOneCodeRead(t *testing.T) {
	const n = 8
	conn := newWasmFlightConn(t)
	r := &ExplorerReader{conn: conn, disasmCache: newWasmDisasmCache(), moduleCache: newWasmModuleCache()}

	var wg sync.WaitGroup
	infos := make([]ContractWasmInfo, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			infos[i], errs[i] = r.ContractWasm(context.Background(), testContractID)
		}()
	}
	// Every caller has passed the contract→hash hop before the blob read is
	// released; a straggler that misses the flight finds the memo instead.
	deadline := time.Now().Add(5 * time.Second)
	for conn.hashReads.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(conn.release)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if infos[i].ContractID != testContractID || infos[i].WasmHash != conn.hashHex ||
			infos[i].SizeBytes != len(contractRegisterWasm) || len(infos[i].Exports) == 0 {
			t.Fatalf("caller %d: info = %+v, want the assembled module view", i, infos[i])
		}
	}
	if got := conn.codeReads.Load(); got != 1 {
		t.Fatalf("contract_code reads = %d for %d concurrent requests, want 1", got, n)
	}

	if _, err := r.ContractWasm(context.Background(), testContractID); err != nil {
		t.Fatalf("warm request: %v", err)
	}
	if got := conn.codeReads.Load(); got != 1 {
		t.Fatalf("contract_code reads = %d after a warm request, want still 1", got)
	}
}

// TestContractWasm_WaiterDeadlineDoesNotCancelFill: a caller that gives up
// returns its own ctx error, and the shared fill still lands in the memo.
func TestContractWasm_WaiterDeadlineDoesNotCancelFill(t *testing.T) {
	conn := newWasmFlightConn(t)
	r := &ExplorerReader{conn: conn, disasmCache: newWasmDisasmCache(), moduleCache: newWasmModuleCache()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.ContractWasm(ctx, testContractID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out caller: err = %v, want DeadlineExceeded", err)
	}
	close(conn.release)

	info, err := r.ContractWasm(context.Background(), testContractID)
	if err != nil || info.SizeBytes != len(contractRegisterWasm) {
		t.Fatalf("follow-up: info.SizeBytes=%d err=%v", info.SizeBytes, err)
	}
	if got := conn.codeReads.Load(); got != 1 {
		t.Fatalf("contract_code reads = %d, want 1 (the abandoned fill completed and was reused)", got)
	}
}

func sacInstanceEntryB64(t *testing.T, name string) string {
	t.Helper()
	var cidRaw xdr.Hash
	cid := xdr.ContractId(cidRaw)
	nameSym, metaSym, str := xdr.ScSymbol("name"), xdr.ScSymbol("METADATA"), xdr.ScString(name)
	meta := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &nameSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str},
	}}
	metaPtr := &meta
	storage := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &metaSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &metaPtr},
	}}
	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
		Storage:    &storage,
	}
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
		},
	}}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal SAC instance entry: %v", err)
	}
	return b64
}

// TestSACClassicAssetName_MemoisesOnlyProvenVerdicts pins that a WASM or SAC instance is read once per contract, while a contract
// with no captured instance is read again, since the lake may yet capture a SAC for it.
func TestSACClassicAssetName_MemoisesOnlyProvenVerdicts(t *testing.T) {
	cases := map[string]struct {
		rows     [][]any
		wantName string
		wantSAC  bool
		reads    int
	}{
		"wasm":        {[][]any{{instanceEntryB64(t, wasmHashN(1))}}, "", false, 1},
		"sac":         {[][]any{{sacInstanceEntryB64(t, "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")}}, "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", true, 1},
		"no instance": {nil, "", false, 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{data: tc.rows}, nil }}
			r := &ExplorerReader{conn: conn}
			for range 3 {
				got, sac, err := r.SACClassicAssetName(context.Background(), testContractID)
				if err != nil || got != tc.wantName || sac != tc.wantSAC {
					t.Fatalf("SACClassicAssetName = %q, %v, %v; want %q, %v, nil", got, sac, err, tc.wantName, tc.wantSAC)
				}
			}
			if len(conn.queries) != tc.reads {
				t.Fatalf("lake reads = %d, want %d", len(conn.queries), tc.reads)
			}
		})
	}
}
