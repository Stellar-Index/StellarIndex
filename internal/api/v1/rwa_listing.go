package v1

import (
	"context"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// C2's second arm on the read path: the independent listing directory,
// and the candidate population it opens up.
//
// The RULE lives in internal/rwa/contract.go, where the argument for why
// a listing map corroborates rather than attests is made. This file is
// the read path — where the second source comes from, which candidates
// it lets the surface evaluate that it could not before, and what the
// response says when it is not answering.
//
// # The population this arm exists to reach
//
// buildRWAContractMembership drew its candidates from ONE place: the
// contract addresses the curated account directory names with an
// issuing tag. An address that directory has never heard of was not
// refused by any requirement — it was never enumerated, never had its
// metadata read, and never appeared in any tally. That is the same
// silent-discard shape the contract arm itself was built to close on
// the classic side, reproduced one level down.
//
// Measured 2026-09-15: the curated directory names 387 contract
// addresses and the listing directory names 17 on Stellar, with FOUR in
// both. The in-repo curated binding set names five, and the directory
// names NONE of them. So the whole curated binding set — verified
// addresses, named instruments, known classes, nine figures of supply
// sitting in the certified lake — was unreachable by construction.
//
// So the population becomes the UNION of two sets: the contract
// addresses the curated directory recognises, and every address the
// in-repo curated binding set names. The second half is bounded by the
// size of a hand-reviewed table in this repository, so it cannot grow
// without a code change.
//
// Enumerating a curated binding is NOT admitting one. Every candidate
// from the second half still has to satisfy C2, and the only way it can
// is for the listing directory to name the same address independently.
// What enumeration buys is that its refusal is now REPORTED: an address
// this repository has verified and nobody else has named appears in the
// funnel under contract_curated_binding_without_independent_listing
// rather than vanishing before the accounting starts.
//
// # Why the two arms are reported separately
//
// They narrow different populations from different roots, exactly as
// the classic and contract arms do, so they are a separate funnel arm
// and their stage counts reconcile only within themselves. A reader who
// took one narrowing for the whole would conclude that an address
// absent from the first was refused, when it was never in that
// population at all.
//
// The two candidate sets are made DISJOINT rather than merely counted
// twice: an address the curated directory already recognises is
// evaluated by the directory arm and removed from this one. Both arms
// would otherwise evaluate it, both would admit it, and the served set
// would carry it twice — which is how a double-counted market cap gets
// shipped.

// ─── storage seam ───────────────────────────────────────────────────

// RWAListingDirectoryReader is the seam the second C2 arm reads its
// corroborating source through. *timescale.Store satisfies it.
//
// Optional, like every other reader on this surface. A deployment
// without it serves the first arm alone and says so — the arm reports
// itself UNMEASURED rather than reporting a population of zero, because
// a zero would assert that no independent listing names any of these
// addresses, which is a finding nobody made.
type RWAListingDirectoryReader interface {
	ListingDirectoryContracts(ctx context.Context) (
		[]timescale.ListingEntry, timescale.ListingDirectoryCensus, error)
}

// ─── snapshot ───────────────────────────────────────────────────────

// rwaListing is one read of the independent listing directory, reduced
// to what C2 may consult.
type rwaListing struct {
	// byAddress holds the listing rows inside the recognition bound,
	// keyed by the exact Stellar contract address.
	byAddress map[string]timescale.ListingEntry
	// census is the storage layer's account of its own numbers,
	// including the rows it refused to serve as stale. That count is
	// the visible form of the fail-closed guarantee: a set that shrank
	// because the sync stopped running must say so.
	census timescale.ListingDirectoryCensus
	// available is false when the read failed or the whole snapshot is
	// past its bound. Either way no naming was established, so none may
	// be assumed and every candidate needing one is refused.
	available bool
	// wired reports that a listing reader is CONFIGURED, whatever it
	// answered. The distinction is the one this whole surface is built
	// on: an unwired reader means nobody looked, and an arm of zeros
	// would assert that no independent party names any of these
	// addresses. A wired reader that failed DID look, and its failure
	// is a measured, reportable shrink of the set — which is the state
	// the funnel has to be loudest about, because it is the one an
	// operator can fix.
	wired bool
	// read reports that the directory ANSWERED, which is what makes
	// `census` a real observation rather than a zero value. A failed
	// read leaves both false, and the difference matters on the wire:
	// zero entries observed says the table has never been synced, while
	// a read that did not complete has observed nothing and must not be
	// published as a count of anything.
	read bool
	// observedAt is when the read that produced `census` was taken.
	//
	// It is the one fact on this struct that cannot be recovered later.
	// Every count beside it can be re-derived by querying the table
	// again; the moment the served set looked at that table cannot be,
	// and it is the only thing that distinguishes "the directory is
	// empty" from "this set was built before the directory filled".
	// Zero when nothing was observed.
	observedAt time.Time
}

// names reports whether the listing directory names this exact address,
// and is false on an unavailable snapshot rather than panicking on a
// nil map — an unavailable read has no opinion about any address.
func (l rwaListing) names(contractID string) bool {
	if !l.available {
		return false
	}
	_, ok := l.byAddress[contractID]
	return ok
}

// rwaListingSnapshot reads the listing directory once per rebuild.
//
// Fails CLOSED in the strong sense: a failed read yields an UNAVAILABLE
// snapshot and never a carried-forward one. The oracle reference path
// one file over deliberately carries its last good snapshot across a
// failed read, and the difference between the two is the difference
// between a price and a permission. Serving a slightly old price is a
// labelled approximation; serving an old RECOGNITION is admitting an
// asset on the strength of a fact nobody re-established, which is the
// one thing C2 exists to prevent.
//
// The staleness bound itself is enforced in SQL by the reader, so a
// snapshot that arrives here is already inside it.
func (s *Server) rwaListingSnapshot(ctx context.Context) rwaListing {
	if s.rwaListings == nil {
		return rwaListing{}
	}
	rows, census, err := s.rwaListings.ListingDirectoryContracts(ctx)
	if err != nil {
		s.logger.Warn("rwa listing directory read failed", "err", err)
		return rwaListing{wired: true}
	}
	out := rwaListing{
		byAddress: make(map[string]timescale.ListingEntry, len(rows)),
		census:    census,
		// A snapshot carrying NO fresh contract rows cannot support the
		// finding "nobody independent names this address", whatever the
		// reason it is empty. The storage reader drops every row past
		// the recognition bound, so a table that has gone entirely
		// stale arrives here as zero rows beside a non-zero Stale
		// count — indistinguishable, from this side, from a listing
		// that genuinely names nothing.
		//
		// The two are NOT the same statement and the difference is the
		// whole fail-closed guarantee. Treated as available, an aged-out
		// snapshot would refuse every binding under
		// contract_curated_binding_without_independent_listing — a
		// finding about the world — when the truth is that our copy
		// expired. Reported as unavailable it refuses them under
		// independent_listing_unavailable, which names an outage an
		// operator can fix.
		//
		// A table nobody has ever synced lands in the same branch and
		// deserves the same answer: zero fresh rows is zero evidence.
		available:  len(rows) > 0,
		wired:      true,
		read:       true,
		observedAt: time.Now().UTC(),
	}
	if !out.available {
		// The same two numbers the funnel now publishes. They were
		// computed here, printed once, and dropped — which meant the
		// question "is the sync down, or is this set older than the
		// sync" could only be answered by somebody with a database
		// prompt. They are carried onto the census below and served.
		s.logger.Warn("rwa listing directory: no fresh contract rows",
			"stale", census.Stale, "entries", census.Entries,
			"observed_at", out.observedAt.Format(time.RFC3339))
	}
	for _, r := range rows {
		out.byAddress[r.Address] = r
	}
	return out
}

// ─── candidate population ───────────────────────────────────────────

// rwaListingCandidate is one address the second arm will evaluate: a
// curated binding, plus the curated-directory tags C3 has to read for
// it.
type rwaListingCandidate struct {
	contractID string
	// dirTags are the curated tags on the address, empty when the
	// directory does not name it. Looked up even though the directory
	// is not this arm's recognition source, because the scam vocabulary
	// lives there and NOWHERE else. An arm that skipped the lookup
	// would be an arm on which a scam flag has no effect.
	dirTags []string
}

// rwaListingCensus is the second arm's accounting.
type rwaListingCensus struct {
	// bindings is every in-repo curated contract binding — the root of
	// this arm's narrowing, and a constant of the build.
	bindings int
	// alsoInDirectoryArm counts bindings the first arm already
	// evaluates. Removed here so no address is evaluated twice.
	alsoInDirectoryArm int
	// listingUnavailable counts bindings refused because the LISTING
	// read did not answer. One source, named exactly.
	//
	// A failed curated-tag lookup used to land here too, and the
	// conflation was wrong in the way this surface exists to prevent.
	// The two reads are unrelated sources with unrelated failure modes,
	// and they are not even correlated: the curated directory can be
	// unreadable while the listing directory sits there perfectly
	// fresh. Folded together, the funnel then reported an outage of the
	// source that ANSWERED and sent an operator to a working sync.
	listingUnavailable int
	// tagsUnavailable counts bindings refused because the CURATED
	// DIRECTORY's tag read did not answer, leaving C3 unevaluated.
	//
	// Counted apart from listingUnavailable because a refusal reason is
	// an instruction to somebody, and these two send that somebody to
	// different systems.
	tagsUnavailable int
	// notListed counts bindings the listing directory does not name.
	// The refusal that holds independence up.
	notListed int
	// evaluated is what reached rwa.QualifyContract.
	evaluated int
	// listedWithoutBinding counts the addresses the listing directory
	// names that no curated binding does — a TERMINAL census, not part
	// of the narrowing. It is the population an operator would review
	// to grow the set, and publishing it is what keeps "the listing
	// admits nothing on its own" an auditable claim rather than a
	// promise.
	listedWithoutBinding int
	// available is false when the arm was not walked at all.
	available bool

	// dir is the storage layer's account of the listing directory
	// ITSELF — every row it holds, not just the ones this arm narrowed
	// — carried so the arm can publish the EVIDENCE for its verdict and
	// not only the verdict. The sibling of [rwaContractCensus].dir, and
	// there for the same reason.
	//
	// The verdict alone is ambiguous in the one direction that costs an
	// operator an afternoon. `listing_corroborated_contracts: 0` is
	// produced by a directory nobody has ever synced, by a sync that
	// died two days ago, and by a healthy directory this set simply
	// predates — three states with three different responses, and
	// nothing on the wire separated them. These counts do:
	//
	//   - Entries 0                 → never synced
	//   - Entries > 0, Stale = all  → the sync stopped
	//   - Entries > 0, Stale 0      → healthy, and this set predates it
	//
	// Zero when nothing was observed; observedAt below is what says
	// which.
	dir timescale.ListingDirectoryCensus
	// observedAt is when the read behind `dir` was taken, and it is the
	// load-bearing half of this pair. The counts are a convenience —
	// anyone holding a database prompt can re-derive them at will. The
	// moment the served set looked cannot be re-derived by anyone,
	// afterwards, at any cost, and without it a reader comparing a
	// healthy directory against a closed arm has no way to tell that
	// the two observations are of different moments.
	//
	// Zero means nothing was observed: no reader wired, or a read that
	// did not answer. Never confused with the Unix epoch, because a
	// zero time publishes no evidence at all rather than a timestamp.
	observedAt time.Time
}

// rwaListingCandidates builds the second arm's population: every curated
// binding the first arm is not already evaluating, with the curated tags
// C3 needs.
//
// The directory lookup is a REQUIREMENT of this arm, not a decoration.
// C3 refuses a scam-flagged address whatever named it, and the only
// place a scam flag exists is the curated directory — so a candidate
// whose tags could not be read has not had C3 evaluated and must not be
// admitted. A failed lookup therefore closes the arm exactly as a failed
// listing read does — under its OWN reason, never the listing's.
//
// Two sources, two reasons. They are unrelated systems with unrelated
// failure modes, and a refusal reason is an instruction about where to
// go and look. Under one reason the funnel could report an outage of
// the listing directory while that directory sat there perfectly
// fresh — an alarm naming a working sync, raised by the failure of
// something else entirely.
func (s *Server) rwaListingCandidates(
	ctx context.Context, listing rwaListing, directoryEvaluated map[string]struct{},
) ([]rwaListingCandidate, rwaListingCensus) {
	// Not wired is NOT a finding. The arm reports itself unwalked, makes
	// no refusals, and the funnel basis says why the set is the size it
	// is. A wired reader that FAILED is the opposite: it looked, it
	// could not answer, and every binding it would have corroborated is
	// refused under a reason an operator can act on.
	if !listing.wired {
		return nil, rwaListingCensus{}
	}
	bindings := rwa.ContractInstrumentBindings()
	census := rwaListingCensus{bindings: len(bindings), available: true}
	// Carried only from a read that ANSWERED. A failed read observed
	// nothing, and a zero census published beside a zero timestamp
	// would read as a directory holding no rows — which is a finding,
	// from a query that did not complete.
	if listing.read {
		census.dir = listing.census
		census.observedAt = listing.observedAt
	}

	bound := make(map[string]struct{}, len(bindings))
	pending := make([]string, 0, len(bindings))
	for _, b := range bindings {
		bound[b.ContractID] = struct{}{}
		if _, dup := directoryEvaluated[b.ContractID]; dup {
			census.alsoInDirectoryArm++
			continue
		}
		pending = append(pending, b.ContractID)
	}
	sort.Strings(pending)

	// The terminal census. Counted over the whole listing snapshot
	// rather than over the pending set, because its subject is the
	// listing's own contents and not this arm's narrowing.
	for addr := range listing.byAddress {
		if _, ok := bound[addr]; !ok {
			census.listedWithoutBinding++
		}
	}

	tags, tagsOK := s.rwaDirectoryTagsFor(ctx, pending)
	out := make([]rwaListingCandidate, 0, len(pending))
	for _, addr := range pending {
		// Ordered by which source is missing, most specific first. The
		// listing read is this arm's RECOGNITION source, so its absence
		// closes the arm whatever the curated directory said; only when
		// the listing answered can a tag failure be the thing standing
		// in the way, and that is exactly the case that used to be
		// reported as a listing outage.
		switch {
		case !listing.available:
			census.listingUnavailable++
		case !tagsOK:
			census.tagsUnavailable++
		case !listing.names(addr):
			census.notListed++
		default:
			out = append(out, rwaListingCandidate{contractID: addr, dirTags: tags[addr]})
		}
	}
	census.evaluated = len(out)
	return out, census
}

// rwaDirectoryTagsFor batch-reads the curated tags for a set of
// addresses, reporting whether the read answered at all.
//
// ok=false is NOT "no tags". An empty tag list means the directory does
// not flag the address; a failed read means nobody looked, and the two
// must never be confused on a path where the absence of a scam flag is
// what admits an asset. An absent reader is the same statement: C3
// cannot be evaluated, so the arm closes.
func (s *Server) rwaDirectoryTagsFor(ctx context.Context, addrs []string) (map[string][]string, bool) {
	if len(addrs) == 0 {
		return map[string][]string{}, true
	}
	if s.directory == nil {
		return nil, false
	}
	found, err := s.directory.DirectoryEntriesByAddresses(ctx, addrs)
	if err != nil {
		s.logger.Warn("rwa listing arm: curated tag lookup failed", "n", len(addrs), "err", err)
		return nil, false
	}
	out := make(map[string][]string, len(found))
	for addr, e := range found {
		out[addr] = e.Tags
	}
	return out, true
}
