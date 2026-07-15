package archivecompleteness

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
)

// HashDBVerifyResult is the outcome of a [HashDBWindowVerifier] pass.
// Mirrors the shape of [CrossAnchorResult]: counts first, a bounded
// sample of the interesting sequences second.
type HashDBVerifyResult struct {
	// From, To — the ledger range that was walked (inclusive).
	From, To uint32

	// Verified counts ledgers whose freshly-computed hash matched the
	// value hashdb recorded when the indexer first observed them.
	// This is the overwhelming majority in the healthy case — see
	// hashdb's TestVerify_OK doc: "drift is rare; agreement is the
	// norm."
	Verified int

	// Drifted counts ledgers whose freshly-computed hash did NOT
	// match the recorded value — the alert condition this package
	// exists to surface. Non-zero means either upstream rewrote a
	// previously-fetched ledger's bytes, or the copy we're reading
	// now is locally corrupted; either way, escalate (see the
	// hashdb-drift-detected runbook).
	Drifted int

	// Missing counts ledgers hashdb has no record for (never
	// appended — e.g. the window reaches back before hashdb was
	// enabled, or before the indexer started). Not an error; just
	// means "no baseline to compare against yet."
	Missing int

	// OutOfRange counts ledgers below hashdb's StartLedger — the
	// caller's window reached further back than the file covers.
	// Same non-error treatment as Missing.
	OutOfRange int

	// DriftSeqs lists the drifted ledger sequences, ascending, capped
	// at [MaxHashDBDriftSeqsReported] so a catastrophic drift event
	// doesn't balloon the result. Drifted is always the true count
	// even when this slice is truncated.
	DriftSeqs []uint32
}

// AnyDrift reports whether the pass found at least one drifted
// ledger — the alert condition.
func (r HashDBVerifyResult) AnyDrift() bool {
	return r.Drifted > 0
}

// MaxHashDBDriftSeqsReported caps HashDBVerifyResult.DriftSeqs —
// mirrors [MaxMissingReported]'s rationale: bound the payload even
// under a worst-case "every ledger in the window drifted" scenario.
const MaxHashDBDriftSeqsReported = 64

// HashDBWindowVerifier accumulates a [HashDBVerifyResult] as the
// caller feeds it one (ledger_seq, freshly-computed sha256(LCM)) pair
// at a time. Per internal/hashdb's package doc: "the indexer reads
// each LCM ... appends a record. A periodic verifier later re-reads
// the same bucket, recomputes sha256, and compares" — this type is
// the compare half.
//
// Deliberately transport-agnostic: it does NOT itself re-read the
// datastore or depend on xdr.LedgerCloseMeta. Re-reading the bucket
// (typically via ledgerstream.Stream) and computing each ledger's
// hash (hashdb.Hash(lcm.MarshalBinary())) is the caller's job — see
// cmd/stellarindex-indexer's periodic verify sweep for the production
// wiring. This keeps internal/archivecompleteness free of the
// ledger-meta xdr dependency scripts/ci/lint-imports.sh's
// B/xdr-scoped-to-scval rule polices (ADR-0013): only packages on the
// ledger-meta plumbing path (ledgerstream, dispatcher, pipeline, the
// indexer/ops binaries) are allow-listed for it, and this package
// isn't ledger-meta plumbing — it's a comparison, fed pre-hashed
// input.
//
// Not safe for concurrent Observe calls — same single-writer
// discipline as hashdb.DB itself.
type HashDBWindowVerifier struct {
	db  *hashdb.DB
	res HashDBVerifyResult
}

// NewHashDBWindowVerifier returns a verifier that will check ledgers
// in [from, to] (inclusive) against db as the caller calls Observe.
// db is read-only from this type's perspective — Observe only ever
// calls db.Verify, never db.Append.
func NewHashDBWindowVerifier(db *hashdb.DB, from, to uint32) *HashDBWindowVerifier {
	return &HashDBWindowVerifier{db: db, res: HashDBVerifyResult{From: from, To: to}}
}

// Observe checks one ledger's freshly-computed hash against hashdb's
// recorded value and updates the running result. Returns an error
// only for a hashdb I/O failure (opening/reading the file itself) —
// NOT for a hash mismatch. Drift is data, not an error: an operator
// investigating a suspected rewrite wants the full picture (how many
// ledgers, which ones), so Observe keeps accepting calls after a
// drift hit rather than forcing the caller to abort its walk.
func (v *HashDBWindowVerifier) Observe(seq uint32, observed [32]byte) error {
	if v.db == nil {
		return errors.New("archivecompleteness: hashdb verify requires a non-nil DB")
	}
	verr := v.db.Verify(seq, observed)
	switch {
	case verr == nil:
		v.res.Verified++
	case errors.Is(verr, hashdb.ErrDrift):
		v.res.Drifted++
		if len(v.res.DriftSeqs) < MaxHashDBDriftSeqsReported {
			v.res.DriftSeqs = append(v.res.DriftSeqs, seq)
		}
	case errors.Is(verr, hashdb.ErrMissing):
		v.res.Missing++
	case errors.Is(verr, hashdb.ErrOutOfRange):
		v.res.OutOfRange++
	default:
		// Anything else (I/O error against the hashdb file itself) is
		// not a drift signal — it's "we couldn't even check", a
		// different failure mode that should surface distinctly
		// rather than being silently folded into a count.
		return fmt.Errorf("archivecompleteness: hashdb verify ledger %d: %w", seq, verr)
	}
	return nil
}

// Result returns the accumulated outcome so far. Safe to call at any
// point, including mid-walk (e.g. to log partial progress).
func (v *HashDBWindowVerifier) Result() HashDBVerifyResult {
	return v.res
}
