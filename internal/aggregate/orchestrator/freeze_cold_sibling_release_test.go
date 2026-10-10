package orchestrator

import (
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
)

// ─── a release must not clear a sibling it has never seen ─────────
//
// The marker is pair-scoped and carries one ladder per window, so the
// last window to release deletes it. "Last" was decided from
// o.freezeStates alone — and a window only enters that map when it reaches
// the freeze step IN THIS PROCESS. A window sitting under the USD-volume
// floor never does, and after a restart no window has yet. Such a window's
// freeze exists only in the marker, so the in-memory check could not see
// it: a sibling's release deleted the marker AND retired the durable
// ladder, and an ESCALATED freeze — which ADR-0019 holds "until manual
// unfreeze" — ended because a different window recovered. That is the
// under-freeze direction: the window's next qualifying bucket finds no
// marker, no ladder, and (cold, so no prev-VWAP comparator) nothing to
// re-fire on, and publishes.

func coldSiblingDecision() anomaly.Decision {
	return anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassCrypto,
		Reason: "phase2:3_signal_AND",
	}
}
