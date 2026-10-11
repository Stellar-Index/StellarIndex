package completeness

// firstLedger is Stellar's first ledger sequence; no ledger 0 exists, which
// is what lets FirstProblem use 0 as its "none" sentinel.
const firstLedger uint32 = 1

// Watermark is the per-source completeness verdict (ADR-0033 headline). Ledger is the highest
// ledger such that every claim whose problem ledgers were passed to ComputeWatermark holds
// contiguously from Genesis; it attests nothing beyond that set. The -ch path passes substrate +
// recognition (projection failures are not localised and gate only Complete, via
// combineWatermark), the legacy Postgres path also passes per-ledger projection mismatches, and
// the system recognition snapshot passes recognition alone. There is no sparsity threshold: a
// single proven problem pins the watermark.
type Watermark struct {
	Genesis     uint32 // effective floor: the caller's genesis, raised to firstLedger
	Tip         uint32
	Ledger      uint32  // highest fully-verified ledger; == Genesis-1 if a problem sits at Genesis
	CoveragePct float64 // (Ledger-Genesis+1)/(Tip-Genesis+1), clamped [0,1]
	Complete    bool    // Ledger >= Tip — verified all the way to tip
	// FirstProblem is the earliest ledger (>= Genesis) where a claim
	// fails, or 0 when none — i.e. exactly where to look / backfill.
	FirstProblem uint32
}

// ComputeWatermark reduces the "problem ledgers" (earliest ledger of each substrate gap,
// hash-chain break, recognition gap or projection mismatch in [Genesis, Tip]) to the completeness
// watermark: one below the earliest problem at or after Genesis, or Tip when there is none.
//
// Pure and deterministic, so the verdict is auditable and re-runnable. Problems below Genesis
// (out of scope) or above Tip (not yet in range) are ignored. A genesis of 0 is raised to
// firstLedger, and an in-scope problem at ledger 0 is placed there too, so a located problem never
// reads as the 0 "none" sentinel.
func ComputeWatermark(genesis, tip uint32, problemLedgers []uint32) Watermark {
	lo := max(genesis, firstLedger)
	w := Watermark{Genesis: lo, Tip: tip}
	if tip < lo {
		// Degenerate range (no ledgers): nothing to verify.
		w.Ledger = lo - 1
		w.CoveragePct = 0
		return w
	}

	first := uint32(0)
	have := false
	for _, p := range problemLedgers {
		if p < genesis || p > tip {
			continue
		}
		p = max(p, lo)
		if !have || p < first {
			first = p
			have = true
		}
	}

	if !have {
		w.Ledger = tip
		w.Complete = true
		w.CoveragePct = 1
		return w
	}

	w.FirstProblem = first
	// Verified up to one before the earliest problem; first >= lo >= 1.
	w.Ledger = first - 1
	if first == lo {
		// Problem at genesis → zero verified coverage.
		w.CoveragePct = 0
		return w
	}

	span := float64(tip - lo + 1)
	verified := float64(w.Ledger - lo + 1)
	w.CoveragePct = min(max(verified/span, 0), 1)
	return w
}
