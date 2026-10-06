package chops

import "github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

// unprovenCarryTargets names the targets a carried prefix would vouch for
// without evidence: present in the served tier, not reconciled down to their
// bottom edge by this run, and with no durable floor at or below that edge.
// floorsToRecord writes a floor only after a clean run reached the edge, so a
// missing or higher floor means the target was added to the catalogue, or had
// rows projected below its old floor, since the last full reconcile.
//
// Not named, because no floor can tell the cases apart: an empty target
// (floorsToRecord never floors one), a target whose rows all start inside this
// run's window, and a target removed and later re-added (it keeps its old floor).
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
