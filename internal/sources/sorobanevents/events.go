// Package sorobanevents is the catch-all raw-event landing zone
// for Soroban contract events (ADR-0029).
//
// Every Soroban contract event the dispatcher routes is also
// captured here as a raw row in the `soroban_events` hypertable
// (migration 0041). This is ORTHOGONAL to the existing per-source
// decoders — soroswap / phoenix / aquarius / blend / cctp / rozo /
// reflector / redstone / band / sep41_supply / etc. all continue
// to write their domain-specific tables from the live event
// stream. The soroban_events table exists so that future per-source
// decoders that ship AFTER an event was emitted on-chain can
// backfill via SQL queries rather than MinIO re-walks:
//
//	INSERT INTO blend_positions / cctp_events / whatever
//	  SELECT ...
//	    FROM soroban_events
//	   WHERE contract_id IN (...) AND topic_0_sym IN (...)
//
// — milliseconds-to-minutes instead of hours-per-source.
//
// # Wiring
//
//   - [RawEventSink] is the interface the dispatcher's
//     [dispatcher.SetRawEventSink] hook accepts (added in ADR-0029).
//     The hook fires AFTER per-source decoders for every Soroban
//     contract event (does NOT filter on topic[0] or contract_id —
//     this is the catch-all).
//   - [Capture] converts a [events.Event] into a [Row] suitable for
//     batched insert.
//   - The consumer (cmd/stellarindex-indexer / stellarindex-ops
//     backfill) wires an [AsyncSink] that batches Rows and calls
//     [timescale.Store.InsertSorobanEventsBatch].
//
// # Encoding
//
// Topics 0-3 are stored as their raw XDR bytes (base64-decoded from
// the wire). topic_0_sym is a convenience column populated when
// topic[0]'s XDR decodes to a Symbol or String. The event body is
// the raw XDR body (un-base64'd). op_args_xdr is the
// XDR-marshalled `xdr.ScVec` of the originating InvokeContract
// op's args, NULL when the event didn't come from an
// InvokeContract op (system events, CAP-67 classic-op events,
// etc.).
//
// # Contract ID encoding
//
// `contract_id` is the C-strkey (`C...`) form for human SQL;
// `contract_id_hex` is the raw 32 bytes (for index-efficient
// byte-equality joins). Both come from the same underlying
// strkey decode.
package sorobanevents

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this catch-all source. Used
// only by metrics / log attribution — soroban-events does not
// register with the dispatcher's per-source `Decoder` chain (it
// hooks via [dispatcher.RawEventSink] instead, which sees every
// event regardless of per-source matching).
const SourceName = "soroban-events"

// Row is one captured soroban_events row, ready for batched insert.
// Fields map 1:1 to the columns in migration 0041; *string / *[]byte
// represent nullable columns.
//
// Canonical definition lives in [domain.SorobanEventRow] (D8 M0-1:
// internal/storage/timescale reads/writes this shape and must not
// import upward into this package to do so); this is a transparent
// alias so every existing caller of sorobanevents.Row is unaffected.
type Row = domain.SorobanEventRow

// ErrSkip is returned by [Capture] for events the soroban_events
// table never stores (non-"contract" types: system / diagnostic, or
// events without a valid ContractID). Callers MUST treat this as a
// silent skip (not a counted error) — the only way it fires is on
// inputs the dispatcher already filtered out, so it represents
// defence-in-depth.
var ErrSkip = errors.New("sorobanevents: event not eligible for capture")

// Capture projects one [events.Event] into a [Row] ready for
// insert. Returns [ErrSkip] for inputs that should never be
// captured (non-contract events, malformed ContractID).
//
// Decoding rules:
//
//   - tx_hash is the 32-byte raw hash (the hex-string on
//     events.Event is decoded here so the indexed column stays
//     binary-equality-comparable).
//   - contract_id is preserved as the wire C-strkey; contract_id_hex
//     is the strkey-decoded 32 bytes.
//   - topic_0_sym is populated when topic[0] decodes to a Symbol or
//     String; empty otherwise.
//   - topic_N_xdr / body_xdr are the raw XDR bytes (base64-decoded
//     from the events.Event wire shape).
//   - op_args_xdr is the XDR-marshalled ScVec of the args; nil when
//     ev.OpArgs is empty (the event didn't come from an
//     InvokeContract op or the dispatcher had no args to surface).
func Capture(ev events.Event) (Row, error) {
	if ev.Type != "contract" {
		return Row{}, ErrSkip
	}
	if ev.ContractID == "" {
		return Row{}, ErrSkip
	}
	if len(ev.Topic) == 0 {
		// Defensive: every Soroban contract event has at least
		// topic[0]. A zero-topic event is malformed; skip rather
		// than insert a row with NULL topic_0_xdr (which would
		// violate the migration's NOT NULL constraint and abort
		// the whole batch on COMMIT).
		return Row{}, ErrSkip
	}

	contractIDRaw, err := strkey.Decode(strkey.VersionByteContract, ev.ContractID)
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: contract_id %q: %w", ev.ContractID, err)
	}

	txHashRaw, err := decodeTxHashHex(ev.TxHash)
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: tx_hash %q: %w", ev.TxHash, err)
	}

	observedAt, err := ev.EventClosedAt()
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: %w", err)
	}

	topicXDRs, err := decodeTopics(ev.Topic)
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: topics: %w", err)
	}
	topic0Sym := tryDecodeSymbolOrString(ev.Topic[0])

	bodyXDR, err := base64.StdEncoding.DecodeString(ev.Value)
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: body: %w", err)
	}

	opArgsXDR, err := encodeOpArgsAsScVec(ev.OpArgs)
	if err != nil {
		return Row{}, fmt.Errorf("sorobanevents: op_args: %w", err)
	}

	return Row{
		Ledger:          ev.Ledger,
		LedgerCloseTime: observedAt.UTC(),
		TxHash:          txHashRaw,
		OpIndex:         int16(ev.OperationIndex),
		// event_index is the event's position within its operation's
		// contract-event list, threaded from the dispatcher
		// (ADR-0033). It is the final component of the soroban_events
		// PK; without it an op emitting ≥2 events (Phoenix: 8 per
		// swap) collides on (ledger, tx_hash, op_index, 0) and the
		// writer's ON CONFLICT DO NOTHING silently drops all but the
		// first. RPC-sourced events leave it 0, but only the
		// dispatcher populates soroban_events in production.
		EventIndex:    int16(ev.EventIndex),
		ContractID:    ev.ContractID,
		ContractIDHex: contractIDRaw,
		TopicCount:    int16(len(ev.Topic)),
		Topic0Sym:     topic0Sym,
		// topicXDRs[0] is safe: the len(ev.Topic)==0 guard above
		// returned ErrSkip, so decodeTopics produced >=1 element.
		Topic0XDR: topicXDRs[0],
		Topic1XDR: topicAt(topicXDRs, 1),
		Topic2XDR: topicAt(topicXDRs, 2),
		Topic3XDR: topicAt(topicXDRs, 3),
		TopicsXDR: topicXDRs,
		BodyXDR:   bodyXDR,
		OpArgsXDR: opArgsXDR,
	}, nil
}

// decodeTopics base64-decodes EVERY topic slot from the wire into an
// ordered slice — no truncation (audit-2026-07-16 C2-11). The first
// four also populate the fixed topic_0..3 columns (back-compat + the
// topic_0_sym index fast-path); the full slice is persisted in the
// topics_xdr array column (migration 0114) so events with 5+ topics —
// e.g. Aquarius multi-token pool events — keep every topic rather than
// silently dropping topics 5+. (Soroban's contractevent macro caps at
// 4, but custom-emitted events exceed that.) topic_count still records
// the full arity.
func decodeTopics(topics []string) ([][]byte, error) {
	out := make([][]byte, len(topics))
	for i := range topics {
		raw, err := base64.StdEncoding.DecodeString(topics[i])
		if err != nil {
			return nil, fmt.Errorf("topic[%d]: %w", i, err)
		}
		out[i] = raw
	}
	return out, nil
}

// topicAt returns the i-th decoded topic or nil when the event had
// fewer than i+1 topics — used to fill the fixed topic_0..3 columns
// from the variable-length full list.
func topicAt(xs [][]byte, i int) []byte {
	if i < len(xs) {
		return xs[i]
	}
	return nil
}

// tryDecodeSymbolOrString returns the decoded Symbol/String value
// of `b64Topic` when the underlying SCVal is one of those types;
// returns "" otherwise (the catch-all sink writes SQL NULL).
//
// This is purely a convenience for index fast-paths — downstream
// correctness reads topic_0_xdr.
func tryDecodeSymbolOrString(b64Topic string) string {
	sv, err := scval.Parse(b64Topic)
	if err != nil {
		return ""
	}
	if s, err := scval.AsSymbol(sv); err == nil {
		return s
	}
	if s, err := scval.AsString(sv); err == nil {
		return s
	}
	return ""
}

// encodeOpArgsAsScVec marshals the dispatcher-provided op args
// (each already base64-encoded XDR of one ScVal) into a single
// XDR-marshalled ScVec. Returns nil when args is empty so the
// caller writes SQL NULL rather than an empty-vec blob.
//
// Delegates to [scval.EncodeArgsAsScVec] so the xdr-dependency
// stays inside internal/scval (lint-imports B/xdr-scoped-to-scval).
func encodeOpArgsAsScVec(args []string) ([]byte, error) {
	return scval.EncodeArgsAsScVec(args)
}

// decodeTxHashHex parses the wire tx_hash (lowercase hex, 64 chars)
// into 32 raw bytes. The dispatcher stamps hex via
// `hex.EncodeToString(tx.Result.TransactionHash[:])` so reversing
// it here is exact.
func decodeTxHashHex(h string) ([]byte, error) {
	if len(h) == 0 {
		return nil, errors.New("empty tx_hash")
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("tx_hash decoded to %d bytes, want 32", len(raw))
	}
	return raw, nil
}
