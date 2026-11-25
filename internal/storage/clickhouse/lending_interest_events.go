package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
)

// LendingInterestCauseTopics are the topic[0] symbols of every cause in lendinginterest.CauseRules.
func LendingInterestCauseTopics() []string {
	var out []string
	for _, r := range lendinginterest.CauseRules {
		if !slices.Contains(out, r.Event) {
			out = append(out, r.Event)
		}
	}
	slices.Sort(out)
	return out
}

// LendingInterestEvents streams the pools' cause events of [lo, hi] under FINAL, one row per event identity, in
// apply order. Only the pools' own contract ids are read; the caller still checks each event's contract. The
// pools emit their topics as Symbols, and the census counts with this same predicate.
func LendingInterestEvents(ctx context.Context, addr string, lo, hi uint32, pools []string, fn func(events.Event) error) error {
	if len(pools) == 0 {
		return nil
	}
	where := contractEventsWhere(pools, symbolTopic0Predicate(LendingInterestCauseTopics()), nil)
	return streamContractEventsWhere(ctx, addr, lo, hi, pools, where, true, false, false, false, fn)
}

// ErrLendingInterestEventShortfall refuses a window whose residual ledgers' contract events are not all in the lake.
var ErrLendingInterestEventShortfall = errors.New("clickhouse: lending interest residual ledger lacks contract events")

// lendingInterestEventCensusChunk bounds one census query's IN list.
const lendingInterestEventCensusChunk = 5000

// lendingInterestEventCensusQuery counts, per ledger, the events stellar.ledgers declares (soroban_event_count, deduped
// per ledger as cap67WindowEventCensusQuery does), the distinct event identities contract_events holds, and those of
// them that are the pools' cause events. Binds: ledgers, ledgers, ledgers, pools, topics.
const lendingInterestEventCensusQuery = `
	SELECT l.ledger_seq, toUInt64(l.cnt), toUInt64(e.n), toUInt64(e.causes)
	FROM (
		SELECT ledger_seq, argMax(soroban_event_count, ingested_at) AS cnt
		FROM stellar.ledgers WHERE ledger_seq IN ? GROUP BY ledger_seq
	) AS l
	LEFT JOIN (
		SELECT ledger_seq, uniqExact((tx_hash, op_index, event_index)) AS n,
		       uniqExactIf((tx_hash, op_index, event_index), contract_id IN ? AND topic_0_sym IN ?) AS causes
		FROM stellar.contract_events WHERE ledger_seq IN ? GROUP BY ledger_seq
	) AS e ON l.ledger_seq = e.ledger_seq
	SETTINGS join_use_nulls = 0`

// LendingInterestEventsCovered proves, for every ledger of read, that the pass's read of the pools' cause events
// missed none: contract_events holds exactly the events stellar.ledgers declares for the ledger, and exactly as many
// cause events of pools as read says the pass streamed there. A ledger with no stellar.ledgers row refuses.
func LendingInterestEventsCovered(ctx context.Context, addr string, pools []string, read map[uint32]uint64) error {
	if len(read) == 0 {
		return nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	pSet, tSet := anySet(pools), anySet(LendingInterestCauseTopics())
	for chunk := range slices.Chunk(slices.Sorted(maps.Keys(read)), lendingInterestEventCensusChunk) {
		if err := eventCensusChunk(ctx, conn, chunk, pSet, tSet, read); err != nil {
			return err
		}
	}
	return nil
}

func eventCensusChunk(ctx context.Context, conn driver.Conn, chunk []uint32, pools, topics clickhouse.GroupSet, read map[uint32]uint64) error {
	set := anySet(chunk)
	rows, err := conn.Query(ctx, lendingInterestEventCensusQuery, set, pools, topics, set)
	if err != nil {
		return fmt.Errorf("clickhouse: lending interest event census: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[uint32]bool{}
	for rows.Next() {
		var ledger uint32
		var declared, present, causes uint64
		if err := rows.Scan(&ledger, &declared, &present, &causes); err != nil {
			return fmt.Errorf("clickhouse: lending interest event census: %w", err)
		}
		seen[ledger] = true
		if err := censusVerdict(ledger, declared, present, causes, read[ledger]); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: lending interest event census: %w", err)
	}
	for _, l := range chunk {
		if !seen[l] {
			return fmt.Errorf("%w: ledger %d has no stellar.ledgers row", ErrLendingInterestEventShortfall, l)
		}
	}
	return nil
}

func anySet[T any](vs []T) clickhouse.GroupSet {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = v
	}
	return clickhouse.GroupSet{Value: out}
}

func censusVerdict(ledger uint32, declared, present, causes, read uint64) error {
	switch {
	case present != declared:
		return fmt.Errorf("%w: ledger %d holds %d of the %d contract events stellar.ledgers declares",
			ErrLendingInterestEventShortfall, ledger, present, declared)
	case causes != read:
		return fmt.Errorf("%w: ledger %d holds %d cause events of the pools, the pass read %d (the lake changed under the read)",
			ErrLendingInterestEventShortfall, ledger, causes, read)
	}
	return nil
}
