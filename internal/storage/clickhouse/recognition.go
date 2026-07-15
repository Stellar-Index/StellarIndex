package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TopicShape is one distinct (contract_id, topic_0_sym) event shape in the lake,
// with a representative event for recognition (ADR-0033 Claim 2a). The CH lake
// is the complete authoritative source, so a shape-scan over it sees every event
// shape any contract has ever emitted — no Postgres soroban_events scan needed.
type TopicShape struct {
	ContractID string
	Topic0Sym  string
	Count      uint64
	MinLedger  uint32
	MaxLedger  uint32
	EventType  string
	Topics     []string // base64 SCVal topics of a representative event
	DataXDR    string   // base64 SCVal data of that event
}

// Event reconstructs the representative [events.Event] for this shape — enough
// for a decoder's Matches() (Type, ContractID, Topic, Value). It is NOT a full
// event (no ledger/tx identity); recognition only needs the shape.
func (s TopicShape) Event() events.Event {
	return events.Event{
		Type:                     s.EventType,
		ContractID:               s.ContractID,
		Topic:                    s.Topics,
		Value:                    s.DataXDR,
		InSuccessfulContractCall: true,
	}
}

// MaxLedger returns the highest ledger_seq in the lake's ledgers table.
func MaxLedger(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	var hi uint64
	if err := conn.QueryRow(ctx, `SELECT toUInt64(max(ledger_seq)) FROM stellar.ledgers`).Scan(&hi); err != nil {
		return 0, fmt.Errorf("clickhouse: max ledger: %w", err)
	}
	return uint32(hi), nil
}

// recognitionScanWindow is the per-query ledger span for the distinct-shape
// scan: one lake partition (PARTITION BY intDiv(ledger_seq, 1000000)), so each
// window query reads exactly one partition's parts. Windowing is what makes
// the scan's peak memory independent of history size — the lake grows in
// partitions, each of which is scanned by its own bounded query.
const recognitionScanWindow = 1_000_000

// exemplarBatchSize is how many shapes each representative-event fetch covers.
// The batch query's GROUP BY holds at most this many wide exemplar states, so
// the fetch is bounded by the batch size, never by the number of shapes.
const exemplarBatchSize = 200

// shapeKey is the distinct-shape identity the scan accumulates on.
type shapeKey struct{ contract, topic0 string }

// DistinctTopicShapes returns one representative event per distinct
// (contract_id, topic_0_sym) in contract_events over [from,to]. Optionally
// excludes topic[0] symbols (e.g. the CAP-67 classic-token firehose, which the
// enabled protocol decoders don't claim — pass ClassicTokenTopic0Syms to focus
// the audit on protocol shapes). Results are ordered by Count descending.
//
// Structure (2026-07-08 OOM fix): the old single GROUP BY carried
// argMax(topics_xdr)/argMax(data_xdr) exemplar states — one WIDE string pair
// per distinct key — over the whole range in one query. Post-P23/CAP-67 every
// classic asset movement emits events, so the distinct key set and the wide
// read behind it grew until the query died at ANY server memory cap (the
// in-order read pool's buffers scale with parts × width, and are undertracked
// per-query while counted server-wide). Now:
//
//  1. SCAN: per-partition windows GROUP BY only the NARROW identity columns
//     (contract_id, topic_0_sym) + count/min/max — no wide column is read at
//     all — merged Go-side into the distinct set (small: shape identities +
//     counters).
//  2. EXEMPLAR: for each distinct shape, fetch the representative event's
//     bytes with a point read pinned to the shape's MaxLedger (a primary-key
//     range of ONE ledger), batched exemplarBatchSize shapes per query. Same
//     semantics as the old argMax(…, ledger_seq): the newest event's encoding.
//
// Both phases carry boundedScanSettings, so peak memory is bounded regardless
// of history size or server cap; growth costs time, not failures.
func DistinctTopicShapes(ctx context.Context, addr string, from, to uint32, excludeTopic0 []string) ([]TopicShape, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	acc := make(map[shapeKey]*TopicShape)
	scanQ := distinctShapesWindowQuery(excludeTopic0)
	err = forEachLedgerWindow(from, to, recognitionScanWindow, func(lo, hi uint32) error {
		rows, qerr := conn.Query(ctx, scanQ, lo, hi)
		if qerr != nil {
			return fmt.Errorf("clickhouse: distinct topic shapes [%d,%d]: %w", lo, hi, qerr)
		}
		defer func() { _ = rows.Close() }()
		return mergeShapeWindow(rows, acc)
	})
	if err != nil {
		return nil, err
	}

	out := make([]TopicShape, 0, len(acc))
	for _, s := range acc {
		out = append(out, *s)
	}
	// Count-descending like the old ORDER BY cnt DESC; identity tie-break for
	// deterministic output.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].ContractID != out[j].ContractID {
			return out[i].ContractID < out[j].ContractID
		}
		return out[i].Topic0Sym < out[j].Topic0Sym
	})

	if err := fetchShapeExemplars(ctx, conn, out); err != nil {
		return nil, err
	}
	return out, nil
}

// distinctShapesWindowQuery is the phase-1 scan: NARROW identity columns only
// (no topics_xdr/data_xdr/op_args_xdr, no argMax exemplar state), per-query
// bounded settings. Split out so the query shape is unit-testable.
func distinctShapesWindowQuery(excludeTopic0 []string) string {
	where := "WHERE ledger_seq BETWEEN ? AND ?"
	if len(excludeTopic0) > 0 {
		where += " AND topic_0_sym NOT IN (" + sqlQuoteList(excludeTopic0) + ")"
	}
	return fmt.Sprintf(`
		SELECT
			contract_id,
			topic_0_sym,
			count() AS cnt,
			min(ledger_seq) AS lo,
			max(ledger_seq) AS hi
		FROM stellar.contract_events
		%s
		GROUP BY contract_id, topic_0_sym
		%s`, where, boundedScanSettings)
}

// mergeShapeWindow folds one window's distinct-shape rows into the
// accumulated set (sum counts, widen the ledger span).
func mergeShapeWindow(rows driver.Rows, acc map[shapeKey]*TopicShape) error {
	for rows.Next() {
		var (
			contract, topic0 string
			cnt              uint64
			lo, hi           uint32
		)
		if err := rows.Scan(&contract, &topic0, &cnt, &lo, &hi); err != nil {
			return fmt.Errorf("clickhouse: scan topic shape: %w", err)
		}
		k := shapeKey{contract: contract, topic0: topic0}
		s := acc[k]
		if s == nil {
			acc[k] = &TopicShape{ContractID: contract, Topic0Sym: topic0, Count: cnt, MinLedger: lo, MaxLedger: hi}
			continue
		}
		s.Count += cnt
		if lo < s.MinLedger {
			s.MinLedger = lo
		}
		if hi > s.MaxLedger {
			s.MaxLedger = hi
		}
	}
	return rows.Err()
}

// shapeExemplarQuery is the phase-2 representative fetch for one batch of
// shapes: every shape's exemplar is read at its own MaxLedger, so the ledger
// IN-set prunes to |batch| single-ledger primary-key ranges, and the GROUP BY
// holds at most |batch| wide exemplar states. argMax over the matched rows
// picks each shape's max-ledger event — its own MaxLedger is always in the
// IN-set, so the result matches the old whole-range argMax semantics.
func shapeExemplarQuery(shapes []TopicShape) string {
	ledgers := make([]string, 0, len(shapes))
	pairs := make([]string, 0, len(shapes))
	seenLedger := map[uint32]bool{}
	for _, s := range shapes {
		if !seenLedger[s.MaxLedger] {
			seenLedger[s.MaxLedger] = true
			ledgers = append(ledgers, strconv.FormatUint(uint64(s.MaxLedger), 10))
		}
		pairs = append(pairs, "("+sqlQuoteEscaped(s.ContractID)+","+sqlQuoteEscaped(s.Topic0Sym)+")")
	}
	return fmt.Sprintf(`
		SELECT
			contract_id,
			topic_0_sym,
			argMax(event_type, ledger_seq) AS event_type,
			argMax(topics_xdr, ledger_seq) AS topics,
			argMax(data_xdr, ledger_seq)   AS data
		FROM stellar.contract_events
		WHERE ledger_seq IN (%s)
		  AND (contract_id, topic_0_sym) IN (%s)
		GROUP BY contract_id, topic_0_sym
		%s`, strings.Join(ledgers, ","), strings.Join(pairs, ","), boundedScanSettings)
}

// fetchShapeExemplars fills EventType/Topics/DataXDR for every shape in-place,
// exemplarBatchSize shapes per query.
func fetchShapeExemplars(ctx context.Context, conn driver.Conn, shapes []TopicShape) error {
	idx := make(map[shapeKey]int, len(shapes))
	for i, s := range shapes {
		idx[shapeKey{contract: s.ContractID, topic0: s.Topic0Sym}] = i
	}
	for start := 0; start < len(shapes); start += exemplarBatchSize {
		end := start + exemplarBatchSize
		if end > len(shapes) {
			end = len(shapes)
		}
		batch := shapes[start:end]
		rows, err := conn.Query(ctx, shapeExemplarQuery(batch))
		if err != nil {
			return fmt.Errorf("clickhouse: shape exemplars (batch %d..%d): %w", start, end-1, err)
		}
		filled, err := scanShapeExemplars(rows, idx, shapes)
		if err != nil {
			return err
		}
		if filled != len(batch) {
			// The scan just saw every shape at its MaxLedger; a missing exemplar
			// means the read went wrong, and an empty exemplar would surface as
			// a FALSE recognition gap — fail loudly instead.
			return fmt.Errorf("clickhouse: shape exemplars (batch %d..%d): got %d of %d representatives", start, end-1, filled, len(batch))
		}
	}
	return nil
}

// scanShapeExemplars applies one exemplar batch's rows to the shape slice and
// returns how many shapes it filled.
func scanShapeExemplars(rows driver.Rows, idx map[shapeKey]int, shapes []TopicShape) (int, error) {
	defer func() { _ = rows.Close() }()
	filled := 0
	for rows.Next() {
		var (
			contract, topic0, eventType, data string
			topics                            []string
		)
		if err := rows.Scan(&contract, &topic0, &eventType, &topics, &data); err != nil {
			return filled, fmt.Errorf("clickhouse: scan shape exemplar: %w", err)
		}
		i, ok := idx[shapeKey{contract: contract, topic0: topic0}]
		if !ok {
			return filled, fmt.Errorf("clickhouse: shape exemplar for unknown shape (%s, %q)", contract, topic0)
		}
		shapes[i].EventType = eventType
		shapes[i].Topics = topics
		shapes[i].DataXDR = data
		filled++
	}
	return filled, rows.Err()
}
