package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPrewarmCoversRWAMembersTheListingNeverRanks is the regression for a gap
// between two surfaces rather than a bug in either. The lake-supply prewarm
// took its population from the ASSETS listing's ranked pages; /v1/rwa/assets
// publishes a set chosen by attestation, not by rank, and a tokenized
// instrument is bought and held — so a member can sit outside every page
// ordered by observation count or 24h volume, never be warmed, and serve the
// trustline floor permanently rather than for one TTL gap.
//
// Measured on r1 2026-09-16: USDY served 461,621,813.40 against 467,502,151.70
// across all holding domains, USTRY 9.31% short, TESOURO 14.91% short.
func TestPrewarmCoversRWAMembersTheListingNeverRanks(t *testing.T) {
	const (
		code   = "USDY"
		issuer = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"
	)
	s := &Server{
		logger:   discardLogger(),
		rwaCache: &rwaMembership{members: []rwaMember{{code: code, issuer: issuer}}, builtAt: time.Now()},
	}

	got := s.rwaClassicPrewarmSet(context.Background())

	sac, ok := classicSACContractID(code + "-" + issuer)
	if !ok {
		t.Fatal("could not derive the SAC for the fixture asset")
	}
	if got[code+"-"+issuer] != sac {
		t.Errorf("prewarm set = %v, want the member keyed to its own SAC (%s) — a member "+
			"the listing never ranks is never warmed, and serves the trustline floor for good",
			got, sac)
	}
}

// TestPrewarmWarmsAnRWAMemberTheListingDoesNotReturn is the WIRING half, and
// the one that matters: the helper above can be perfect while nothing calls
// it. It drives the production sweep with a listing that returns a DIFFERENT
// asset, so the only way the member reaches the cache is through the join.
//
// Without that join this passes against a build that computes the RWA set and
// discards it — which is precisely the state this test was written after
// finding.
func TestPrewarmWarmsAnRWAMemberTheListingDoesNotReturn(t *testing.T) {
	// The listing ranks CETES. The RWA set holds CETES *and* a member the
	// listing never returns, which is the shape on r1: a tokenized instrument
	// is bought and held, so it sits outside pages ordered by volume.
	const outsideTheListing = "USTRY-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	rows := []timescale.AssetRow{classicListingRow("CETES")}
	s, _, _ := lakeSupplyPrewarmServer(t, rows, map[string]string{
		cetesAsset:        cetesTrueSupply,
		outsideTheListing: "115139464900000",
	})
	s.rwaMu.Lock()
	s.rwaCache = &rwaMembership{
		members: []rwaMember{
			{code: "USTRY", issuer: "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"},
		},
		builtAt: time.Now(),
	}
	s.rwaMu.Unlock()

	s.PrewarmClassicLakeSupply(context.Background(), oneShapeOpts)

	s.lakeSupplyMu.Lock()
	entry, cached := s.lakeSupply[outsideTheListing]
	s.lakeSupplyMu.Unlock()
	if !cached {
		t.Fatalf("no cache entry for %s after a prewarm pass. It is an RWA member the "+
			"listing never ranks, so nothing warms it and /v1/rwa/assets serves the "+
			"trustline floor for good — measured on r1 as USTRY 9.31%% short and "+
			"TESOURO 14.91%% short of their all-domain totals.", outsideTheListing)
	}
	if entry.value != "115139464900000" {
		t.Errorf("cached reading = %q, want the lake total", entry.value)
	}
}

// TestPrewarmNeverForcesAnRWARebuild — the sweep exists to keep expensive work
// off the request path. A prewarm that could trigger the attestation scan
// would put that scan on a timer, which is the opposite of the point. Before
// the first membership build there is nothing to warm, and saying so by
// returning nothing is correct rather than a degradation.
func TestPrewarmNeverForcesAnRWARebuild(t *testing.T) {
	s := &Server{logger: discardLogger()} // no cache, no reader, no in-flight build

	if got := s.rwaClassicPrewarmSet(context.Background()); len(got) != 0 {
		t.Errorf("prewarm set = %v on a server with no membership, want empty", got)
	}
}

// TestPrewarmDropsMembersTheReadPathWouldNotLookUp — the reduction is the
// REQUEST PATH's own, so the prewarm cannot warm an asset the read would never
// ask about. Native has no issuer and a contract-issued member has no
// (code, issuer) pair, and neither derives a classic SAC.
func TestPrewarmDropsMembersTheReadPathWouldNotLookUp(t *testing.T) {
	s := &Server{
		logger: discardLogger(),
		rwaCache: &rwaMembership{
			members: []rwaMember{
				{code: "native", issuer: ""},
				{code: "BAD", issuer: "not-a-strkey"},
			},
			builtAt: time.Now(),
		},
	}

	if got := s.rwaClassicPrewarmSet(context.Background()); len(got) != 0 {
		t.Errorf("prewarm set = %v, want empty — neither member derives a classic SAC", got)
	}
}
