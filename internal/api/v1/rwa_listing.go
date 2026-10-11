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
// Enumerating is not admitting: a curated binding must still be named by the listing directory to
// pass C2, else it is refused under contract_curated_binding_without_independent_listing. The two
// arms' candidate sets are disjoint: an address the curated directory recognises is evaluated by
// the directory arm and removed here, or its market cap would be counted twice.

// ─── storage seam ───────────────────────────────────────────────────

// RWAListingDirectoryReader is the seam the second C2 arm reads its corroborating source
// through; *timescale.Store satisfies it. It is optional: without it the arm reports itself
// unmeasured, not a population of zero, because a zero would assert that no independent listing
// names any of these addresses.
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
	// wired reports that a listing reader is configured, whatever it answered. Unwired means nobody
	// looked; a wired reader that failed is a measured shrink the funnel must report loudly.
	wired bool
	// read reports that the directory answered, making `census` a real observation. A failed read
	// observed nothing; zero entries observed means the table was never synced.
	read bool
	// observedAt is when the read behind `census` was taken, to tell "the directory is empty" from
	// "this set was built before the directory filled". Zero when unobserved.
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
// Fails CLOSED: a failed read yields an unavailable snapshot, never a carried-forward one, since
// an old recognition admits an asset on a fact nobody re-established, which is what C2 exists to
// prevent. The reader enforces the staleness bound in SQL.
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
		// The reader drops stale rows, so a fully stale (or never-synced) table looks like a listing
		// naming nothing. As available, bindings would be refused under
		// contract_curated_binding_without_independent_listing (a finding about the world) when our
		// copy merely expired; as unavailable, under independent_listing_unavailable, which an
		// operator can fix.
		available:  len(rows) > 0,
		wired:      true,
		read:       true,
		observedAt: time.Now().UTC(),
	}
	if !out.available {
		// Carried onto the served census, so "sync down" versus "set older than the sync" is
		// answerable without database access.
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

// rwaListingCandidate is one address the second arm will evaluate: a curated binding, plus the
// curated-directory tags C3 has to read for it.
type rwaListingCandidate struct {
	contractID string
	// dirTags are the curated tags on the address. Looked up even though the directory is not this
	// arm's recognition source, because the scam vocabulary lives only there.
	dirTags []string
}

// rwaListingCensus is the second arm's accounting.
type rwaListingCensus struct {
	// bindings is every in-repo curated contract binding, the root of this arm's narrowing.
	bindings int
	// alsoInDirectoryArm counts bindings the first arm already evaluates, removed here.
	alsoInDirectoryArm int
	// listingUnavailable counts bindings refused because the listing read did not answer. A failed
	// curated-tag lookup is counted apart (tagsUnavailable): the two sources fail independently,
	// and folding them would send an operator to a working sync.
	listingUnavailable int
	// tagsUnavailable counts bindings refused because the curated directory's tag read did not
	// answer, leaving C3 unevaluated.
	tagsUnavailable int
	// notListed counts bindings the listing directory does not name: the refusal that holds
	// independence up.
	notListed int
	// evaluated is what reached rwa.QualifyContract.
	evaluated int
	// listedWithoutBinding counts addresses the listing names that no curated binding does: a
	// terminal census, not part of the narrowing, that keeps "the listing admits nothing on its
	// own" auditable.
	listedWithoutBinding int
	// available is false when the arm was not walked at all.
	available bool

	// dir is the storage layer's census of the whole listing directory. A zero corroborated count
	// is ambiguous; these counts separate never synced, sync stopped, and a healthy directory this
	// set predates:
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
// exist only there, so a candidate whose tags could not be read must not be admitted. A failed
// lookup closes the arm under its own refusal reason, never the listing's, because the reason
// tells an operator which system to inspect.
func (s *Server) rwaListingCandidates(
	ctx context.Context, listing rwaListing, directoryEvaluated map[string]struct{},
) ([]rwaListingCandidate, rwaListingCensus) {
	// Not wired is not a finding: the arm reports itself unwalked and makes no refusals. A wired
	// reader that failed refuses every binding it would have corroborated.
	if !listing.wired {
		return nil, rwaListingCensus{}
	}
	bindings := rwa.ContractInstrumentBindings()
	census := rwaListingCensus{bindings: len(bindings), available: true}
	// Only from a read that answered: a zero census from a failed read would read as an empty
	// directory.
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

	// The terminal census counts the whole listing snapshot, not the pending set.
	for addr := range listing.byAddress {
		if _, ok := bound[addr]; !ok {
			census.listedWithoutBinding++
		}
	}

	tags, tagsOK := s.rwaDirectoryTagsFor(ctx, pending)
	out := make([]rwaListingCandidate, 0, len(pending))
	for _, addr := range pending {
		// The listing read is this arm's recognition source, so its absence closes the arm; only
		// when it answered can a tag failure be the blocker, and that must not be reported as a
		// listing outage.
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
// the read answered at all. ok=false is not "no tags": confusing a failed read with an empty list
// admits an asset on the mere absence of a scam flag. An absent reader is the same: C3 cannot be
// evaluated, so the arm closes.
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
