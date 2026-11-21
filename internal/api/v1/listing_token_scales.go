// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// errTokenDecimalsUnwired is [Server.lakeTokenDecimals]'s error when no lake reader is wired (the boot dial missed ClickHouse,
// and it stays nil until restart): the scale is unread, never "declares none".
var errTokenDecimalsUnwired = errors.New("token decimals: no lake reader wired")

// tokenDecimalsBatchReader is the optional seam a listing page reads all its Soroban scales through in one query.
type tokenDecimalsBatchReader interface {
	TokenDecimalsBatch(ctx context.Context, contractIDs []string) (map[string]uint32, error)
}

var _ tokenDecimalsBatchReader = (*clickhouse.ExplorerReader)(nil)

// protocolSAC reports a contract known without a lake read to be a Stellar Asset Contract, 7 decimals by protocol: the SAC of
// native or a verified asset, or a configured wrapper (the alias registry proves each one's derivation at load).
func (s *Server) protocolSAC(contractID string) bool {
	if _, ok := s.resolveSACAsset(contractID); ok {
		return true
	}
	a, err := canonical.NewSorobanAsset(contractID)
	return err == nil && canonical.CanonicalAsset(a).Type != canonical.AssetSoroban
}

// listingScales is one page's token scales. sacs holds the SAC address derived from each classic row on the page, so an
// unverified asset's SAC row is 7 by protocol rather than a lake read. Without a batch seam (batched=false) each other row
// reads through [Server.lakeTokenDecimals]; a non-nil err means the batch failed and those rows are unread, never undeclared.
type listingScales struct {
	declared map[string]int
	sacs     map[string]bool
	batched  bool
	err      error
}

// readListingScales derives the page's SACs and batch-reads the lake scale of every other Soroban row when the reader can.
func (s *Server) readListingScales(ctx context.Context, rows []AssetDetail) *listingScales {
	ls := &listingScales{declared: map[string]int{}, sacs: map[string]bool{}}
	for i := range rows {
		if sac, ok := derivedSAC(rows[i].AssetID); ok {
			ls.sacs[sac] = true
		}
	}
	br, ok := s.TokenDecimals.(tokenDecimalsBatchReader)
	if !ok {
		return ls
	}
	ls.batched = true
	var ids []string
	seen := make(map[string]bool)
	for i := range rows {
		id := rows[i].AssetID
		if rows[i].Type != string(canonical.AssetSoroban) || seen[id] {
			continue
		}
		seen[id] = true
		if !ls.sacs[id] && !s.protocolSAC(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ls
	}
	got, err := br.TokenDecimalsBatch(ctx, ids)
	if err != nil {
		s.logger.Warn("listing token decimals read failed; the page's soroban scales are unresolved", "contracts", len(ids), "err", err)
		ls.err = err
		return ls
	}
	for id, d := range got {
		ls.declared[id] = int(d)
	}
	return ls
}

// lakeScale is [Server.lakeTokenDecimals] answered from the page's batch when there is one.
func (s *Server) lakeScale(ctx context.Context, ls *listingScales, contractID string) (decimals int, known bool, err error) {
	if ls != nil && ls.sacs[contractID] {
		return defaultTokenDecimals, true, nil // the SAC of a classic row on this page: its scale is fixed by the protocol
	}
	if ls == nil || !ls.batched || s.protocolSAC(contractID) {
		return s.lakeTokenDecimals(ctx, contractID)
	}
	if ls.err != nil {
		return 0, false, ls.err
	}
	d, ok := ls.declared[contractID]
	return d, ok, nil
}
