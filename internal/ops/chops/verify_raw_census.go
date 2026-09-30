// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// runRawCensusCheck prints verify-lake's raw-table census section and returns
// the number of short (table, partition) pairs.
func runRawCensusCheck(ctx context.Context, addr string, from, to uint32) (uint64, error) {
	short, totals, err := clickhouse.RawTableCensus(ctx, addr, from, to)
	if err != nil {
		return 0, err
	}
	fmt.Print(renderRawCensus(from, to, short, totals))
	return uint64(len(short)), nil
}

func renderRawCensus(from, to uint32, short []clickhouse.RawTableShortfall, totals []clickhouse.RawTableTotal) string {
	perTable := make(map[string]int, len(totals))
	for _, s := range short {
		perTable[s.Table]++
	}
	out := fmt.Sprintf("\n=== raw-table census [%d,%d] vs stellar.ledgers headers (1M-ledger partitions, active-part rows) ===\n", from, to)
	for _, t := range totals {
		if t.PresenceOnly {
			out += fmt.Sprintf("  %-23s presence-only (rows owed wherever op_count>0) present=%d short_partitions=%d\n", t.Table, t.Present, perTable[t.Table])
			continue
		}
		ratio := 0.0
		if t.Expected > 0 {
			ratio = float64(t.Present) / float64(t.Expected)
		}
		out += fmt.Sprintf("  %-23s expected=%d present=%d (x%.2f) short_partitions=%d\n", t.Table, t.Expected, t.Present, ratio, perTable[t.Table])
	}
	for _, s := range short {
		lo := uint64(s.Partition) * 1_000_000
		out += fmt.Sprintf("  SHORT %s partition=%d ledgers=[%d,%d] expected=%d present=%d\n", s.Table, s.Partition, lo, lo+999_999, s.Expected, s.Present)
	}
	out += "  note: present includes un-merged duplicates — a shortfall proves loss; a pass bounds loss to present−expected. " +
		"Confirm a suspect range exactly with `stellarindex-ops ch-gate -config PATH -from LO -to HI`.\n"
	return out
}
