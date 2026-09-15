package v1_test

import (
	"errors"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — the EVIDENCE behind C2's second arm, as opposed
// to its verdict.
//
// These tests exist because of a production episode on 2026-09-15. The
// listing sync completed at 17:01:21Z having written 50 good rows, and
// the funnel went on reporting the arm closed for the next ten and a
// half minutes — correctly, because the set in hand had been built from
// a read taken at 17:00:28Z, fifty-three seconds before the rows
// landed. Nothing was broken. Nothing needed fixing.
//
// Establishing that took four production queries, a role-switch test
// and a row-level-security check, because the response published the
// VERDICT — an arm reporting nothing corroborated — and none of the
// evidence for it. Three unrelated states of the world produce that
// same verdict:
//
//   - a directory nobody has ever synced,
//   - a sync that stopped days ago and left every row to age out,
//   - a healthy directory this set simply predates.
//
// They call for three different responses: run the sync, go and fix the
// sync, and do nothing at all. Told apart from the wire now, and the
// tests below are one per state plus the two states that publish no
// evidence at all.

// rwaEvidenceServer wires the listing arm with nothing else able to
// admit a row, so the funnel under test is arm 2's alone.
func rwaEvidenceServer(t *testing.T, listings v1.RWAListingDirectoryReader) *v1.Server {
	t.Helper()
	return rwaListingServer(t, listings,
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	)
}

// The test that would have answered the question directly: a set built
// from a read taken BEFORE the sync landed must say, on the wire, when
// it looked and what it saw.
//
// Without the timestamp this is unanswerable after the fact by anyone,
// at any cost. Every count beside it can be re-derived by reading the
// table again a minute later — and reading the table again a minute
// later is precisely what produces the misleading answer, because by
// then the sync has run and the directory looks healthy while the
// response still reports a closed arm.
func TestRWAListingEvidence_DatesTheReadThatClosedTheArm(t *testing.T) {
	// The directory as it stood before the first sync ever completed.
	before := time.Now().UTC().Add(-time.Second)
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{}))
	after := time.Now().UTC().Add(time.Second)

	ev := view.Funnel.ListingDirectory
	if ev == nil {
		t.Fatal("the arm closed and published no evidence for it: a reader " +
			"holding only the verdict cannot tell a stopped sync from a set " +
			"that predates a working one")
	}
	at := ev.ObservedAt.Time()
	if at.Before(before) || at.After(after) {
		t.Errorf("observed_at = %s, want the instant of the read (between %s and %s)",
			at.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
	if at.Location() != time.UTC {
		t.Errorf("observed_at carries a %v offset; the wire contract is UTC", at.Location())
	}
	// And it is the read's instant, not the response's: a timestamp
	// stamped at render time would date every response to now and
	// answer the opposite of the question asked.
	if ev.Entries != 0 || ev.Stale != 0 {
		t.Errorf("evidence = %+v, want the empty directory the read actually saw", ev)
	}
	if n := stageCount(t, view, "listing_corroborated_contracts"); n != 0 {
		t.Fatalf("corroborated = %d — the fixture must close the arm for this to mean anything", n)
	}
}

// One case per state of the world that closes the arm. The verdict is
// identical in all three; only the evidence separates them.
//
// `entries` counts FRESH rows only — the storage layer's invariant is
// Entries = Contracts + Classic, with Stale counted BESIDE them and
// never inside them — so the two zero-entry states are told apart by
// `stale`, and by nothing else on the wire.
func TestRWAListingEvidence_SeparatesTheThreeClosedArmStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listings  *stubRWAListings
		entries   int
		contracts int
		stale     int
		reading   string
	}{
		{
			name:     "never synced",
			listings: &stubRWAListings{},
			reading:  "the table is empty and nothing has ever written to it: run the sync",
		},
		{
			// Every row ages out together, because synced_at is stamped
			// once per sync transaction. The recognised set empties
			// while every read keeps succeeding.
			name:     "the sync stopped",
			listings: &stubRWAListings{stale: 17},
			stale:    17,
			reading:  "rows are present and every one is past the bound: go and fix the sync",
		},
		{
			// A directory that lists Stellar assets only by their
			// classic CODE-GISSUER ids names no contract address at
			// all. The arm has nothing to consult and nothing is wrong.
			name: "healthy, but it names no contract address",
			listings: &stubRWAListings{census: &timescale.ListingDirectoryCensus{
				Entries: 33, Classic: 33,
			}},
			entries: 33,
			reading: "fresh rows, none of them contracts: nothing to fix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := getRWA(t, rwaEvidenceServer(t, tc.listings))
			if n := stageCount(t, view, "listing_corroborated_contracts"); n != 0 {
				t.Fatalf("corroborated = %d, want 0 — every case here closes the arm", n)
			}
			ev := view.Funnel.ListingDirectory
			if ev == nil {
				t.Fatalf("no evidence published; a reader cannot reach %q from the verdict alone", tc.reading)
			}
			if ev.Entries != tc.entries || ev.Contracts != tc.contracts || ev.Stale != tc.stale {
				t.Errorf("evidence = {entries %d, contracts %d, stale %d}, want {%d, %d, %d} — %s",
					ev.Entries, ev.Contracts, ev.Stale, tc.entries, tc.contracts, tc.stale, tc.reading)
			}
			if ev.ObservedAt.IsZero() {
				t.Error("evidence carries counts with no instant: the counts can be re-derived " +
					"from the table afterwards, the moment this set looked cannot")
			}
		})
	}
}

// A HEALTHY arm publishes its evidence too. Without this a change that
// only ever emitted the block on the closed path would pass everything
// above, and the one comparison the block exists for — this set's read
// against the sync's own clock — would be unavailable exactly when a
// reader is trying to confirm that a recovery has landed.
func TestRWAListingEvidence_PublishedOnTheHealthyPathToo(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{
		rows: []timescale.ListingEntry{listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22")},
	}))
	if n := stageCount(t, view, "listing_corroborated_contracts"); n != 1 {
		t.Fatalf("corroborated = %d, want 1 — the fixture must OPEN the arm here", n)
	}
	ev := view.Funnel.ListingDirectory
	if ev == nil {
		t.Fatal("an open arm published no evidence: the block is not a failure report")
	}
	if ev.Entries != 1 || ev.Contracts != 1 || ev.Stale != 0 {
		t.Errorf("evidence = %+v, want one fresh contract row", ev)
	}
}

// A read that did not ANSWER has observed nothing, and must publish
// nothing rather than a census of zeros.
//
// This is the distinction the whole block turns on. Zeros from a failed
// query render identically to zeros from an empty table, and the second
// is a finding about the directory while the first is a finding about
// our own connection. The absent block is therefore a statement in its
// own right: refusals reported with no evidence beside them are
// refusals whose source could not be read at all.
func TestRWAListingEvidence_AFailedReadPublishesNone(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{err: errRWAListingRead}))
	if ev := view.Funnel.ListingDirectory; ev != nil {
		t.Fatalf("a failed read published evidence %+v — zeros from a query that "+
			"never completed read as a directory holding no rows, which is a "+
			"finding nobody made", ev)
	}
	// The outage is still reported; it is the EVIDENCE that is absent,
	// not the verdict.
	bindings := stageCount(t, view, "curated_contract_bindings")
	if n := dropCount(view, "curated_contract_bindings", rwaRejectListingUnavailable); n != bindings {
		t.Errorf("outage drop = %d, want every one of the %d bindings", n, bindings)
	}
}

// No reader wired is the other no-evidence state, and for the stronger
// reason: nobody looked at all.
func TestRWAListingEvidence_UnwiredReaderPublishesNone(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, nil))
	if ev := view.Funnel.ListingDirectory; ev != nil {
		t.Fatalf("an unwired reader published evidence %+v — nothing was observed", ev)
	}
}

// Fixture constants kept beside the tests that read them.
var errRWAListingRead = errors.New("listing read failed")

// rwaRejectListingUnavailable is the reason an outage is published
// under, spelled once here so a rename in internal/rwa reaches these
// tests through the compiler.
const rwaRejectListingUnavailable = rwa.RejectContractListingUnavailable
