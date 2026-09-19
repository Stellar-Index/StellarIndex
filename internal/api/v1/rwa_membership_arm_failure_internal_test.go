package v1

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A rebuild in which one arm's READ FAILED must not become the served
// set, and must not clear the failure stamp (RLT-096).
//
// The shape the defect had: the cache decision asked only whether
// EITHER arm had answered, so a failed classic scan beside a healthy
// contract scan cached a set with no classic members in it, dated the
// cache NOW and cleared rwaFailedAt — so the surface served a
// half-empty membership set for the full ten-minute lifetime while
// reporting `stale: false` and no `rebuild_failed_at`. Membership is
// what this index CALLS a real-world asset, so a half set is published
// coverage that is wrong, self-certified fresh, and invisible to a
// reader of the response.
//
// The distinction the fix rests on is WIRED-AND-FAILED versus NOT
// WIRED. `available: false` says both, and only the first may refuse a
// cache write — which is what [TestRefreshRWAMembership_UnwiredArmStillCaches]
// below holds in place.

// stubRWAContractArm answers the contract arm's directory scan. Empty
// by default: what matters here is that the arm ANSWERED, not what it
// found.
type stubRWAContractArm struct {
	entries []timescale.DirectoryEntry
	err     error
}

func (s *stubRWAContractArm) DirectoryRecognisedContracts(
	context.Context, []string,
) ([]timescale.DirectoryEntry, timescale.DirectoryRWACensus, error) {
	if s.err != nil {
		return nil, timescale.DirectoryRWACensus{}, s.err
	}
	return s.entries, timescale.DirectoryRWACensus{
		Entries:             len(s.entries),
		Contracts:           len(s.entries),
		ContractsRecognised: len(s.entries),
	}, nil
}

func (s *stubRWAContractArm) DirectoryRecognisedIssuersWithoutAsset(
	context.Context, []string, int,
) ([]timescale.DirectoryEntry, error) {
	return nil, nil
}

// unwiredSep1Reader satisfies [Sep1CachedReader] and NOT
// [Sep1BoundCurrencyReader] — the deployment that never wired the
// attestation scan, which is a configuration statement rather than an
// outage.
type unwiredSep1Reader struct{}

func (unwiredSep1Reader) GetIssuerSep1Cached(
	context.Context, string,
) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

// seedDatedRWACache plants a last-good set that carries its own build
// instant, so the served set can be asked whether a failed rebuild
// re-dated it.
func seedDatedRWACache(s *Server, age time.Duration) time.Time {
	at := time.Now().UTC().Add(-age)
	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	s.rwaCache = &rwaMembership{
		available: true,
		refusals:  map[string]int{},
		members: []rwaMember{{
			code:   "USTRY",
			issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		}},
		builtAt: at,
	}
	s.rwaAt = at
	return at
}

// The classic arm fails while the contract arm answers: the last good
// set stays, and the response says a rebuild failed.
func TestRefreshRWAMembership_FailedClassicArmDoesNotOverwrite(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	reader.mu.Lock()
	reader.err = context.DeadlineExceeded
	reader.mu.Unlock()
	s := rwaCacheTestServer(reader)
	s.rwaContracts = &stubRWAContractArm{}
	builtAt := seedDatedRWACache(s, rwaMembershipTTL+time.Minute)

	s.cachedRWAMembership(context.Background())
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	close(reader.release)
	waitForRWAFlight(t, s)

	after := s.cachedRWAMembership(context.Background())
	if len(after.members) != 1 || after.members[0].code != "USTRY" {
		t.Fatalf("a failed classic scan overwrote the served set with a half-empty one: %+v", after.members)
	}
	set := rwaMembershipSetOf(after)
	if set == nil {
		t.Fatal("the served set carries no build instant")
	}
	if got := time.Time(set.BuiltAt); !got.Equal(builtAt) {
		t.Fatalf("the served set was re-dated to %s; it was built at %s", got, builtAt)
	}
	if !set.Stale {
		t.Fatal("a set kept past its lifetime by a failed rebuild published stale=false")
	}
	if set.RebuildFailedAt == nil {
		t.Fatal("a failed rebuild published no rebuild_failed_at: " +
			"a lapsed set nothing is replacing reads as one about to be replaced")
	}
	if failed := time.Time(*set.RebuildFailedAt); time.Since(failed) > time.Minute {
		t.Fatalf("rebuild_failed_at is %s, not the failure that just happened", failed)
	}
}

// The mirror case, on the other arm: the contract directory scan fails
// while the classic scan answers. The classic answer here is EMPTY (the
// stub scan binds nothing), so caching it would blank the served set
// just as certainly as the case above.
func TestRefreshRWAMembership_FailedContractArmDoesNotOverwrite(t *testing.T) {
	reader := newBlockingRWASep1Reader()
	s := rwaCacheTestServer(reader)
	s.rwaContracts = &stubRWAContractArm{err: context.DeadlineExceeded}
	seedDatedRWACache(s, rwaMembershipTTL+time.Minute)

	s.cachedRWAMembership(context.Background())
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild was started")
	}
	close(reader.release)
	waitForRWAFlight(t, s)

	after := s.cachedRWAMembership(context.Background())
	if len(after.members) != 1 || after.members[0].code != "USTRY" {
		t.Fatalf("a failed contract scan overwrote the served set: %+v", after.members)
	}
	set := rwaMembershipSetOf(after)
	if set == nil || set.RebuildFailedAt == nil {
		t.Fatalf("a failed contract scan published no rebuild_failed_at: %+v", set)
	}
}

// An arm that was never WIRED must still cache. Treating it as a
// failure would rebuild on every request and never cache on a
// deployment that simply has one reader — the behaviour the either-arm
// rule was written for, and which the fix above must not take away.
func TestRefreshRWAMembership_UnwiredArmStillCaches(t *testing.T) {
	s := rwaCacheTestServer(unwiredSep1Reader{})
	s.rwaContracts = &stubRWAContractArm{}

	s.PrewarmRWA(context.Background())

	s.rwaMu.Lock()
	defer s.rwaMu.Unlock()
	if s.rwaCache == nil {
		t.Fatal("an unwired classic arm was treated as a failed one: nothing was cached")
	}
	if !s.rwaFailedAt.IsZero() {
		t.Fatal("an unwired arm stamped a rebuild failure: that is a configuration statement, not an outage")
	}
}

// The funnel has to say the classic arm was not measured, for the
// reason it already says so for the other two: its stages then read
// zero, and a narrowing of zeros asserts that no issuer on the network
// publishes a real-world-asset attestation — a finding, out of a scan
// that never ran.
func TestRWAFunnelBasis_NamesAnUnmeasuredClassicArm(t *testing.T) {
	const unmeasuredSaid = "`classic` arm was NOT MEASURED"

	measured := rwaFunnelOf(
		rwaMembership{available: true, refusals: map[string]int{}},
		rwaCatalogueJoin{}, 0, 0, 0, nil,
	)
	if strings.Contains(measured.Basis, unmeasuredSaid) {
		t.Fatal("a measured classic arm was reported as unmeasured")
	}

	unmeasured := rwaFunnelOf(
		rwaMembership{refusals: map[string]int{}},
		rwaCatalogueJoin{}, 0, 0, 0, nil,
	)
	if !strings.Contains(unmeasured.Basis, unmeasuredSaid) {
		t.Fatalf("an unmeasured classic arm is not named in the funnel basis: %s", unmeasured.Basis)
	}
}
