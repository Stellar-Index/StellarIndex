package oracleagg

// DeviationKind names how an adapter program checks a feed price against an earlier one before returning it.
type DeviationKind string

// The deviation rules of the audited programs.
const (
	// DeviationNone: the price is returned unchecked (v1; v2 and the fork when max_dev is 0 or >= 100; base assets).
	DeviationNone DeviationKind = "none"
	// DeviationPreviousPricedPeriod is v2.0.0: the first earlier period carrying a price, within MaxAge/resolution
	// lookups; a move of exactly max_dev% passes.
	DeviationPreviousPricedPeriod DeviationKind = "previous_priced_period"
	// DeviationNewestTwoPricedRounds is the YieldBlox fork: the feed's newest record against the one before it, with
	// no age bound on the earlier one beyond the feed's 256-period history; a move of exactly max_dev% is refused.
	DeviationNewestTwoPricedRounds DeviationKind = "newest_two_priced_rounds"
)

// DeviationRule is the deviation check an adapter applies to one asset.
type DeviationRule struct {
	Kind DeviationKind
	// MaxDevPercent is the configured max_dev; 0 when Kind is none.
	MaxDevPercent uint32
	// MaxLookbackPeriods is how many periods before the returned price the comparison price may lie (v2: the
	// adapter's cached resolution; the fork: the feed's).
	MaxLookbackPeriods uint64
	// RejectsAtLimit is true when a move of exactly MaxDevPercent is refused.
	RejectsAtLimit bool
}

// DeviationRule describes the check cfg's program applies to asset; ok=false for an unconfigured asset or an
// unaudited program.
func (c Config) DeviationRule(asset Asset) (DeviationRule, bool) {
	if c.IsBase(asset) {
		return DeviationRule{Kind: DeviationNone}, true
	}
	rule, ok := c.Assets[asset]
	if !ok {
		return DeviationRule{}, false
	}
	checked := rule.MaxDev > 0 && rule.MaxDev < 100
	switch c.Program {
	case ProgramV1:
		return DeviationRule{Kind: DeviationNone}, true
	case ProgramV2:
		if !checked {
			return DeviationRule{Kind: DeviationNone}, true
		}
		d := DeviationRule{Kind: DeviationPreviousPricedPeriod, MaxDevPercent: rule.MaxDev}
		if rule.ResolutionSeconds > 0 {
			d.MaxLookbackPeriods = c.MaxAge / uint64(rule.ResolutionSeconds)
		}
		return d, true
	case ProgramYBForkV2:
		if !checked {
			return DeviationRule{Kind: DeviationNone}, true
		}
		return DeviationRule{
			Kind: DeviationNewestTwoPricedRounds, MaxDevPercent: rule.MaxDev,
			MaxLookbackPeriods: reflectorHistoryPeriods - 1, RejectsAtLimit: true,
		}, true
	case ProgramUnknown:
	}
	return DeviationRule{}, false
}
