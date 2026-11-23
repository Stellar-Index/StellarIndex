package v1

import (
	"context"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// dropClassicMemberSACs removes from c a contract member that is an admitted classic member's SAC: one
// asset, already valued on its classic row. The refusal lands in the tally of the arm that admitted it.
// An unreadable listing page marks the build read-failed, so it is never cached.
func (s *Server) dropClassicMemberSACs(ctx context.Context, out *rwaMembership, c rwaContractBuild) {
	sacs, ok := s.classicMemberSACs(ctx, out.members)
	if !ok {
		out.readFailed = true
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
	out.contracts = kept
}

// classicMemberSACs is the SAC of each listing row the catalogue join ([Server.rwaListingRows]) resolves a classic member to.
// Codes are case-sensitive, so the id comes from the row's real code, never from a guessed spelling of the declared one.
func (s *Server) classicMemberSACs(ctx context.Context, members []rwaMember) (map[string]struct{}, bool) {
	sacs := make(map[string]struct{}, len(members))
	if s.AssetsReader == nil {
		return sacs, true
	}
	codesByIssuer := make(map[string][]string, len(members))
	for _, mem := range members {
		codesByIssuer[mem.issuer] = append(codesByIssuer[mem.issuer], strings.TrimSpace(mem.code))
	}
	for issuer, codes := range codesByIssuer {
		rows, _, _, err := s.listAssetsExtAt(ctx, timescale.ListAssetsOptions{Limit: rwaAssetsPerIssuer, Issuer: issuer, Type: "classic"})
		if err != nil {
			return sacs, false
		}
		keep, _ := rwaMatchMemberRows(codes, rows)
		for _, row := range keep {
			if id, err := (canonical.Asset{Type: canonical.AssetClassic, Code: row.Code, Issuer: issuer}).SacContractID(); err == nil {
				sacs[id] = struct{}{}
			}
		}
	}
	return sacs, true
}
