package completeness

// Watermark is the per-source completeness verdict (ADR-0033 headline).
// Ledger is the highest ledger such that every completeness claim holds
// contiguously from Genesis: substrate continuity + hash chain
// (Claim 1), recognition (Claim 2a), and projection (Claim 2b). It
// replaces density_pct / gap_free_pct as the confidence signal — there
// is no sparsity threshold here; a single proven problem pins the
// watermark and CoveragePct is honest by construction.
type Watermark struct {
	Genesis     uint32
	Tip         uint32
	Ledger      uint32  // highest fully-verified ledger; == Genesis-1 if a problem sits at Genesis
	CoveragePct float64 // (Ledger-Genesis+1)/(Tip-Genesis+1), clamped [0,1]
	Complete    bool    // Ledger >= Tip — verified all the way to tip
	// FirstProblem is the earliest ledger (>= Genesis) where a claim
	// fails, or 0 when none — i.e. exactly where to look / backfill.
	FirstProblem uint32
}

// ComputeWatermark reduces the set of "problem ledgers" (the earliest
// ledger of each substrate gap / hash-chain break / recognition gap /
// projection mismatch found in [Genesis, Tip]) into the completeness
// watermark. The watermark is one below the earliest problem at or
// after Genesis; if there are no problems it reaches Tip.
//
// Pure and deterministic: the same inputs always yield the same
// verdict, so it is auditable and re-runnable (a Proof-of-Indexing
// analogue). Problems below Genesis are ignored (out of this source's
// scope); problems above Tip are ignored (not yet in range).
func ComputeWatermark(genesis, tip uint32, problemLedgers []uint32) Watermark {
	w := Watermark{Genesis: genesis, Tip: tip}
	if tip < genesis {
		// Degenerate range (no ledgers): nothing to verify.
		w.Ledger = genesis
		if genesis > 0 {
			w.Ledger = genesis - 1
		}
		w.CoveragePct = 0
		return w
	}

	first := uint32(0)
	have := false
	for _, p := range problemLedgers {
		if p < genesis || p > tip {
			continue
		}
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
	// Verified up to one before the earliest problem.
	if first == 0 {
		w.Ledger = 0
	} else {
		w.Ledger = first - 1
	}
	if w.Ledger < genesis {
		// Problem at (or before) genesis → zero verified coverage.
		w.CoveragePct = 0
		// Keep Ledger as genesis-1 sentinel for "nothing verified".
		if genesis > 0 {
			w.Ledger = genesis - 1
		} else {
			w.Ledger = 0
		}
		return w
	}

	span := float64(tip - genesis + 1)
	verified := float64(w.Ledger - genesis + 1)
	w.CoveragePct = verified / span
	if w.CoveragePct < 0 {
		w.CoveragePct = 0
	}
	if w.CoveragePct > 1 {
		w.CoveragePct = 1
	}
	return w
}
