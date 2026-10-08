package clickhouse

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestContractWasmHash_PartialIndexMissFallsBackToLegacy is the
// residual regression (the instance-changes reader the first fix missed).
//
// instanceChangesIndexAvailable is a table-global `LIMIT 1` emptiness
// probe: it goes true as soon as the operator-applied
// stellar.contract_instance_changes holds ANY row — it cannot see whether
// the paired historical backfill has reached a PARTICULAR contract. So
// while the index is applied-but-still-backfilling, the per-contract
// lookup for a not-yet-reached contract returns zero rows even though the
// contract's executable is fully resolvable from current state.
//
// Without the fallback, contractWasmHash would trust that empty index result as an
// authoritative not-found (ok=false, err=nil) and ContractWasm would turn it
// into ErrContractWasmUnresolved — a confidently-wrong "no wasm" 404 for a
// contract that DOES have code. So only a POSITIVE index verdict (a
// resolved hash or a SAC verdict) short-circuits; an index MISS falls
// through to the legacy current-state read (the fallback source of truth),
// mirroring the contract_active_ledgers empty-walk fallthrough.
//
// Proven red: revert the `(err == nil && ok)` guard in contractWasmHash
// back to `err == nil` and this test fails — the reader returns ok=false
// and never issues the ledger_entries_current read (asserted below).
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

// TestContractCodeHistory_PartialIndexMissFallsBackToLegacy is the
// sibling gap: ContractCodeHistory trusted an EMPTY per-contract
// result from contract_instance_changes as an authoritative "never
// upgraded", even though instanceChangesIndexAvailable is the same
// table-global LIMIT-1 emptiness probe that cannot see partial per-contract
// backfill coverage. Only contractWasmHash guarded against this; this read
// did not.
//
// Without the fallback, an applied-but-still-backfilling index would make ContractCodeHistory
// return an empty timeline for any contract the backfill hadn't reached
// yet, even though the changes log holds its real upgrade history. The
// fallback mirrors contractWasmHash: only a NON-EMPTY indexed result is
// trusted; an empty one falls through to the legacy changes-log scan.
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
