package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// TopicSample is one representative soroban_events row for a distinct (contract_id, topic_0_sym) shape, with the count and ledger span of events sharing it.
type TopicSample struct {
	Row       domain.SorobanEventRow
	Count     int64
	MinLedger uint32
	MaxLedger uint32
}

// distinctTopicSampleWindow bounds the PHASE-1 scan to recent examples per (contract_id, topic_0_sym); older-only shapes are found by
// [Store.distinctSorobanContractTopicPairs], full-history recognition by computeRecognitionGapsCH (ADR-0033 Claim 2a).
const distinctTopicSampleWindow = 30 * 24 * time.Hour

// DistinctSorobanTopicSamples returns one representative row per distinct (contract_id, topic_0_sym) in soroban_events over [from, to] with per-shape count and ledger span, the input to the recognition audit.
func (s *Store) DistinctSorobanTopicSamples(ctx context.Context, from, to uint32) ([]TopicSample, error) {
	return s.distinctSorobanTopicSamplesAt(ctx, from, to, time.Now())
}

// distinctSorobanTopicSamplesAt is [Store.DistinctSorobanTopicSamples] with the clock threaded in
// so the windowing is testable.
func (s *Store) distinctSorobanTopicSamplesAt(ctx context.Context, from, to uint32, now time.Time) ([]TopicSample, error) {
	if to < from {
		return nil, errors.New("timescale: DistinctSorobanTopicSamples: to < from")
	}

	// Phase 1: the ONLY phase that touches the wide XDR columns; bounded to
	// distinctTopicSampleWindow however far back `from` reaches, chunk-pruned by ledger_close_time.
	windowFrom := now.Add(-distinctTopicSampleWindow)
	samples, err := s.distinctSorobanTopicSamplesWindowed(ctx, from, to, windowFrom)
	if err != nil {
		return nil, err
	}
	seen := make(map[[2]string]bool, len(samples))
	for i := range samples {
		seen[[2]string{samples[i].Row.ContractID, samples[i].Row.Topic0Sym}] = true
	}

	// Phase 2: every (contract_id, topic_0_sym) pair that EVER emitted a Symbol/String topic[0],
	// including contracts quiet before the window. Reads ONLY soroban_events_contract_topic_idx
	// (O(rows), no skip scan, but narrow). Non-Symbol/String shapes are excluded by the partial
	// index (NULL topic_0_sym); `-ch` backstops them.
	pairs, err := s.distinctSorobanContractTopicPairs(ctx)
	if err != nil {
		return nil, fmt.Errorf("timescale: DistinctSorobanTopicSamples pair discovery: %w", err)
	}
	for _, p := range pairs {
		if seen[p] {
			continue
		}
		sample, ok, err := s.oneSorobanTopicSample(ctx, p[0], p[1], from, to)
		if err != nil {
			return nil, fmt.Errorf("timescale: DistinctSorobanTopicSamples fallback sample (%s,%s): %w", p[0], p[1], err)
		}
		if ok {
			samples = append(samples, sample)
		}
	}
	return samples, nil
}

// distinctSorobanTopicSamplesWindowedQuery builds the PHASE 1 text purely so the always-present `ledger_close_time >= $3` bound is
// testable; rangeCovered (from SorobanEventsTimeBound) adds a [$4,$5] bound for chunk pruning.
func distinctSorobanTopicSamplesWindowedQuery(rangeCovered bool) string {
	timeFilter := " AND ledger_close_time >= $3"
	if rangeCovered {
		timeFilter += " AND ledger_close_time BETWEEN $4 AND $5"
	}
	return `
        SELECT DISTINCT ON (contract_id, topic_0_sym)
            ledger, ledger_close_time, tx_hash, op_index, event_index,
            contract_id, contract_id_hex, topic_count, topic_0_sym,
            topic_0_xdr, topic_1_xdr, topic_2_xdr, topic_3_xdr,
            body_xdr, op_args_xdr, topics_xdr,
            count(*)   OVER (PARTITION BY contract_id, topic_0_sym) AS grp_count,
            min(ledger) OVER (PARTITION BY contract_id, topic_0_sym) AS grp_min,
            max(ledger) OVER (PARTITION BY contract_id, topic_0_sym) AS grp_max
        FROM soroban_events
        WHERE ledger BETWEEN $1 AND $2` + timeFilter + `
        ORDER BY contract_id, topic_0_sym, ledger`
}

// distinctSorobanTopicSamplesWindowed is PHASE 1: the DISTINCT ON scan floored at windowFrom, so
// it reads only the trailing distinctTopicSampleWindow of chunks.
func (s *Store) distinctSorobanTopicSamplesWindowed(ctx context.Context, from, to uint32, windowFrom time.Time) ([]TopicSample, error) {
	// Chunk pruning: when ledger_ingest_log fully covers [from,to], add a ledger_close_time bound
	// so only relevant chunks are hit (see SorobanEventsTimeBound). Otherwise the scan is
	// unpruned, still bounded by windowFrom.
	args := []any{int64(from), int64(to), windowFrom}
	rangeCovered := false
	if lo, hi, covered, terr := s.SorobanEventsTimeBound(ctx, from, to); terr == nil && covered {
		args = append(args, lo, hi)
		rangeCovered = true
	}
	q := distinctSorobanTopicSamplesWindowedQuery(rangeCovered)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: DistinctSorobanTopicSamples query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TopicSample
	for rows.Next() {
		var (
			samp                     TopicSample
			r                        domain.SorobanEventRow
			ledger                   int64
			opIdx, eventIdx          int16
			topicCount               int16
			topic0Sym                sql.NullString
			topic1, topic2, topic3   []byte
			opArgs                   []byte
			topicsXDR                [][]byte
			grpCount, grpMin, grpMax int64
		)
		if err := rows.Scan(
			&ledger, &r.LedgerCloseTime, &r.TxHash, &opIdx, &eventIdx,
			&r.ContractID, &r.ContractIDHex, &topicCount, &topic0Sym,
			&r.Topic0XDR, &topic1, &topic2, &topic3,
			&r.BodyXDR, &opArgs, pgarray.Bytea(&topicsXDR),
			&grpCount, &grpMin, &grpMax,
		); err != nil {
			return nil, fmt.Errorf("timescale: DistinctSorobanTopicSamples scan: %w", err)
		}
		r.Ledger = uint32(ledger)
		r.OpIndex = opIdx
		r.EventIndex = eventIdx
		r.TopicCount = topicCount
		if topic0Sym.Valid {
			r.Topic0Sym = topic0Sym.String
		}
		r.Topic1XDR = topic1
		r.Topic2XDR = topic2
		r.Topic3XDR = topic3
		r.OpArgsXDR = opArgs
		r.TopicsXDR = topicsXDR
		samp.Row = r
		samp.Count = grpCount
		samp.MinLedger = uint32(grpMin)
		samp.MaxLedger = uint32(grpMax)
		out = append(out, samp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: DistinctSorobanTopicSamples rows: %w", err)
	}
	return out, nil
}

// distinctSorobanContractTopicPairsQuery is PHASE 2's query text, a const so a regression test can
// assert its shape (topic_0_sym IS NOT NULL, no ledger-range predicate) without a live DB.
const distinctSorobanContractTopicPairsQuery = `
        SELECT DISTINCT contract_id, topic_0_sym
        FROM soroban_events
        WHERE topic_0_sym IS NOT NULL`

// distinctSorobanContractTopicPairs is PHASE 2: every (contract_id, topic_0_sym) pair with a Symbol/String topic[0], read narrowly off
// soroban_events_contract_topic_idx (partial, topic_0_sym IS NOT NULL) rather than the wide XDR/body columns; it touches every row.
func (s *Store) distinctSorobanContractTopicPairs(ctx context.Context) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, distinctSorobanContractTopicPairsQuery)
	if err != nil {
		return nil, fmt.Errorf("timescale: distinctSorobanContractTopicPairs query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out [][2]string
	for rows.Next() {
		var contractID, topic0Sym string
		if err := rows.Scan(&contractID, &topic0Sym); err != nil {
			return nil, fmt.Errorf("timescale: distinctSorobanContractTopicPairs scan: %w", err)
		}
		out = append(out, [2]string{contractID, topic0Sym})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: distinctSorobanContractTopicPairs rows: %w", err)
	}
	return out, nil
}

// oneSorobanTopicSampleQuery is PHASE 3's text: equality on the composite index plus LIMIT 1 with no ORDER BY, so Postgres stops at the
// first entry. No per-pair count/min/max (a dormant pair would exhaust the budget); sampled pairs report Count = -1 ("not measured").
const oneSorobanTopicSampleQuery = `
        SELECT e.ledger, e.ledger_close_time, e.tx_hash, e.op_index, e.event_index,
               e.contract_id, e.contract_id_hex, e.topic_count, e.topic_0_sym,
               e.topic_0_xdr, e.topic_1_xdr, e.topic_2_xdr, e.topic_3_xdr,
               e.body_xdr, e.op_args_xdr, e.topics_xdr
        FROM soroban_events e
        WHERE e.contract_id = $1 AND e.topic_0_sym = $2 AND e.ledger BETWEEN $3 AND $4
        LIMIT 1`

// oneSorobanTopicSampleTimeout bounds each PHASE 3 fetch: a miss is tolerable, a run abort is not.
const oneSorobanTopicSampleTimeout = 90 * time.Second

// oneSorobanTopicSample is PHASE 3: fetch ANY one row for a (contract_id, topic_0_sym) pair in [from, to], only for pairs Phase 1
// missed. ok=false when the pair has no rows in range (Phase 2 is not range-scoped).
func (s *Store) oneSorobanTopicSample(ctx context.Context, contractID, topic0Sym string, from, to uint32) (TopicSample, bool, error) {
	var (
		samp                   TopicSample
		r                      domain.SorobanEventRow
		ledger                 int64
		opIdx, eventIdx        int16
		topicCount             int16
		topic0SymOut           sql.NullString
		topic1, topic2, topic3 []byte
		opArgs                 []byte
		topicsXDR              [][]byte
	)
	pairCtx, cancel := context.WithTimeout(ctx, oneSorobanTopicSampleTimeout)
	defer cancel()
	err := s.db.QueryRowContext(pairCtx, oneSorobanTopicSampleQuery, contractID, topic0Sym, int64(from), int64(to)).Scan(
		&ledger, &r.LedgerCloseTime, &r.TxHash, &opIdx, &eventIdx,
		&r.ContractID, &r.ContractIDHex, &topicCount, &topic0SymOut,
		&r.Topic0XDR, &topic1, &topic2, &topic3,
		&r.BodyXDR, &opArgs, pgarray.Bytea(&topicsXDR),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TopicSample{}, false, nil
	}
	if err != nil {
		// Per-pair deadline expiry (not the caller's ctx): degrade to "unsampled" instead of
		// failing the whole recognition scan.
		if pairTimeoutSkip(pairCtx, ctx, contractID, topic0Sym, from, to) {
			return TopicSample{}, false, nil
		}
		return TopicSample{}, false, fmt.Errorf("timescale: oneSorobanTopicSample query: %w", err)
	}
	r.Ledger = uint32(ledger)
	r.OpIndex = opIdx
	r.EventIndex = eventIdx
	r.TopicCount = topicCount
	if topic0SymOut.Valid {
		r.Topic0Sym = topic0SymOut.String
	}
	r.Topic1XDR = topic1
	r.Topic2XDR = topic2
	r.Topic3XDR = topic3
	r.OpArgsXDR = opArgs
	r.TopicsXDR = topicsXDR
	samp.Row = r
	// Count/span deliberately not measured on the fallback path (see the
	// query const's note): -1 = "dormant pair, not measured".
	samp.Count = -1
	samp.MinLedger = uint32(ledger)
	samp.MaxLedger = uint32(ledger)
	return samp, true, nil
}

// pairTimeoutSkip reports (and logs) whether a PHASE 3 fetch failed on ITS OWN deadline rather than caller cancel or a query error;
// unlogged, dormant shapes (likeliest to time out) would leave no gap and no trace.
func pairTimeoutSkip(pairCtx, ctx context.Context, contractID, topic0Sym string, from, to uint32) bool {
	if pairCtx.Err() == nil || ctx.Err() != nil {
		return false
	}
	slog.Warn("timescale: oneSorobanTopicSample: per-pair timeout, shape unsampled",
		"contract_id", contractID, "topic_0_sym", topic0Sym, "from", from, "to", to,
		"timeout", oneSorobanTopicSampleTimeout)
	return true
}
