package explorer

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// barrier releases every caller only once n have arrived, so the reads can
// finish only if they are in flight at the same time. A serial caller never
// reaches n and hits the timeout.
type barrier struct {
	mu      sync.Mutex
	arrived int
	n       int
	open    chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, open: make(chan struct{})} }

func (b *barrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.open)
	}
	b.mu.Unlock()
	select {
	case <-b.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type sidecarReader struct {
	ExplorerReader
	b *barrier
}

func (r sidecarReader) ContractActivitySummaryFor(ctx context.Context, _ string, _ int) (clickhouse.ContractActivitySummary, bool, error) {
	if err := r.b.wait(ctx); err != nil {
		return clickhouse.ContractActivitySummary{}, false, err
	}
	return clickhouse.ContractActivitySummary{ActiveLedgersTotal: 1}, true, nil
}

func (r sidecarReader) ContractInstanceState(ctx context.Context, _ string) (clickhouse.ContractInstanceState, error) {
	if err := r.b.wait(ctx); err != nil {
		return clickhouse.ContractInstanceState{}, err
	}
	return clickhouse.ContractInstanceState{Known: true}, nil
}

type sidecarDirectory struct {
	DirectoryReader
	b *barrier
}

func (d sidecarDirectory) DirectoryEntryByAddress(ctx context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	if err := d.b.wait(ctx); err != nil {
		return timescale.DirectoryEntry{}, false, err
	}
	return timescale.DirectoryEntry{Name: "x"}, true, nil
}

type sidecarContracts struct {
	ContractsReader
	b *barrier
}

func (c sidecarContracts) ProtocolContractIndex(ctx context.Context) (map[string]string, error) {
	if err := c.b.wait(ctx); err != nil {
		return nil, err
	}
	return map[string]string{"CX": "blend"}, nil
}

// The four contract-page reads must overlap (latency = max, not sum) and
// each must keep its own result and error flag.
func TestReadContractSidecars_RunConcurrently(t *testing.T) {
	b := newBarrier(4)
	h := &Handler{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reader:            sidecarReader{b: b},
		Directory:         sidecarDirectory{b: b},
		ProtocolContracts: sidecarContracts{b: b},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s := h.readContractSidecars(ctx, "CX")

	if s.protocol != "blend" || s.directory == nil || !s.dirOK || s.activity == nil || !s.instOK || !s.inst.Known {
		t.Fatalf("reads did not all complete with their own results (serial execution times out at the barrier): %+v", s)
	}
}

// A cancelled context surfaces as per-read failure flags, never a hang.
func TestReadContractSidecars_CancelledContext(t *testing.T) {
	b := newBarrier(5) // never opens
	h := &Handler{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reader:            sidecarReader{b: b},
		Directory:         sidecarDirectory{b: b},
		ProtocolContracts: sidecarContracts{b: b},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	s := h.readContractSidecars(ctx, "CX")

	if s.dirOK || s.instOK || s.activity != nil || s.protocol != "" {
		t.Fatalf("cancelled reads must report failure: %+v", s)
	}
}

type panicDirectory struct{ DirectoryReader }

func (panicDirectory) DirectoryEntryByAddress(context.Context, string) (timescale.DirectoryEntry, bool, error) {
	panic("directory reader bug")
}

// One panicking read must degrade only its own field and not kill the process.
func TestReadContractSidecars_PanicDegradesOneRead(t *testing.T) {
	b := newBarrier(3) // the three healthy reads
	h := &Handler{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reader:            sidecarReader{b: b},
		Directory:         panicDirectory{},
		ProtocolContracts: sidecarContracts{b: b},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s := h.readContractSidecars(ctx, "CX")

	if s.dirOK || s.directory != nil {
		t.Fatalf("panicking directory read must report unavailable: %+v", s)
	}
	if s.protocol != "blend" || s.activity == nil || !s.instOK {
		t.Fatalf("healthy reads must be unaffected: %+v", s)
	}
}
