package band

import "testing"

// DefaultResolutionSeconds is not documentation — it is published as
// `stellarindex_oracle_resolution_seconds{source="band"}` and the
// `stellarindex_oracle_stale` rule alerts at 10× it (see the expr in
// configs/prometheus/rules.r1/divergence.yml and its multi-host twin).
// So the constant IS this source's alert threshold, and getting it
// wrong does not fail a build — it produces an alert that is either
// permanently firing or permanently blind.
//
// It was 60, copied from a poll-cadence RECOMMENDATION (how often a
// consumer might ask) rather than the relayer's publication interval.
// Band publishes hourly, so the threshold sat at 10 minutes and
// `stellarindex_oracle_stale{source="band"}` fired for 100% of samples
// over 7 days on both crypto:USDC and crypto:XLM.
//
// These tests pin the RELATIONSHIP that was violated, not the number.
// A test that only asserted `== 3600` would pass just as happily on a
// future value that is wrong in the same way.

// staleAlertMultiplier mirrors the `10 *` in the oracle-stale rule
// expression. If that multiplier changes, this must change with it —
// the two together decide when band tickets.
const staleAlertMultiplier = 10

// measuredRelayIntervalSeconds is Band's observed mainnet cadence,
// from r1 on 2026-09-01:
//
//	changes(stellarindex_oracle_last_update_unix{source="band"}[24h])
//	  → 24 for crypto:USDC and 24 for crypto:XLM, i.e. hourly.
const measuredRelayIntervalSeconds = 3600

// TestStaleThresholdIsLooserThanBandsActualCadence is the regression.
// The alert must not fire while Band is behaving normally. With the
// old value the threshold was 600s against a 3600s cadence, so the
// alert was true for ~50 minutes of every hour — which is what a
// 100%-firing week looks like.
func TestStaleThresholdIsLooserThanBandsActualCadence(t *testing.T) {
	threshold := staleAlertMultiplier * DefaultResolutionSeconds
	if threshold <= measuredRelayIntervalSeconds {
		t.Fatalf("oracle-stale threshold for band is %ds, but Band relays every "+
			"%ds — the alert fires during NORMAL operation and carries no "+
			"information. DefaultResolutionSeconds must describe the observed "+
			"publication interval, not a poll-cadence recommendation.",
			threshold, measuredRelayIntervalSeconds)
	}
}

// TestResolutionIsNotAbsurdlyLoose is the other side, and it is the
// half that stops this fix from becoming the opposite bug. Silencing a
// noisy alert by inflating its threshold is always available and always
// wrong; the constant has to stay close to the real cadence so a
// genuine Band outage is still detected in reasonable time.
func TestResolutionIsNotAbsurdlyLoose(t *testing.T) {
	if DefaultResolutionSeconds > 4*measuredRelayIntervalSeconds {
		t.Errorf("DefaultResolutionSeconds = %d is more than 4× Band's measured "+
			"%ds cadence. That is silencing, not fixing: at 10× it would take "+
			"%.1f hours to notice Band had stopped.",
			DefaultResolutionSeconds, measuredRelayIntervalSeconds,
			float64(staleAlertMultiplier*DefaultResolutionSeconds)/3600)
	}
}

// TestResolutionIsPositive — a zero or negative resolution makes the
// threshold zero or negative, so `time() - last_update > 0` is true
// forever. The gauge is set unconditionally at dispatcher registration
// (pipeline.BuildDispatcher), so a bad constant reaches production
// without any other check catching it.
func TestResolutionIsPositive(t *testing.T) {
	if DefaultResolutionSeconds <= 0 {
		t.Fatalf("DefaultResolutionSeconds = %d; a non-positive resolution makes "+
			"the stale alert fire permanently for every asset", DefaultResolutionSeconds)
	}
}
