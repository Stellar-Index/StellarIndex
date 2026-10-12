// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sdexServedPK is the served trades primary key minus its per-ledger constants (source "sdex", ts = close time): within a ledger ON
// CONFLICT reduces to (tx_hash, op_index). The 1024 op_index fanout stride collides for >1024-claim ops; the served tier de-dups those.
type sdexServedPK struct {
	tx string
	op uint32
}

// sdexServedCensus is the per-ledger set of priceable SDEX trades (passes canonical.Trade.Validate, not a one-side-zero fill) keyed by
// served primary key; the only SDEX projection oracle, since the ledger_ingest_log census keeps one-side-zero fills. Served side counts via
// sdexPriceableFilter.
type sdexServedCensus map[uint32]map[sdexServedPK]struct{}

func (c sdexServedCensus) add(outs []consumer.Event) {
	for _, ev := range outs {
		te, ok := ev.(sdex.TradeEvent)
		// One-side-zero fills are stored, but ledgers written before they were
		// admitted hold none; a full-history ch-rebuild -sdex retires this arm.
		if !ok || te.Trade.Validate() != nil || timescale.IsOneSideZeroFill(te.Trade) {
			continue
		}
		s := c[te.Trade.Ledger]
		if s == nil {
			s = make(map[sdexServedPK]struct{})
			c[te.Trade.Ledger] = s
		}
		s[sdexServedPK{tx: te.Trade.TxHash, op: te.Trade.OpIndex}] = struct{}{}
	}
}

func (c sdexServedCensus) addTo(out map[uint32]int) {
	for ledger, s := range c {
		out[ledger] += len(s)
	}
}

// sdexProjectionExpected is the SDEX expected-row oracle for callers that don't prove the lake substrate: it refuses a range whose lake
// ledgers are not contiguous and hash-linked (a missing ledger would read as zero expected trades), else re-derives through the served
// projection.
func sdexProjectionExpected(ctx context.Context, chAddr string, lo, hi uint32) (map[uint32]int, completeness.BlindSpots, error) {
	problem, has, detail, err := clickhouse.SubstrateProblem(ctx, chAddr, lo, hi)
	if err != nil {
		return nil, completeness.BlindSpots{}, fmt.Errorf("sdex: lake substrate check: %w", err)
	}
	if has {
		return nil, completeness.BlindSpots{}, fmt.Errorf("sdex: ClickHouse lake substrate problem at ledger %d in [%d,%d] (%s) — the SDEX projection re-derives from the lake's operations; run ch-backfill first",
			problem, lo, hi, detail)
	}
	return reDeriveSDEXCensusViaDecoder(ctx, chAddr, lo, hi)
}
