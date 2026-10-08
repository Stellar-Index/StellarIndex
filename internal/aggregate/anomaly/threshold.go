package anomaly

import (
	"fmt"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Thresholds holds a class's absolute percentages: above WarnPct warns; above
// FreezePct with at most one source freezes. Both > 0 and FreezePct > WarnPct.
type Thresholds struct {
	WarnPct   float64
	FreezePct float64
}

// Validate checks that the thresholds are well-formed.
func (t Thresholds) Validate() error {
	if t.WarnPct <= 0 {
		return fmt.Errorf("anomaly: WarnPct must be > 0, got %g", t.WarnPct)
	}
	if t.FreezePct <= 0 {
		return fmt.Errorf("anomaly: FreezePct must be > 0, got %g", t.FreezePct)
	}
	if t.FreezePct <= t.WarnPct {
		return fmt.Errorf("anomaly: FreezePct (%g) must be > WarnPct (%g)",
			t.FreezePct, t.WarnPct)
	}
	return nil
}

// DefaultThresholds is the ADR-0019 per-class baseline, used where config omits a class.
func DefaultThresholds() map[AssetClass]Thresholds {
	return map[AssetClass]Thresholds{
		ClassStablecoin: {WarnPct: 1.0, FreezePct: 3.0},
		ClassTreasury:   {WarnPct: 1.0, FreezePct: 3.0},
		ClassCrypto:     {WarnPct: 20.0, FreezePct: 50.0},
		ClassGovernance: {WarnPct: 50.0, FreezePct: 100.0},
		ClassDefault:    {WarnPct: 30.0, FreezePct: 75.0},
	}
}

// Checker decides whether a bucket's VWAP is anomalous; safe for concurrent use.
type Checker struct {
	thresholds map[AssetClass]Thresholds
	classifier *Classifier
}

// NewChecker validates thresholds, which must include the [ClassDefault] fallback.
func NewChecker(thresholds map[AssetClass]Thresholds, classifier *Classifier) (*Checker, error) {
	if classifier == nil {
		return nil, fmt.Errorf("anomaly: classifier is required")
	}
	if _, ok := thresholds[ClassDefault]; !ok {
		return nil, fmt.Errorf("anomaly: thresholds map must include ClassDefault (the fallback)")
	}
	for cls, t := range thresholds {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("anomaly: thresholds[%s]: %w", cls, err)
		}
	}
	cp := make(map[AssetClass]Thresholds, len(thresholds))
	for k, v := range thresholds {
		cp[k] = v
	}
	return &Checker{thresholds: cp, classifier: classifier}, nil
}

// ClassOf returns the asset's class, so freeze-path metrics share Evaluate's labels.
func (c *Checker) ClassOf(asset canonical.Asset) AssetClass {
	return c.classifier.ClassOf(asset)
}

// Observation is the input to [Checker.Evaluate]. The aggregator
// fills this in for each bucket-close before publishing.
type Observation struct {
	// Pair is the asset pair being evaluated. The Checker uses
	// Pair.Base.String() to look up the asset's class.
	Pair canonical.Pair

	// PrevVWAP is the previous closed 1-minute bucket's VWAP, never a rolling
	// window's previous value (that damps the move). Nil means no prior bucket: allow.
	PrevVWAP *big.Rat

	// CurrVWAP is the new closed bucket's VWAP. Nil is invalid —
	// the caller must compute SOMETHING before asking whether to
	// publish it.
	CurrVWAP *big.Rat

	// SourceCount is how many distinct sources contributed to
	// CurrVWAP. The Phase-1 freeze condition fires only when
	// SourceCount <= 1 (single-source signature of manipulation).
	SourceCount int
}

// thresholdsFor returns the threshold row for the asset's class,
// falling back to ClassDefault if the class isn't in the table.
func (c *Checker) thresholdsFor(class AssetClass) Thresholds {
	if t, ok := c.thresholds[class]; ok {
		return t
	}
	return c.thresholds[ClassDefault]
}

// Evaluate returns a [Decision] for obs (ADR-0019). A deviation at or above
// FreezePct freezes only on a single source; with corroborating sources it is a
// real market move and only warns.
func (c *Checker) Evaluate(obs Observation) Decision {
	class := c.classifier.ClassOf(obs.Pair.Base)
	thresholds := c.thresholdsFor(class)

	if obs.PrevVWAP == nil {
		return Decision{
			Action:       ActionAllow,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: 0,
			Reason:       "no prior bucket — first observation for pair",
		}
	}
	if obs.CurrVWAP == nil {
		// Caller bug — the aggregator should never publish a nil
		// VWAP. Fail-safe to ActionFreeze so the upstream code
		// notices.
		return Decision{
			Action:       ActionFreeze,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: 0,
			Reason:       "nil CurrVWAP — caller bug",
		}
	}

	deviation := computeDeviationPct(obs.PrevVWAP, obs.CurrVWAP)

	switch {
	case deviation < thresholds.WarnPct:
		return Decision{
			Action:       ActionAllow,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: deviation,
			Reason:       "deviation within normal range",
		}
	case deviation < thresholds.FreezePct:
		return Decision{
			Action:       ActionWarn,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: deviation,
			Reason: fmt.Sprintf("deviation %.2f%% above warn threshold %.2f%% for class %s",
				deviation, thresholds.WarnPct, class),
		}
	case obs.SourceCount > 1:
		return Decision{
			Action:       ActionWarn,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: deviation,
			Reason: fmt.Sprintf("deviation %.2f%% above freeze threshold %.2f%% but %d sources corroborate (real market move)",
				deviation, thresholds.FreezePct, obs.SourceCount),
		}
	default:
		return Decision{
			Action:       ActionFreeze,
			Class:        class,
			Thresholds:   thresholds,
			DeviationPct: deviation,
			Reason: fmt.Sprintf("deviation %.2f%% above freeze threshold %.2f%% on single source — possible manipulation",
				deviation, thresholds.FreezePct),
		}
	}
}

// computeDeviationPct returns abs((curr - prev) / prev) * 100. Both
// inputs must be non-nil and prev must be non-zero (caller's
// responsibility — Evaluate guards this).
func computeDeviationPct(prev, curr *big.Rat) float64 {
	if prev.Sign() == 0 {
		// CAGGs don't materialise empty buckets, but treat any move off zero as huge.
		if curr.Sign() == 0 {
			return 0
		}
		return 1e9
	}
	delta := new(big.Rat).Sub(curr, prev)
	delta.Abs(delta)
	delta.Quo(delta, prev)
	// Abs again: dividing by a negative prev flips the sign, and a negative deviation
	// passes every threshold. Unreachable for VWAPs; guards any signed series.
	delta.Abs(delta)
	delta.Mul(delta, big.NewRat(100, 1))
	f, _ := delta.Float64() // i128:ok percentage move for the anomaly threshold compare, not an amount
	return f
}
