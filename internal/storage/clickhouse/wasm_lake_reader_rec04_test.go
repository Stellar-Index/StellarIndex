package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestContractWasmHash_PartialIndexMissFallsBackToLegacy is the REC-04
// residual regression (the instance-changes reader DATA-2 did not cover).
//
// instanceChangesIndexAvailable is a table-global `LIMIT 1` emptiness
// probe: it goes true as soon as the operator-applied
// stellar.contract_instance_changes holds ANY row — it cannot see whether
// the paired historical backfill has reached a PARTICULAR contract. So
// while the index is applied-but-still-backfilling, the per-contract
// lookup for a not-yet-reached contract returns zero rows even though the
// contract's executable is fully resolvable from current state.
//
// Pre-fix, contractWasmHash trusted that empty index result as an
// authoritative not-found (ok=false, err=nil) and ContractWasm turned it
// into ErrContractWasmUnresolved — a confidently-wrong "no wasm" 404 for a
// contract that DOES have code. The fix: only a POSITIVE index verdict (a
// resolved hash or a SAC verdict) short-circuits; an index MISS falls
// through to the legacy current-state read (the fallback source of truth),
// mirroring DATA-2's contract_active_ledgers empty-walk fallthrough.
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
