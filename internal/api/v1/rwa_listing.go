package v1

import (
	"context"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// C2's second arm on the read path: the independent listing directory and the candidate
// population it opens up. The rule, and why a listing map corroborates rather than attests,
// lives in internal/rwa/contract.go.
//
// The population is the UNION of the directory's addresses and every address the in-repo
// binding set names. Enumerating is not admitting: a curated binding must still be named by the
// listing directory to pass C2, else it is refused under
// contract_curated_binding_without_independent_listing.
//
// The two arms' candidate sets are DISJOINT: an address the curated directory recognises is
// evaluated by the directory arm and removed here, or its market cap would be counted twice.

// ─── storage seam ───────────────────────────────────────────────────

// RWAListingDirectoryReader is the seam the second C2 arm reads its corroborating source
// through; *timescale.Store satisfies it. It is optional: without it the first arm is served
// alone and the arm reports itself UNMEASURED, not a population of zero, because a zero would
// assert that no independent listing names any of these addresses, a finding nobody made.
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
	// census includes the rows storage refused to serve as stale, the visible form of the
	// fail-closed guarantee: a set that shrank because the sync stopped must say so.
	census timescale.ListingDirectoryCensus
	// available is false when the read failed or the snapshot is past its bound: no naming was
	// established, so every candidate needing one is refused.
	available bool
	// wired reports that a listing reader is CONFIGURED, whatever it answered. Unwired means
	// nobody looked, and an arm of zeros would wrongly assert that no independent party names
	// these addresses. A wired reader that failed DID look; its failure is a measured shrink
	// the funnel must report loudly, since an operator can fix it.
	wired bool
	// read reports that the directory ANSWERED, making `census` a real observation. A failed
	// read observed nothing; zero entries observed means the table was never synced.
	read bool
	// observedAt is when the read behind `census` was taken. It distinguishes "the directory is
	// empty" from "this set was built before the directory filled". Zero when unobserved.
	observedAt time.Time
}

// names reports whether the listing directory names this exact address; an unavailable snapshot
// has no opinion about any address, so it is false.
func (l rwaListing) names(contractID string) bool {
	if !l.available {
		return false
	}
	_, ok := l.byAddress[contractID]
	return ok
}

// rwaListingSnapshot reads the listing directory once per rebuild.
//
// Fails CLOSED: a failed read yields an UNAVAILABLE snapshot, never a carried-forward one. An
// old price is a labelled approximation, but an old recognition admits an asset on a fact nobody
// re-established, which is what C2 exists to prevent. The reader enforces the staleness bound in
// SQL, so a snapshot arriving here is already inside it.
func (s *Server) rwaListingSnapshot(ctx context.Context) rwaListing {
	if s.RWAListings == nil {
		return rwaListing{}
	}
	rows, census, err := s.RWAListings.ListingDirectoryContracts(ctx)
	if err != nil {
		s.logger.Warn("rwa listing directory read failed", "err", err)
		return rwaListing{wired: true}
	}
	out := rwaListing{
		byAddress: make(map[string]timescale.ListingEntry, len(rows)),
		census:    census,
		// Zero fresh rows cannot support "nobody independent names this address": the reader drops
		// stale rows, so a fully stale table looks like a listing naming nothing. As available,
		// bindings would be refused under contract_curated_binding_without_independent_listing (a
		// finding about the world) when our copy merely expired; as unavailable, under
		// independent_listing_unavailable, an outage an operator can fix. A never-synced table
		// takes the same branch.
		available:  len(rows) > 0,
		wired:      true,
		read:       true,
		observedAt: time.Now().UTC(),
	}
	if !out.available {
		// Carried onto the census below and served, so "is the sync down, or is this set older
		// than the sync" is answerable without database access.
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
	// dirTags are the curated tags on the address, empty when the directory does not name it.
	// Looked up even though the directory is not this arm's recognition source, because the scam
	// vocabulary lives there and NOWHERE else; skipping it would make scam flags inert here.
	dirTags []string
}

// rwaListingCensus is the second arm's accounting.
type rwaListingCensus struct {
	// bindings is every in-repo curated contract binding, the root of this arm's narrowing.
	bindings int
	// alsoInDirectoryArm counts bindings the first arm already evaluates, removed here.
	alsoInDirectoryArm int
	// listingUnavailable counts bindings refused because the LISTING read did not answer.
	// A failed curated-tag lookup is counted apart (tagsUnavailable): the two sources fail
	// independently, and folding them would send an operator to a working sync.
	listingUnavailable int
	// tagsUnavailable counts bindings refused because the CURATED DIRECTORY's tag read did
	// not answer, leaving C3 unevaluated.
	tagsUnavailable int
	// notListed counts bindings the listing directory does not name: the refusal that holds
	// independence up.
	notListed int
	// evaluated is what reached rwa.QualifyContract.
	evaluated int
	// listedWithoutBinding counts addresses the listing directory names that no curated binding
	// does: a TERMINAL census, not part of the narrowing. It is the population an operator
	// reviews to grow the set, and publishing it keeps "the listing admits nothing on its own"
	// auditable.
	listedWithoutBinding int
	// available is false when the arm was not walked at all.
	available bool

	// dir is the storage layer's census of the whole listing directory, so the arm publishes
	// evidence and not only a verdict. A zero corroborated count is ambiguous; these counts
	// separate never synced, sync stopped, and a healthy directory this set predates:
	//
	//   - Entries 0                 -> never synced
	//   - Entries > 0, Stale = all  -> the sync stopped
	//   - Entries > 0, Stale 0      -> healthy, and this set predates it
	//
	// Zero when nothing was observed; observedAt says which.
	dir timescale.ListingDirectoryCensus
	// observedAt is when the read behind `dir` was taken, so readers can compare observations.
	// Zero means nothing was observed and publishes no timestamp rather than the Unix epoch.
	observedAt time.Time
}

// rwaListingCandidates builds the second arm's population: every curated binding the first arm
// is not already evaluating, with the curated tags C3 needs.
//
// The curated-directory lookup is required: C3 refuses scam-flagged addresses and scam flags
// exist only there, so a candidate whose tags could not be read has not had C3 evaluated and
// must not be admitted. A failed lookup closes the arm under its OWN refusal reason, never the
// listing's, because a refusal reason tells an operator which system to inspect.
func (s *Server) rwaListingCandidates(
	ctx context.Context, listing rwaListing, directoryEvaluated map[string]struct{},
) ([]rwaListingCandidate, rwaListingCensus) {
	// Not wired is NOT a finding: the arm reports itself unwalked and makes no refusals. A wired
	// reader that FAILED is the opposite: it could not answer, so every binding it would have
	// corroborated is refused under a reason an operator can act on.
	if !listing.wired {
		return nil, rwaListingCensus{}
	}
	bindings := rwa.ContractInstrumentBindings()
	census := rwaListingCensus{bindings: len(bindings), available: true}
	// Carried only from a read that ANSWERED: a zero census from a failed read would read as a
	// directory holding no rows, a finding from a query that did not complete.
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

	// The terminal census counts the whole listing snapshot, not the pending set: its subject
	// is the listing's contents, not this arm's narrowing.
	for addr := range listing.byAddress {
		if _, ok := bound[addr]; !ok {
			census.listedWithoutBinding++
		}
	}

	tags, tagsOK := s.rwaDirectoryTagsFor(ctx, pending)
	out := make([]rwaListingCandidate, 0, len(pending))
	for _, addr := range pending {
		// Ordered by which source is missing, most specific first. The listing read is this arm's
		// RECOGNITION source, so its absence closes the arm whatever the curated directory said;
		// only when the listing answered can a tag failure be the blocker, and that must not be
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

// rwaDirectoryTagsFor batch-reads the curated tags for a set of addresses, reporting whether
// the read answered at all. ok=false is NOT "no tags": an empty list means the directory does
// not flag the address, a failed read means nobody looked, and confusing them admits an asset
// on the mere absence of a scam flag. An absent reader is the same: C3 cannot be evaluated, so
// the arm closes.
func (s *Server) rwaDirectoryTagsFor(ctx context.Context, addrs []string) (map[string][]string, bool) {
	if len(addrs) == 0 {
		return map[string][]string{}, true
	}
	if s.Directory == nil {
		return nil, false
	}
	found, err := s.Directory.DirectoryEntriesByAddresses(ctx, addrs)
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
