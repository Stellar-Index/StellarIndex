package v1

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// The producer bounds must cap the DATABASE QUERY RATE the registry can
// drive, not only the producer COUNT. Each producer computes on a ticker
// of window_seconds, and window_seconds is client-chosen down to 1s, so a
// count ceiling sized against the default 5s window admits five times its
// intended query rate when every slot is minted at window_seconds=1.
//
// The budget these tests hold the registry to is the one the count caps
// were sized against: every slot filled at the default window.
//
// Proven red against the count-only registry: 512 producers at a 1s window
// reached 30720 ticks/min against a 6144 budget, and one caller's 24
// producers reached 1440 against 288.

// tipTestTicksPerMinute sums the compute rate of the registered producers
// minted by caller ("" = every producer), charging a producer on a w-second
// ticker ceil(60/w) ticks per minute.
func tipTestTicksPerMinute(reg *tipProducerRegistry, caller string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	total := 0
	for key, p := range reg.active {
		if caller != "" && p.minter != caller {
			continue
		}
		total += (60 + key.window - 1) / key.window
	}
	return total
}

func TestTipProducerRegistry_AggregateQueryRateIsBoundedNotJustTheCount(t *testing.T) {
	budget := defaultMaxTipProducers * (60 / defaultTipWindowSeconds)
	reg := &tipProducerRegistry{lingerFor: time.Hour}

	sawRateRefusal := false
	// Many addresses (RFC 5737 TEST-NET-3), each minting 1s-window
	// producers on distinct pairs and aborting at once — the shape that
	// leaves every producer lingering while the count caps still admit it.
	for c := 1; c <= 64; c++ {
		caller := "203.0.113." + strconv.Itoa(c)
		for j := 0; j < 30; j++ {
			key := tipProducerKey{
				asset: "asset-" + strconv.Itoa(c) + "-" + strconv.Itoa(j),
				quote: "fiat:USD", window: minTipWindowSeconds,
			}
			release, outcome := reg.acquireFor(key, caller, nil,
				func(ctx context.Context) { <-ctx.Done() })
			if outcome == tipProducerAdmitted {
				release()
				continue
			}
			if outcome.String() == "global_rate_budget" {
				sawRateRefusal = true
			}
		}
	}

	if got := tipTestTicksPerMinute(reg, ""); got > budget {
		t.Errorf("registered producers drive %d ticks/min against a %d budget — "+
			"the count ceiling alone lets 1s windows multiply the DB query rate",
			got, budget)
	}
	if !sawRateRefusal {
		t.Error("no acquire was refused with reason global_rate_budget; a flood " +
			"stopped by the rate budget must be told apart from the count ceiling")
	}
}

func TestTipProducerRegistry_OneCallerCannotBuyRateWithShortWindows(t *testing.T) {
	const attempts = 40
	budget := defaultMaxTipProducersPerCaller * (60 / defaultTipWindowSeconds)
	reg := &tipProducerRegistry{lingerFor: time.Hour}

	admitted, rateRefusals := 0, 0
	for j := 0; j < attempts; j++ {
		key := tipProducerKey{
			asset: "asset-" + strconv.Itoa(j), quote: "fiat:USD", window: minTipWindowSeconds,
		}
		release, outcome := reg.acquireFor(key, attackerCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		switch {
		case outcome == tipProducerAdmitted:
			admitted++
			release()
		case outcome.String() == "caller_rate_budget":
			rateRefusals++
		}
	}

	if got := tipTestTicksPerMinute(reg, attackerCaller); got > budget {
		t.Errorf("one caller's producers drive %d ticks/min against its %d share — "+
			"short windows let it buy %dx the rate the per-caller quota was sized for",
			got, budget, got/budget)
	}
	if rateRefusals != attempts-admitted {
		t.Errorf("caller_rate_budget refusals = %d, want %d", rateRefusals, attempts-admitted)
	}
	if got := reg.refusedPerCallerCount(); got != uint64(attempts-admitted) {
		t.Errorf("refusedPerCallerCount() = %d, want %d — a per-caller rate refusal "+
			"is still a per-caller refusal", got, attempts-admitted)
	}

	// The rate share must not undercut the count quota at the default
	// window: a legitimate viewer keeps every producer it could mint before.
	for j := 0; j < defaultMaxTipProducersPerCaller; j++ {
		key := tipProducerKey{
			asset: "asset-" + strconv.Itoa(j), quote: "fiat:EUR", window: defaultTipWindowSeconds,
		}
		release, outcome := reg.acquireFor(key, bystanderCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		if outcome != tipProducerAdmitted {
			t.Fatalf("default-window producer %d refused (%s) below the count quota %d",
				j, outcome, defaultMaxTipProducersPerCaller)
		}
		release()
	}
}
