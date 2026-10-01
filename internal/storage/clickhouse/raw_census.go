// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"sort"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// RawTableShortfall is one raw-table partition holding fewer rows than
// stellar.ledgers declares for it.
type RawTableShortfall struct {
	Table string
	// Partition is intDiv(ledger_seq, 1_000_000).
	Partition uint32
	// Expected is the Σ of the table's header count over the partition's
	// ledgers; for a presence-only table it is 1 (rows were owed, none exist).
	Expected uint64
	// Present is the partition's active-part row count (system.parts).
	Present uint64
}

// RawTableTotal is one table's range-wide expected vs present, printed so
// duplicate inflation is visible beside a pass.
type RawTableTotal struct {
	Table             string
	Expected, Present uint64
	PresenceOnly      bool
}

// rawCensusHeader is one partition's Σ of the per-ledger header counts.
type rawCensusHeader struct{ tx, op, ev uint64 }

// rawCensusSpec binds a raw table to the header count that declares its rows.
// A presence-only table has no exact per-ledger count in the header: it owes
// rows wherever the partition has operations, but fewer than op_count.
type rawCensusSpec struct {
	table        string
	presenceOnly bool
	expected     func(rawCensusHeader) uint64
}

// rawCensusTables is the census population in report order: every per-ledger
// raw table except ledgers (the reference) and ledger_entry_changes (checked
// by verify-lake's entry-changes coverage).
var rawCensusTables = []rawCensusSpec{
	{table: "transactions", expected: func(h rawCensusHeader) uint64 { return h.tx }},
	{table: "operations", expected: func(h rawCensusHeader) uint64 { return h.op }},
	{table: "contract_events", expected: func(h rawCensusHeader) uint64 { return h.ev }},
	{table: "operation_results", presenceOnly: true, expected: func(h rawCensusHeader) uint64 { return h.op }},
	{table: "operation_participants", presenceOnly: true, expected: func(h rawCensusHeader) uint64 { return h.op }},
}

// RawTableCensus cross-checks the five non-ledger raw tables against the
// per-partition counts stellar.ledgers declares, over the partitions [from, to]
// touches. Present counts un-merged ReplacingMergeTree duplicates, so a
// shortfall proves loss while a pass bounds loss only to present − expected.
func RawTableCensus(ctx context.Context, addr string, from, to uint32) ([]RawTableShortfall, []RawTableTotal, error) {
	if from > to {
		return nil, nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()

	expected := make(map[uint32]rawCensusHeader)
	err = forEachLedgerWindow(from-from%eventCensusPartitionWidth, to, eventCensusPartitionWidth, func(lo, hi uint32) error {
		return rawCensusExpected(ctx, conn, lo, hi, expected)
	})
	if err != nil {
		return nil, nil, err
	}
	// Read after every expected window: the sink writes raw tables before
	// ledgers, so a batch landing between the reads can only raise present.
	tables := make([]string, len(rawCensusTables))
	for i, s := range rawCensusTables {
		tables[i] = s.table
	}
	present, err := rawCensusPresent(ctx, conn, tables...)
	if err != nil {
		return nil, nil, err
	}
	short, totals := rawCensusShortfalls(expected, present)
	return short, totals, nil
}

func rawCensusExpected(ctx context.Context, conn driver.Conn, lo, hi uint32, into map[uint32]rawCensusHeader) error {
	const q = `
		SELECT toUInt32(intDiv(ledger_seq, 1000000)) AS p,
		       toUInt64(sum(tx)), toUInt64(sum(op)), toUInt64(sum(ev))
		FROM (
			SELECT ledger_seq,
			       argMax(tx_count, ingested_at) AS tx,
			       argMax(op_count, ingested_at) AS op,
			       argMax(soroban_event_count, ingested_at) AS ev
			FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
			GROUP BY ledger_seq
		)
		GROUP BY p`
	rows, err := conn.Query(ctx, q, lo, hi)
	if err != nil {
		return fmt.Errorf("clickhouse: raw census expected [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p uint32
		var h rawCensusHeader
		if err := rows.Scan(&p, &h.tx, &h.op, &h.ev); err != nil {
			return fmt.Errorf("clickhouse: scan raw census expected: %w", err)
		}
		acc := into[p]
		into[p] = rawCensusHeader{tx: acc.tx + h.tx, op: acc.op + h.op, ev: acc.ev + h.ev}
	}
	return rows.Err()
}

// rawCensusPresent returns active-part row counts per (table, partition) for
// the named stellar tables, from one system.parts read.
func rawCensusPresent(ctx context.Context, conn driver.Conn, tables ...string) (map[string]map[uint32]uint64, error) {
	const q = `
		SELECT table, toUInt32(partition) AS p, toUInt64(sum(rows))
		FROM system.parts
		WHERE database = 'stellar' AND active AND table IN (?)
		GROUP BY table, p`
	rows, err := conn.Query(ctx, q, tables)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: raw census present: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]map[uint32]uint64, len(tables))
	for rows.Next() {
		var table string
		var p uint32
		var n uint64
		if err := rows.Scan(&table, &p, &n); err != nil {
			return nil, fmt.Errorf("clickhouse: scan raw census present: %w", err)
		}
		if out[table] == nil {
			out[table] = make(map[uint32]uint64)
		}
		out[table][p] = n
	}
	return out, rows.Err()
}

// rawCensusShortfalls compares expected header sums with present row counts;
// a partition absent from present reads as 0 rows. Output is in
// rawCensusTables order, then by partition. Pure.
func rawCensusShortfalls(expected map[uint32]rawCensusHeader, present map[string]map[uint32]uint64) ([]RawTableShortfall, []RawTableTotal) {
	parts := make([]uint32, 0, len(expected))
	for p := range expected {
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i] < parts[j] })

	var short []RawTableShortfall
	totals := make([]RawTableTotal, 0, len(rawCensusTables))
	for _, spec := range rawCensusTables {
		tot := RawTableTotal{Table: spec.table, PresenceOnly: spec.presenceOnly}
		for _, p := range parts {
			want := spec.expected(expected[p])
			if want == 0 {
				continue
			}
			got := present[spec.table][p]
			tot.Expected += want
			tot.Present += got
			switch {
			case spec.presenceOnly && got == 0:
				short = append(short, RawTableShortfall{Table: spec.table, Partition: p, Expected: 1, Present: 0})
			case !spec.presenceOnly && got < want:
				short = append(short, RawTableShortfall{Table: spec.table, Partition: p, Expected: want, Present: got})
			}
		}
		totals = append(totals, tot)
	}
	return short, totals
}
