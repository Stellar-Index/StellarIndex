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

// listingScales is one page's lake token scales from a single batched read. A nil *listingScales reads each row through
// [Server.lakeTokenDecimals]; a non-nil err means the batch failed and every non-SAC Soroban row is unread, never undeclared.
type listingScales struct {
	declared map[string]int
	err      error
}

// readListingScales batch-reads the lake scale of every non-SAC Soroban row; nil when the reader has no batch seam.
func (s *Server) readListingScales(ctx context.Context, rows []AssetDetail) *listingScales {
	br, ok := s.TokenDecimals.(tokenDecimalsBatchReader)
	if !ok {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for i := range rows {
		id := rows[i].AssetID
		if rows[i].Type != string(canonical.AssetSoroban) || seen[id] {
			continue
		}
		seen[id] = true
		if _, sac := s.resolveSACAsset(id); !sac {
			ids = append(ids, id)
		}
	}
	ls := &listingScales{declared: map[string]int{}}
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
	if _, sac := s.resolveSACAsset(contractID); sac || ls == nil {
		return s.lakeTokenDecimals(ctx, contractID)
	}
	if ls.err != nil {
		return 0, false, ls.err
	}
	d, ok := ls.declared[contractID]
	return d, ok, nil
}
