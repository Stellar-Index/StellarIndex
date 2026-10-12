package chops

import "github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

// unprovenCarryTargets names targets a carried prefix would vouch for without evidence: served, not reconciled to their bottom edge this
// run, with no durable floor at or below it. Not named (no floor can tell): empty targets, targets whose rows all start in this window,
// removed-then-re-added targets.
func unprovenCarryTargets(src reconSource, scopes []projectionScope, servedMins []servedFloor, floors map[string]timescale.CompletenessTargetFloor) []string {
	var out []string
	for i, tgt := range src.targets {
		if i >= len(scopes) || i >= len(servedMins) {
			break
		}
		sm := servedMins[i]
		if !sm.present || scopes[i].From <= sm.min {
			continue
		}
		if f, ok := floors[timescale.TargetFloorKey(src.name, tgt.table, tgt.whereFilter)]; ok && f.VerifiedFrom <= sm.min {
			continue
		}
		out = append(out, targetLabel(tgt.table, tgt.whereFilter))
	}
	return out
}

// claimScope is what a projection claim may vouch for: text names the targets
// this run counted, and unproven lists targets a carry would cover unproven.
type claimScope struct {
	text     string
	unproven []string
}

func newClaimScope(src reconSource, scopes []projectionScope, servedMins []servedFloor, floors map[string]timescale.CompletenessTargetFloor) claimScope {
	return claimScope{text: src.projectionScope(scopes), unproven: unprovenCarryTargets(src, scopes, servedMins, floors)}
}
