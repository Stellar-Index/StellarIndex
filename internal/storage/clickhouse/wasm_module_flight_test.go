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

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

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
