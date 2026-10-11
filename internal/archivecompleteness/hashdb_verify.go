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

	// Verified counts ledgers whose fresh hash matched the one hashdb recorded on first
	// observation.
	Verified int

	// Drifted counts ledgers whose fresh hash did NOT match the recorded value, the alert
	// condition. Either upstream rewrote a fetched ledger or the local copy is corrupted;
	// escalate per the hashdb runbook.
	Drifted int

	// Missing counts ledgers hashdb never recorded (the window predates hashdb or the indexer).
	// Not an error: there is no baseline to compare against.
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

// HashDBWindowVerifier accumulates a [HashDBVerifyResult] as the caller feeds it one
// (ledger_seq, fresh sha256(LCM)) pair at a time.
//
// It is transport-agnostic: re-reading the bucket and hashing each ledger is the caller's job, so
// this package stays free of the ledger-meta xdr dependency that lint-imports.sh rule
// B/xdr-scoped-to-scval polices (ADR-0013).
//
// Not safe for concurrent Observe calls, matching hashdb.DB's single-writer discipline.
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

// Observe checks one ledger's fresh hash against hashdb's recorded value. It errors only on a
// hashdb I/O failure, never on a mismatch: drift is data, and an operator investigating a
// suspected rewrite wants every drifted ledger, so the caller's walk should continue past a hit.
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
