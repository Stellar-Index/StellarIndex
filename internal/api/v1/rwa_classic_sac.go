package v1

import (
	"context"
	"maps"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
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

// withoutServedClassicSACs is [Server.dropClassicMemberSACs] against the classic rows a view serves: the membership's dedupe
// read an earlier issuer page, so a row catalogued since would leave its SAC valued as a second asset. Only a contract whose
// classic row is in classic is dropped, so the asset keeps a row. The tallies are copied: m is the cached membership's.
func withoutServedClassicSACs(m rwaMembership, classic []RWAAsset) rwaMembership {
	sacs := make(map[string]struct{}, len(classic))
	for _, a := range classic {
		if a.ContractID != "" || a.Code == "" || a.Issuer == "" {
			continue
		}
		if id, err := (canonical.Asset{Type: canonical.AssetClassic, Code: a.Code, Issuer: a.Issuer}).SacContractID(); err == nil {
			sacs[id] = struct{}{}
		}
	}
	kept := m.contracts[:0:0]
	for _, c := range m.contracts {
		if _, dup := sacs[c.contractID]; !dup {
			kept = append(kept, c)
		}
	}
	if len(kept) == len(m.contracts) {
		return m
	}
	m.refusals, m.contractRefusals, m.listingRefusals = maps.Clone(m.refusals), maps.Clone(m.contractRefusals), maps.Clone(m.listingRefusals)
	for _, c := range m.contracts {
		if _, dup := sacs[c.contractID]; !dup {
			continue
		}
		m.refusals[rwa.RejectContractIsClassicMemberSAC]++
		if c.recognition == rwa.RecognitionCuratedDirectory {
			m.contractRefusals[rwa.RejectContractIsClassicMemberSAC]++
		} else {
			m.listingRefusals[rwa.RejectContractIsClassicMemberSAC]++
		}
	}
	m.contracts = kept
	return m
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
		got, err := s.rwaIssuerMemberRows(ctx, issuer, codes)
		if err != nil {
			return sacs, false
		}
		for _, row := range got.keep {
			if id, err := (canonical.Asset{Type: canonical.AssetClassic, Code: row.Code, Issuer: issuer}).SacContractID(); err == nil {
				sacs[id] = struct{}{}
			}
		}
	}
	return sacs, true
}
