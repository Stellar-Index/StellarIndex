package v1

import (
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// dropClassicMemberSACs removes a contract member that is an admitted classic member's SAC: one
// asset, already valued on its classic row. The refusal lands in the tally of the arm that admitted it.
func dropClassicMemberSACs(classic []rwaMember, c rwaContractBuild) []rwaContractMember {
	sacs := make(map[string]struct{}, len(classic))
	for _, mem := range classic {
		a := canonical.Asset{Type: canonical.AssetClassic, Code: mem.code, Issuer: mem.issuer}
		if id, err := a.SacContractID(); err == nil {
			sacs[id] = struct{}{}
		}
	}
	kept := c.members[:0:0]
	for _, m := range c.members {
		if _, dup := sacs[m.contractID]; !dup {
			kept = append(kept, m)
			continue
		}
		if m.recognition == rwa.RecognitionCuratedDirectory {
			c.refusals[rwa.RejectContractIsClassicMemberSAC]++
		} else {
			c.listingRefusals[rwa.RejectContractIsClassicMemberSAC]++
		}
	}
	return kept
}
