package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/StellarIndex/stellar-index/internal/events"
)

// ClassicTokenTopic0Syms are the CAP-67 / SEP-41 token-event topic[0] symbols.
// Under the r1 archive's uniform V4 meta these classic-asset transfer/mint/burn
// events are synthesized for ALL history and utterly dominate contract_events
// (446.91 M of 446.92 M rows in partition 50 — 99.9988 %). No protocol DEX /
// lending decoder (soroswap/aquarius/phoenix/comet/blend/cctp/rozo/defindex)
// consumes them, so a re-derivation pass for those sources can exclude them via
// StreamContractEvents' excludeTopic0 arg, turning a 447 M-row partition scan
// into a ~5 k-row one. (sep41_supply/sep41_transfers DO use these topics, so
// never exclude them when re-deriving those sources.)
var ClassicTokenTopic0Syms = []string{
	"transfer", "mint", "burn", "clawback", "approve", "set_admin", "set_authorized",
}

// FirehoseExcludeSyms is ClassicTokenTopic0Syms MINUS set_admin — the exclusion
// the DEX/lending re-derive paths (projector + ch-rebuild) must use, because
// Blend (and Comet) emit a pool-level "set_admin" event that shares topic[0]
// with the CAP-67 token set_admin. Excluding set_admin wholesale would silently
// drop those protocol admin events (the bug behind blend_admin's missing
// set_admin rows). set_admin's CAP-67 volume is negligible, so retaining it
// costs nothing. Keep in lockstep with internal/projector.firehoseExcludeSyms.
var FirehoseExcludeSyms = []string{
	"transfer", "mint", "burn", "clawback", "approve", "set_authorized",
}

// sqlQuoteList renders a string slice as a SQL IN list: 'a','b',... The inputs
// are compile-time constants (topic symbols), so inlining carries no injection
// risk and avoids driver-specific slice-binding for IN (?).
func sqlQuoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ",")
}

// sqlQuoteEscaped renders a single-quoted SQL string literal with backslash +
// quote escaping. For values that come back FROM the lake (contract ids, topic
// symbols read in a prior query) rather than compile-time constants — they are
// well-formed in practice (strkeys, ScSymbol charset), but the exemplar fetch
// inlines them into query text, so escape rather than assume.
func sqlQuoteEscaped(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// boundedScanSettings is the per-QUERY settings clause for the full-history
// scan class (the recognition shape scan and the completeness reconcile event
// stream). The connection-level class in openRead already caps tracked
// per-query memory, but the in-order read pool's wide-column buffers are
// UNDERTRACKED per query while counted in the SERVER-WIDE total: the
// 2026-07-08 `compute-completeness -ch -source sep41_transfers` runs died at
// ANY server cap ("would use 61G" at a 64G cap, "would use 69G" at 72G) —
// consumption scaled with available memory, so raising caps was not a fix.
// What bounds this class's true footprint is WORK SHAPE, enforced here:
// max_threads=2 (at most two concurrent wide part streams hold read buffers),
// an 8 GiB tracked ceiling, and external group-by/sort spill at 4 GiB so a
// growing lake costs time (disk spill), never an OOM kill. Values are bytes
// (8589934592 = 8 GiB, 4294967296 = 4 GiB).
const boundedScanSettings = "SETTINGS max_threads = 2, max_memory_usage = 8589934592, " +
	"max_bytes_before_external_group_by = 4294967296, max_bytes_before_external_sort = 4294967296"

// forEachLedgerWindow invokes fn over consecutive [lo,hi] windows covering
// [from,to] inclusive. Windows end at multiple-of-stride boundaries minus one,
// so when stride is the lake's partition size (1M) — or divides it evenly —
// each query touches the minimum number of partitions. A no-op when to < from.
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

// StreamContractEvents is the Phase-4 input adapter (ADR-0034): it reads
// stellar.contract_events for [from,to] inclusive, ordered by
// (ledger_seq, tx_hash, op_index, event_index) — the dispatcher's natural
// emission order — and invokes fn for each row reconstructed as an
// events.Event.
//
// The CH columns are a byte-identical serialization of events.Event: topics,
// value, and op-args are all base64(scval.MarshalBinary), exactly as the
// production dispatcher writes them (internal/dispatcher.contractEventToEventsEvent
// at dispatcher.go:881/:907 vs the extractor's eventRow at extract.go:181/:206).
// So the existing protocol decoders consume these events verbatim — no
// re-encoding, no galexie re-touch.
//
// FINAL dedups concurrent/duplicate ReplacingMergeTree parts at read time.
// Callers re-projecting all history should window [from,to] (e.g. per 1M-ledger
// partition) so the streamed result set stays bounded in memory.
//
// Note: ID and TransactionIndex are left zero — the CH lake keys events by
// (ledger, tx_hash, op_index, event_index) and decoders use TxHash, not the
// RPC-shape ID/tx-index. If a future decoder needs tx_index, add it to the
// contract_events schema + extractor first.
// StreamContractEventsFiltered is the projector's forward-read source (ADR-0034
// #10 feed-switch): it streams contract_events for [from,to] narrowed by a
// per-source prefilter (contract_id IN / topic_0_sym IN — mirrors the Postgres
// soroban_events path's prefilter), reconstructing each as an events.Event for
// the source's decoder. NO FINAL: the projector reads small forward windows and
// its downstream writes are idempotent (ON CONFLICT DO NOTHING), so a duplicate
// event decodes to the same row and is absorbed — FINAL's full-partition merge
// would be pure overhead here. Empty filters → match-by-Decoder.Matches alone
// (coarser, but the window is BatchLimit-bounded).
//
// excludeTopic0Syms (nil = no exclusion) drops events whose topic[0] symbol is
// in the list — used so the no-contract-prefilter DEX/lending sources skip the
// CAP-67 classic-token firehose at the SQL layer instead of streaming it all
// and discarding it via Decoder.Matches (see ClassicTokenTopic0Syms; matters
// for a far-behind source's wide catch-up window).
//
// useFinal toggles FINAL. The live projector passes false: it reads small
// forward windows and its downstream writes are idempotent (ON CONFLICT DO
// NOTHING), so a duplicate ReplacingMergeTree part decodes to the same row and
// is absorbed — FINAL would be pure overhead. A COUNTING consumer (the
// completeness reconcile) MUST pass true: without FINAL, un-merged duplicate
// parts (e.g. the footprint-sample / validation re-run partitions 25/45/62)
// are double-counted, producing false projection mismatches.
//
// withOpArgs selects whether the read includes op_args_xdr. Only decoders that
// consume events.Event.OpArgs need it (redstone zips write_prices feed_ids
// from the op args — PR 166); every other decoder decodes from topics + data.
// op_args_xdr is a WIDE column (the whole InvokeContract arg vector per row),
// and reading it across the CAP-67 classic-token firehose was one of the two
// legs of the 2026-07-08 sep41 completeness OOMs — pass false unless the
// consuming decoder actually reads OpArgs.
func StreamContractEventsFiltered(ctx context.Context, addr string, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string, useFinal, withOpArgs bool, fn func(events.Event) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	q := contractEventsFilteredQuery(contractIDs, topic0Syms, excludeTopic0Syms, useFinal, withOpArgs)
	rows, err := conn.Query(ctx, q, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query contract_events filtered [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return scanContractEvents(rows, withOpArgs, fn)
}

// contractEventsFilteredQuery builds the StreamContractEventsFiltered query
// text. Split out so the query shape (column trim, FINAL, prefilters, the
// bounded-scan SETTINGS) is unit-testable without a ClickHouse server.
func contractEventsFilteredQuery(contractIDs, topic0Syms, excludeTopic0Syms []string, useFinal, withOpArgs bool) string {
	where := "WHERE ledger_seq BETWEEN ? AND ?"
	if len(contractIDs) > 0 {
		where += " AND contract_id IN (" + sqlQuoteList(contractIDs) + ")"
	}
	if len(topic0Syms) > 0 {
		where += " AND topic_0_sym IN (" + sqlQuoteList(topic0Syms) + ")"
	}
	if len(excludeTopic0Syms) > 0 {
		where += " AND topic_0_sym NOT IN (" + sqlQuoteList(excludeTopic0Syms) + ")"
	}
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

// excludeTopic0 (nil = no filter) drops events whose topic[0] symbol is in the
// list — used to skip the CAP-67 classic-token firehose when re-deriving
// protocol sources that don't consume it (see ClassicTokenTopic0Syms).
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
	return scanContractEvents(rows, true, fn)
}

// scanContractEvents maps contract_events result rows to events.Event and
// invokes fn for each. Shared by StreamContractEvents (FINAL, exclude-filter,
// always reads op_args_xdr) and StreamContractEventsFiltered (per-source
// prefilter; withOpArgs must match the query's column list — false leaves
// events.Event.OpArgs nil).
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
		dest := []any{&ledger, &closeTime, &txHash, &opIndex, &eventIndex,
			&contractID, &eventType, &topics, &dataXDR}
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
