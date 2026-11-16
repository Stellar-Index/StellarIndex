package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// A cold WASM fill (lake read plus two wabt processes, detached from its callers) takes a refresh-gate slot. With the class
// full it is refused before any lake read; a hash already filled is served without a slot.
func TestWasmModuleView_ColdFillTakesARefreshSlot(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		return nil, errors.New("the lake must not be read without a slot")
	}}
	r := &ExplorerReader{
		conn:        conn,
		refreshGate: NewRefreshGate(4),
		moduleCache: newWasmModuleCache(),
		disasmCache: newWasmDisasmCache(),
	}
	if !r.refreshGate.TryAcquireClass(wasmFillClass) {
		t.Fatal("could not fill the class")
	}

	cold := wasmHashN(1)
	if _, err := r.wasmModuleView(context.Background(), cold); !errors.Is(err, ErrRefreshSaturated) {
		t.Fatalf("cold fill with the class full: err = %v, want ErrRefreshSaturated", err)
	}
	if len(conn.queries) != 0 {
		t.Fatalf("a refused fill read the lake %d time(s)", len(conn.queries))
	}

	warm := wasmHashN(2)
	key := hex.EncodeToString(warm[:])
	r.moduleCache.put(key, wasmModuleEntry{code: []byte{0, 'a', 's', 'm'}}, time.Now())
	r.disasmCache.put(key, wasmDisasmEntry{wat: "(module)"}, time.Now())
	info, err := r.wasmModuleView(context.Background(), warm)
	if err != nil || info.Wat != "(module)" {
		t.Fatalf("a filled hash with the class full: info.Wat = %q, err = %v; want it served from the caches", info.Wat, err)
	}

	r.refreshGate.ReleaseClass(wasmFillClass)
	if _, err := r.wasmModuleView(context.Background(), cold); errors.Is(err, ErrRefreshSaturated) || len(conn.queries) == 0 {
		t.Fatalf("with a slot free the fill must reach the lake: err = %v, queries = %d", err, len(conn.queries))
	}
	if !r.refreshGate.TryAcquireClass(wasmFillClass) {
		t.Fatal("the fill did not return its slot")
	}
}
