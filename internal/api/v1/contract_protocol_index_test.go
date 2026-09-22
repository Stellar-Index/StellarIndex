package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const contractIndexTestPool = "CAQUARIUSPOOL000000000000000000000000000000000000000000000"

// rosterStub answers the aquarius roster with one pool and every other
// source with nothing. It fails a read whenever the ctx it is handed is
// already dead, and for the first failRounds full walks regardless — the
// two ways a build comes back incomplete in production.
type rosterStub struct {
	failRounds int32
	panicFirst bool
	calls      atomic.Int32
	rounds     atomic.Int32
}

func (r *rosterStub) ListProtocolContracts(ctx context.Context, source string) ([]timescale.ProtocolContract, error) {
	r.calls.Add(1)
	if source == protocolRegistry[0].Name {
		r.rounds.Add(1)
	}
	if r.panicFirst && r.rounds.Load() == 1 {
		panic("registry roster read panicked")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if r.rounds.Load() <= r.failRounds {
		return nil, errors.New("registry unavailable")
	}
	if source == "aquarius" {
		return []timescale.ProtocolContract{{Source: source, ContractID: contractIndexTestPool}}, nil
	}
	return nil, nil
}

func (r *rosterStub) ListSourceContractsFromProjection(ctx context.Context, _ string) ([]string, error) {
	return nil, ctx.Err()
}

func (r *rosterStub) ProtocolContractIndex(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func newContractIndexServer(stub *rosterStub) *Server {
	return &Server{
		logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		protocolContractsReader: stub,
	}
}

// A request whose budget is already spent must not build — and cache —
// a statics-only map for everyone else: the build runs on its own
// deadline, so the pool is labelled on that very call and the map is
// complete.
func TestContractProtocolIndex_BuildIsDetachedFromTheRequestDeadline(t *testing.T) {
	stub := &rosterStub{}
	s := newContractIndexServer(stub)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if name, ok := s.contractProtocol(dead, contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label under a dead request ctx = %q/%v, want aquarius/true", name, ok)
	}
	s.contractIndex.mu.Lock()
	complete := s.contractIndex.complete
	s.contractIndex.mu.Unlock()
	if !complete {
		t.Fatal("a build every source answered must be recorded complete")
	}
	if name, ok := s.contractProtocol(context.Background(), contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label on the next live request = %q/%v, want aquarius/true", name, ok)
	}
}

// A build that lost a roster source is served (the statics still label)
// but never earns the long TTL: it is retried once the short retry TTL
// lapses, and the retry that succeeds labels.
func TestContractProtocolIndex_IncompleteBuildRetriesInsteadOfCachingForTenMinutes(t *testing.T) {
	stub := &rosterStub{failRounds: 1}
	s := newContractIndexServer(stub)
	ctx := context.Background()

	if name, ok := s.contractProtocol(ctx, contractIndexTestPool); ok {
		t.Fatalf("first build (registry down) labelled %q; the stub answered nothing", name)
	}
	for _, meta := range protocolRegistry {
		if len(meta.Factories) == 0 {
			continue
		}
		if name, ok := s.contractProtocol(ctx, meta.Factories[0]); !ok || name != meta.Name {
			t.Fatalf("the statics must still label while the registry is down: %s factory = %q/%v", meta.Name, name, ok)
		}
		break
	}
	s.contractIndex.mu.Lock()
	if s.contractIndex.complete {
		t.Fatal("a build with failed sources must not be recorded complete")
	}
	if s.contractIndex.byID == nil {
		t.Fatal("the partial map must be served, not discarded")
	}
	// Still inside the retry TTL: served as is, no rebuild.
	s.contractIndex.mu.Unlock()
	before := stub.calls.Load()
	s.contractProtocol(ctx, contractIndexTestPool)
	if got := stub.calls.Load(); got != before {
		t.Fatalf("a rebuild ran inside the retry TTL (%d → %d registry reads)", before, got)
	}
	// Past the retry TTL but well inside the long TTL: it must rebuild.
	s.contractIndex.mu.Lock()
	s.contractIndex.built = time.Now().Add(-contractProtocolIndexRetryTTL - time.Second)
	s.contractIndex.mu.Unlock()
	if name, ok := s.contractProtocol(ctx, contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label after the retry = %q/%v, want aquarius/true", name, ok)
	}
	s.contractIndex.mu.Lock()
	defer s.contractIndex.mu.Unlock()
	if !s.contractIndex.complete {
		t.Fatal("the successful retry must be recorded complete")
	}
}

// A panic inside the build must not leave inFlight stuck true: the next
// call must still retry the build instead of serving the stale/nil map
// forever.
func TestContractProtocolIndex_PanicDuringBuildClearsInFlight(t *testing.T) {
	stub := &rosterStub{panicFirst: true}
	s := newContractIndexServer(stub)
	ctx := context.Background()

	func() {
		defer func() { _ = recover() }()
		s.contractProtocolIndexFor(ctx)
	}()

	s.contractIndex.mu.Lock()
	inFlight := s.contractIndex.inFlight
	s.contractIndex.mu.Unlock()
	if inFlight {
		t.Fatal("inFlight left true after a panic in the build — every later call would be stuck serving the stale/nil map")
	}

	if name, ok := s.contractProtocol(ctx, contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label after the panicking build recovered = %q/%v, want aquarius/true", name, ok)
	}
}

// The prewarm builds the map so a request never has to, and is a no-op
// while it is fresh.
func TestContractProtocolIndex_PrewarmBuildsOnceWhileFresh(t *testing.T) {
	stub := &rosterStub{}
	s := newContractIndexServer(stub)
	s.PrewarmContractProtocolIndex(context.Background())
	after := stub.calls.Load()
	if after == 0 {
		t.Fatal("prewarm did not build")
	}
	s.PrewarmContractProtocolIndex(context.Background())
	if got := stub.calls.Load(); got != after {
		t.Fatalf("prewarm rebuilt a fresh map (%d → %d registry reads)", after, got)
	}
	if name, ok := s.contractProtocol(context.Background(), contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label after prewarm = %q/%v, want aquarius/true", name, ok)
	}
}
