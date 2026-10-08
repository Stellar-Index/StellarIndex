package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ClassicTokenTopic0Syms are the CAP-67 / SEP-41 token-event topic[0] symbols. They
// dominate contract_events (>99.99% of a partition) and no DEX/lending decoder reads
// them, so those re-derivations exclude them. sep41_supply and sep41_transfers DO
// use them: never exclude when re-deriving those.
var ClassicTokenTopic0Syms = []string{
	"transfer", "mint", "burn", "clawback", "approve", "set_admin", "set_authorized",
}

// FirehoseExcludeSyms is ClassicTokenTopic0Syms minus set_admin: Blend and Comet emit
// a pool-level set_admin sharing topic[0] with the token one, so excluding it would
// drop protocol admin events. Keep in lockstep with internal/projector.firehoseExcludeSyms.
var FirehoseExcludeSyms = []string{
	"transfer", "mint", "burn", "clawback", "approve", "set_authorized",
}

// sqlQuoteList renders a SQL IN list from compile-time constants (topic symbols), so
// inlining carries no injection risk.
func sqlQuoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ",")
}

// sqlQuoteEscaped renders an escaped string literal for values read back from the lake
// (contract ids, topic symbols) that are inlined into query text.
func sqlQuoteEscaped(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// sqlQuoteEscapedList is the list form of sqlQuoteEscaped.
func sqlQuoteEscapedList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = sqlQuoteEscaped(s)
	}
	return strings.Join(q, ",")
}

// boundedScanSettings is the per-query clause for full-history scans. The connection
// cap does not bound them: the in-order read pool's wide-column buffers are
// undertracked per query yet counted server-wide, so raising caps did not help.
// Work shape bounds the footprint: 2 threads, 8 GiB tracked, external group-by/sort
// spill at 4 GiB (bytes), so a growing lake costs time, never an OOM kill.
const boundedScanSettings = "SETTINGS max_threads = 2, max_memory_usage = 8589934592, " +
	"max_bytes_before_external_group_by = 4294967296, max_bytes_before_external_sort = 4294967296"

// forEachLedgerWindow invokes fn over consecutive [lo,hi] windows covering [from,to].
// Windows end at stride boundaries minus one, so a stride dividing the 1M partition
// size touches the fewest partitions. No-op when to < from.
func forEachLedgerWindow(from, to, stride uint32, fn func(lo, hi uint32) error) error {
	if to < from || stride == 0 {
		return nil
	}
	for lo := from; ; {
		hi := to
		if aligned := (uint64(lo)/uint64(stride)+1)*uint64(stride) - 1; aligned < uint64(to) {
			hi = uint32(aligned)
		}
		if err := fn(lo, hi); err != nil {
			return err
		}
		if hi >= to {
			return nil
		}
		lo = hi + 1
	}
}

// StreamContractEventsFiltered is the projector's forward-read source (ADR-0041):
// contract_events for [from,to] narrowed by contract_id IN / topic_0_sym IN, rebuilt
// as events.Event in apply order (SQL sorts by tx_hash; scanInApplyOrder re-sorts each
// ledger by tx_index). ID and TransactionIndex stay zero. Empty filters leave
// Decoder.Matches as the only gate.
//
// excludeTopic0Syms (nil = none) drops the CAP-67 firehose in SQL (ClassicTokenTopic0Syms).
//
// useFinal=false (projector): windows are small and writes idempotent, so duplicate
// ReplacingMergeTree parts are absorbed. A COUNTING consumer must not double-count
// them: pass true (ch-rebuild sep41 dry-run), or dedup adjacent rows in Go (ORDER BY
// makes duplicates consecutive; the completeness reconcile does this, gentler on the
// shared host). FINAL + contract_id filter + wide range is the protocol_reader.go
// trap; the sole useFinal=true caller is deliberate (openRead has no execution-time
// cap and it streams), but a new request-path caller needs its own row ceiling.
//
// withOpArgs reads op_args_xdr, a wide column: only decoders reading
// events.Event.OpArgs (redstone) need it. withStateWriteKeys resolves StateWriteKeys
// from ledger_entry_changes (state_write_keys.go); per-source opt-in, only redstone
// reads them.
func StreamContractEventsFiltered(ctx context.Context, addr string, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string, useFinal, withOpArgs, withStateWriteKeys bool, fn func(events.Event) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	emit := fn
	var enricher *stateWriteKeyEnricher
	if withStateWriteKeys {
		// openRead's MaxOpenConns=2 leaves one connection for the enricher's batch lookups
		// while the stream holds the other.
		enricher = newStateWriteKeyEnricher(ctx, conn, fn)
		emit = enricher.add
	}

	qctx, err := withContractIDsTable(ctx, contractIDs)
	if err != nil {
		return err
	}
	q := contractEventsFilteredQuery(contractIDs, topic0Syms, excludeTopic0Syms, useFinal, withOpArgs)
	rows, err := conn.Query(qctx, q, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query contract_events filtered [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	if err := scanInApplyOrder(ctx, conn, rows, withOpArgs, emit); err != nil {
		return err
	}
	if enricher != nil {
		return enricher.flush()
	}
	return nil
}

// topic0Predicate renders the topic[0] prefilter and matches BOTH encodings:
// extract.go fills topic_0_sym from GetSym() (Symbol only), so it is empty for an
// ScvString topic[0] (phoenix) and filtering on it alone silently matches nothing.
// Also test topics_xdr[1] against the ScvString encoding; widening the column would
// mean re-extracting the lake. It only widens a prefilter, and each decoder's
// Matches() remains the final gate.
func topic0Predicate(topic0Syms []string) string {
	asStrings := make([]string, 0, len(topic0Syms))
	for _, s := range topic0Syms {
		b64, err := scval.EncodeString(s)
		if err != nil {
			// Not encodable as an ScvString: no lake row carries that form, so the Symbol arm is exhaustive.
			continue
		}
		asStrings = append(asStrings, b64)
	}
	pred := "topic_0_sym IN (" + sqlQuoteList(topic0Syms) + ")"
	if len(asStrings) > 0 {
		pred += " OR topics_xdr[1] IN (" + sqlQuoteList(asStrings) + ")"
	}
	return "(" + pred + ")"
}

// contractEventsFilteredQuery builds the query text, split out so its shape is
// unit-testable without a ClickHouse server.
func contractEventsFilteredQuery(contractIDs, topic0Syms, excludeTopic0Syms []string, useFinal, withOpArgs bool) string {
	where := contractEventsFilterWhere(contractIDs, topic0Syms, excludeTopic0Syms)
	final := ""
	if useFinal {
		final = "FINAL"
	}
	opArgsCol := ""
	if withOpArgs {
		opArgsCol = " op_args_xdr,"
	}
	return fmt.Sprintf(`
		SELECT ledger_seq, close_time, tx_hash, op_index, event_index,
		       contract_id, event_type, topics_xdr, data_xdr,%s
		       in_successful_call
		FROM stellar.contract_events %s
		%s
		ORDER BY ledger_seq, tx_hash, op_index, event_index
		%s`, opArgsCol, final, where, boundedScanSettings)
}

// FirstContractEventLedgerFiltered returns the lowest ledger in [from, to]
// holding a row StreamContractEventsFiltered would stream for the same
// filters, and false when the range holds none. The projector seeds a
// never-run source from it.
func FirstContractEventLedgerFiltered(ctx context.Context, addr string, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string) (uint32, bool, error) {
	if to < from {
		return 0, false, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	qctx, err := withContractIDsTable(ctx, contractIDs)
	if err != nil {
		return 0, false, err
	}
	var ledger uint32
	err = conn.QueryRow(qctx, firstContractEventLedgerQuery(contractIDs, topic0Syms, excludeTopic0Syms), from, to).Scan(&ledger)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("clickhouse: first contract_events ledger [%d,%d]: %w", from, to, err)
	}
	return ledger, true, nil
}

// firstContractEventLedgerQuery orders by the sort-key prefix with LIMIT 1 so
// CH reads in order and stops at the first match instead of scanning [from, to].
func firstContractEventLedgerQuery(contractIDs, topic0Syms, excludeTopic0Syms []string) string {
	return fmt.Sprintf(`
		SELECT ledger_seq FROM stellar.contract_events
		%s
		ORDER BY ledger_seq
		LIMIT 1
		%s`, contractEventsFilterWhere(contractIDs, topic0Syms, excludeTopic0Syms), boundedScanSettings)
}

// maxInlineContractIDs caps how many contract ids are inlined as SQL
// literals (~60 bytes each). A factory's gated set can hold 100k+ children,
// which as literals overflows ClickHouse's 256 KiB max_query_size, so a
// larger set is sent as the external table contractIDsTable instead.
const maxInlineContractIDs = 1000

const contractIDsTable = "gated_contract_ids"

// withContractIDsTable attaches contractIDs as the contractIDsTable external
// table when contractEventsFilterWhere will reference it.
func withContractIDsTable(ctx context.Context, contractIDs []string) (context.Context, error) {
	if len(contractIDs) <= maxInlineContractIDs {
		return ctx, nil
	}
	tbl, err := ext.NewTable(contractIDsTable, ext.Column("contract_id", "String"))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: build %s external table: %w", contractIDsTable, err)
	}
	for _, id := range contractIDs {
		if err := tbl.Append(id); err != nil {
			return nil, fmt.Errorf("clickhouse: append %s row: %w", contractIDsTable, err)
		}
	}
	return clickhouse.Context(ctx, clickhouse.WithExternalTable(tbl)), nil
}

// contractEventsFilterWhere renders the `WHERE ledger_seq BETWEEN ? AND ?`
// clause plus the contract / topic[0] prefilters, shared by the stream and
// the first-ledger seek so they cannot disagree about which rows match.
func contractEventsFilterWhere(contractIDs, topic0Syms, excludeTopic0Syms []string) string {
	where := "WHERE ledger_seq BETWEEN ? AND ?"
	if len(contractIDs) > maxInlineContractIDs {
		where += " AND contract_id IN " + contractIDsTable
	} else if len(contractIDs) > 0 {
		// contractIDs is caller-supplied (a live-grown gated set), so escape it.
		where += " AND contract_id IN (" + sqlQuoteEscapedList(contractIDs) + ")"
	}
	if len(topic0Syms) > 0 {
		where += " AND " + topic0Predicate(topic0Syms)
	}
	if len(excludeTopic0Syms) > 0 {
		where += " AND topic_0_sym NOT IN (" + sqlQuoteList(excludeTopic0Syms) + ")"
	}
	return where
}

// StreamContractEvents reads contract_events FINAL for [from,to] in apply order,
// always with op_args_xdr. excludeTopic0 (nil = none) skips the CAP-67 firehose.
func StreamContractEvents(ctx context.Context, addr string, from, to uint32, excludeTopic0 []string, fn func(events.Event) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	where := "WHERE ledger_seq BETWEEN ? AND ?"
	if len(excludeTopic0) > 0 {
		where += " AND topic_0_sym NOT IN (" + sqlQuoteList(excludeTopic0) + ")"
	}
	rows, err := conn.Query(ctx, fmt.Sprintf(`
		SELECT ledger_seq, close_time, tx_hash, op_index, event_index,
		       contract_id, event_type, topics_xdr, data_xdr, op_args_xdr,
		       in_successful_call
		FROM stellar.contract_events FINAL
		%s
		ORDER BY ledger_seq, tx_hash, op_index, event_index`, where), from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query contract_events [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return scanInApplyOrder(ctx, conn, rows, true, fn)
}

// scanInApplyOrder scans rows through an [applyOrderer], so fn sees each
// ledger's events in transaction apply order rather than the table's tx_hash
// sort. The lookups use the pool's second connection while rows holds the first.
func scanInApplyOrder(ctx context.Context, conn driver.Conn, rows driver.Rows, withOpArgs bool, fn func(events.Event) error) error {
	o := newApplyOrderer(ctx, connTxIndexLookup(conn), fn)
	if err := scanContractEvents(rows, withOpArgs, o.add); err != nil {
		return err
	}
	return o.flush()
}

// scanContractEvents maps result rows to events.Event for fn. withOpArgs must match
// the query's column list; false leaves events.Event.OpArgs nil.
func scanContractEvents(rows driver.Rows, withOpArgs bool, fn func(events.Event) error) error {
	for rows.Next() {
		var (
			ledger     uint32
			closeTime  time.Time
			txHash     string
			opIndex    uint32
			eventIndex uint32
			contractID string
			eventType  string
			topics     []string
			dataXDR    string
			opArgs     []string
			inSucc     uint8
		)
		dest := []any{
			&ledger, &closeTime, &txHash, &opIndex, &eventIndex,
			&contractID, &eventType, &topics, &dataXDR,
		}
		if withOpArgs {
			dest = append(dest, &opArgs)
		}
		dest = append(dest, &inSucc)
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("clickhouse: scan contract_event: %w", err)
		}
		if err := fn(events.Event{
			Type:                     eventType,
			Ledger:                   ledger,
			LedgerClosedAt:           closeTime.UTC().Format(time.RFC3339),
			ContractID:               contractID,
			OperationIndex:           int(opIndex),
			EventIndex:               int(eventIndex),
			TxHash:                   txHash,
			InSuccessfulContractCall: inSucc != 0,
			Topic:                    topics,
			Value:                    dataXDR,
			OpArgs:                   opArgs,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}
