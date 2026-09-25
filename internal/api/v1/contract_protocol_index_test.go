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
	// block, when set, holds every roster read until it is closed or the
	// read's ctx ends: a registry that has stopped answering.
	block chan struct{}
	// failSource, when set to a source name, fails every roster read for
	// that one source; dropPool makes aquarius answer without the pool.
	failSource atomic.Value
	dropPool   atomic.Bool
	calls      atomic.Int32
	rounds     atomic.Int32
}

func (r *rosterStub) ListProtocolContracts(ctx context.Context, source string) ([]timescale.ProtocolContract, error) {
	r.calls.Add(1)
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
		}
	}
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
	if failed, _ := r.failSource.Load().(string); failed == source {
		return nil, errors.New("roster read timed out")
	}
	if source == "aquarius" && !r.dropPool.Load() {
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

// waitContractIndexIdle blocks until no build is in flight, so a test can
// read what a request-triggered background build published.
func waitContractIndexIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * contractProtocolIndexBuildTimeout)
	for time.Now().Before(deadline) {
		s.contractIndex.mu.Lock()
		inFlight := s.contractIndex.inFlight
		s.contractIndex.mu.Unlock()
		if !inFlight {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("contract protocol index build still in flight past twice its own timeout")
}

// A request whose budget is already spent must not build — and cache —
// a statics-only map for everyone else: the build it triggers runs on
// its own deadline, so the map it publishes labels the pool and is
// complete.
func TestContractProtocolIndex_BuildIsDetachedFromTheRequestDeadline(t *testing.T) {
	stub := &rosterStub{}
	s := newContractIndexServer(stub)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	s.contractProtocol(dead, contractIndexTestPool)
	waitContractIndexIdle(t, s)
	if name, ok := s.contractProtocol(dead, contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label after a build triggered under a dead request ctx = %q/%v, want aquarius/true", name, ok)
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

	s.contractProtocol(ctx, contractIndexTestPool)
	waitContractIndexIdle(t, s)
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
	waitContractIndexIdle(t, s)
	if got := stub.calls.Load(); got != before {
		t.Fatalf("a rebuild ran inside the retry TTL (%d → %d registry reads)", before, got)
	}
	// Past the retry TTL but well inside the long TTL: it must rebuild.
	s.contractIndex.mu.Lock()
	s.contractIndex.built = time.Now().Add(-contractProtocolIndexRetryTTL - time.Second)
	s.contractIndex.mu.Unlock()
	s.contractProtocol(ctx, contractIndexTestPool)
	waitContractIndexIdle(t, s)
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

	// Request-triggered: the panic lands on the build goroutine, which
	// must survive it (an unrecovered panic there would kill the process).
	s.contractProtocolIndexFor(ctx)
	waitContractIndexIdle(t, s)

	s.contractProtocol(ctx, contractIndexTestPool)
	waitContractIndexIdle(t, s)
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

// A cohort request that finds the map due must not wait on the rebuild:
// against a registry that has stopped answering, an inline build holds
// the request for the whole build timeout on top of the handler's own
// read budget. The request serves the map it found and the rebuild runs
// on its own goroutine.
func TestContractProtocolIndex_RequestNeverWaitsOnARebuild(t *testing.T) {
	stub := &rosterStub{block: make(chan struct{})}
	s := newContractIndexServer(stub)
	s.contractIndex.byID = map[string]string{contractIndexTestPool: "aquarius"}
	s.contractIndex.complete = true
	s.contractIndex.built = time.Now().Add(-contractProtocolIndexTTL - time.Second)
	t.Cleanup(func() {
		close(stub.block)
		waitContractIndexIdle(t, s)
	})

	type label struct {
		name string
		ok   bool
	}
	got := make(chan label, 1)
	go func() {
		name, ok := s.contractProtocol(context.Background(), contractIndexTestPool)
		got <- label{name, ok}
	}()
	select {
	case l := <-got:
		if !l.ok || l.name != "aquarius" {
			t.Fatalf("label served during the rebuild = %q/%v, want the previous map's aquarius/true", l.name, l.ok)
		}
	case <-time.After(contractProtocolIndexBuildTimeout / 5):
		t.Fatalf("the request was still waiting on the rebuild after %s — it must serve the map it found",
			contractProtocolIndexBuildTimeout/5)
	}
	s.contractIndex.mu.Lock()
	inFlight := s.contractIndex.inFlight
	s.contractIndex.mu.Unlock()
	if !inFlight {
		t.Fatal("the request found the map due but started no rebuild")
	}
}

// expireContractIndex ages the map past even the long TTL, so the next
// prewarm rebuilds it whatever its completeness.
func expireContractIndex(s *Server) {
	s.contractIndex.mu.Lock()
	s.contractIndex.built = time.Now().Add(-contractProtocolIndexTTL - time.Second)
	s.contractIndex.mu.Unlock()
}

// A rebuild in which one protocol's roster read fails must not unlabel
// that protocol's discovered contracts: the complete map before it knew
// them, and the failed read says nothing about whether they left. The
// map stays incomplete, so the short retry TTL still applies.
func TestContractProtocolIndex_FailedRebuildKeepsTheFailedProtocolsLabels(t *testing.T) {
	stub := &rosterStub{}
	s := newContractIndexServer(stub)
	s.PrewarmContractProtocolIndex(context.Background())
	if name, ok := s.contractProtocol(context.Background(), contractIndexTestPool); !ok || name != "aquarius" {
		t.Fatalf("label after the complete build = %q/%v, want aquarius/true", name, ok)
	}

	stub.failSource.Store("aquarius")
	expireContractIndex(s)
	s.PrewarmContractProtocolIndex(context.Background())

	s.contractIndex.mu.Lock()
	name, ok := s.contractIndex.byID[contractIndexTestPool]
	complete := s.contractIndex.complete
	s.contractIndex.mu.Unlock()
	if !ok || name != "aquarius" {
		t.Fatalf("label after a rebuild whose aquarius roster read failed = %q/%v, want the previous "+
			"map's aquarius/true", name, ok)
	}
	if complete {
		t.Fatal("a rebuild with a failed roster read must stay incomplete so it is retried soon")
	}
}

// Only the FAILED protocols carry forward. A protocol that answered is
// taken as it answered, so a contract it no longer lists loses its label
// even while some other protocol's read is failing.
func TestContractProtocolIndex_FailedRebuildDoesNotResurrectAnAnsweringProtocolsContracts(t *testing.T) {
	other := ""
	for _, meta := range protocolRegistry {
		if meta.Name != "aquarius" {
			other = meta.Name
			break
		}
	}
	stub := &rosterStub{}
	s := newContractIndexServer(stub)
	s.PrewarmContractProtocolIndex(context.Background())

	stub.failSource.Store(other)
	stub.dropPool.Store(true)
	expireContractIndex(s)
	s.PrewarmContractProtocolIndex(context.Background())

	if name, ok := s.contractProtocol(context.Background(), contractIndexTestPool); ok {
		t.Fatalf("aquarius answered without the pool but it is still labelled %q (%s was the failed read)", name, other)
	}
}
