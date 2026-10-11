package orchestrator

import (
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
)

// A release must not clear a sibling it has never seen. The marker is pair-scoped with one ladder per window, and "last window" was decided from o.freezeStates alone, which misses windows not yet at the freeze step in this process (under the volume floor, or after a restart).
// Clearing then retired an escalated freeze that ADR-0019 holds until manual unfreeze, leaving nothing to re-fire on.

func coldSiblingDecision() anomaly.Decision {
	return anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassCrypto,
		Reason: "phase2:3_signal_AND",
	}
}
