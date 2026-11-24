package v1

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwaIssuerRows is one issuer's member codes resolved to listing rows.
type rwaIssuerRows struct {
	keep        []timescale.AssetRow
	memberCodes []string
	at          time.Time
	// tailUnchecked: the page filled and a member missing by exact spelling could not be checked for a case variant past it.
	tailUnchecked bool
	// reads are the catalogue listings behind keep, re-checked for a failed refresh at build and at serve.
	reads []timescale.ListAssetsOptions
}

// notePage folds one issuer page's freshness, truncation and catalogue reads into the join.
func (j *rwaCatalogueJoin) notePage(issuer string, got rwaIssuerRows, stale bool) {
	j.unanswered = j.unanswered || stale
	j.reads = append(j.reads, got.reads...)
	if !got.at.IsZero() && (j.observedAt.IsZero() || got.at.Before(j.observedAt)) {
		j.observedAt = got.at
	}
	if got.tailUnchecked {
		j.pagesTruncated++
		if j.fullPageIssuers == nil {
			j.fullPageIssuers = map[string]bool{}
		}
		j.fullPageIssuers[issuer] = true
	}
}

// rwaIssuerMemberRows reads one issuer's listing page and, when the page is full, every member it missed by exact
// (code, issuer), then by its one case variant. Anyone can open trustlines that fill the page, so a page alone can push a
// member out. Bounded by one page, one batched variant probe and two indexed reads per missed member.
func (s *Server) rwaIssuerMemberRows(ctx context.Context, issuer string, codes []string) (rwaIssuerRows, error) {
	opts := timescale.ListAssetsOptions{Limit: rwaAssetsPerIssuer, Issuer: issuer, Type: "classic"}
	page, at, _, err := s.listAssetsExtAt(ctx, opts)
	if err != nil {
		return rwaIssuerRows{}, err
	}
	out := rwaIssuerRows{at: at, reads: []timescale.ListAssetsOptions{opts}}
	out.keep, out.memberCodes = rwaMatchMemberRows(codes, page)
	if len(page) < rwaAssetsPerIssuer {
		return out, nil
	}
	var missed []string
	for _, c := range codes {
		if !slices.Contains(out.memberCodes, c) {
			missed = append(missed, c)
		}
	}
	missed, err = s.rwaAddExactRows(ctx, issuer, missed, &out)
	if err != nil || len(missed) == 0 {
		return out, err
	}
	reader, ok := s.Sep1Cache.(classicCaseVariantReader)
	if !ok {
		out.tailUnchecked = true
		return out, nil
	}
	return out, s.rwaAddFoldedRows(ctx, reader, issuer, missed, &out)
}

// rwaAddFoldedRows adds, for each code with no exact row, the one on-chain spelling folding to it.
func (s *Server) rwaAddFoldedRows(
	ctx context.Context, reader classicCaseVariantReader, issuer string, missed []string, out *rwaIssuerRows,
) error {
	keys := make([]timescale.ClassicCodeKey, len(missed))
	for i, c := range missed {
		keys[i] = timescale.ClassicCodeKey{Issuer: issuer, Code: c}
	}
	spellings, err := reader.ClassicCodesFoldingTo(ctx, keys)
	if err != nil {
		return fmt.Errorf("rwa member case-variant read: %w", err)
	}
	// A spelling two members fold to, or one a member already holds exactly, goes to neither.
	claims := map[string][]string{}
	for i, c := range missed {
		if j := matchAssetCode(c, spellings[keys[i]], true); j >= 0 && !slices.Contains(out.keepCodes(), spellings[keys[i]][j]) {
			claims[spellings[keys[i]][j]] = append(claims[spellings[keys[i]][j]], c)
		}
	}
	for spelling, members := range claims {
		if len(members) != 1 {
			continue
		}
		row, found, err := s.rwaExactClassicRow(ctx, out, issuer, spelling)
		if err != nil {
			return err
		}
		if found {
			out.keep = append(out.keep, row)
			out.memberCodes = append(out.memberCodes, members[0])
		}
	}
	return nil
}

// rwaAddExactRows adds the row spelled exactly as each code to out and returns the codes with none.
func (s *Server) rwaAddExactRows(ctx context.Context, issuer string, codes []string, out *rwaIssuerRows) ([]string, error) {
	var missed []string
	for _, c := range codes {
		row, found, err := s.rwaExactClassicRow(ctx, out, issuer, c)
		if err != nil {
			return nil, err
		}
		if !found {
			missed = append(missed, c)
			continue
		}
		out.keep = append(out.keep, row)
		out.memberCodes = append(out.memberCodes, c)
	}
	return missed, nil
}

func (r rwaIssuerRows) keepCodes() []string {
	codes := make([]string, len(r.keep))
	for i := range r.keep {
		codes[i] = r.keep[i].Code
	}
	return codes
}

// rwaExactClassicRow is the classic row spelled exactly code under issuer, by the listing's indexed, case-sensitive Code filter;
// the read joins out.reads.
func (s *Server) rwaExactClassicRow(ctx context.Context, out *rwaIssuerRows, issuer, code string) (timescale.AssetRow, bool, error) {
	opts := timescale.ListAssetsOptions{Limit: 2, Issuer: issuer, Code: code, Type: "classic"}
	out.reads = append(out.reads, opts)
	rows, _, _, err := s.listAssetsExtAt(ctx, opts)
	if err != nil {
		return timescale.AssetRow{}, false, err
	}
	for _, row := range rows {
		if row.Code == code {
			return row, true, nil
		}
	}
	return timescale.AssetRow{}, false, nil
}
