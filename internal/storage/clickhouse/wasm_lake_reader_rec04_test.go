package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

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
