// Package sorobanevents is the catch-all raw-event landing zone for Soroban contract events
// (ADR-0029). Every contract event the dispatcher routes is also captured as a raw row in the
// `soroban_events` hypertable, so decoders shipped AFTER an event was emitted can backfill via SQL
// instead of MinIO re-walks.
//
// [RawEventSink] fires AFTER the per-source decoders for every contract event, with no filter.
// [Capture] converts a [events.Event] into a [Row]; an [AsyncSink] batches Rows into
// [timescale.Store.InsertSorobanEventsBatch].
//
// Topics and body are raw XDR; topic_0_sym is set when topic[0] is a Symbol or String.
// `contract_id` is the C-strkey for human SQL; `contract_id_hex` the raw 32 bytes for
// index-efficient byte-equality joins.
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

// SourceName is the registry key for this catch-all source, used only for metrics / log attribution; it hooks in via [dispatcher.RawEventSink], not the per-source Decoder chain.
const SourceName = "soroban-events"

// Row is one captured soroban_events row for batched insert, fields mapping 1:1 to table columns (pointers are nullable); it aliases [domain.SorobanEventRow] because internal/storage/timescale must not import upward into this package.
type Row = domain.SorobanEventRow

// ErrSkip is returned by [Capture] for events the table never stores (non-"contract" types, or no valid ContractID); callers MUST treat it as a silent skip.
var ErrSkip = errors.New("sorobanevents: event not eligible for capture")

// Capture projects one [events.Event] into a [Row], returning [ErrSkip] for non-contract events or a missing ContractID; tx_hash is stored as 32 raw bytes, topics and body as base64-decoded raw XDR, and op_args_xdr is nil when the event did not come from an InvokeContract op.
func Capture(ev events.Event) (Row, error) {
	if ev.Type != "contract" {
		return Row{}, ErrSkip
	}
	if ev.ContractID == "" {
		return Row{}, ErrSkip
	}
	if len(ev.Topic) == 0 {
		// A zero-topic event is malformed; a NULL topic_0_xdr would violate the NOT NULL
		// constraint and abort the whole batch on COMMIT.
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
		// event_index is the event's position within its operation, and the final PK component:
		// without it an op emitting 2+ events (Phoenix: 8 per swap) collides on
		// (ledger, tx_hash, op_index, 0) and ON CONFLICT DO NOTHING silently drops all but the first.
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

// decodeTopics base64-decodes EVERY topic into an ordered slice, never truncating. The first four also fill topic_0..3; the full
// slice goes to topics_xdr so events with 5+ topics keep every topic. topic_count records the full arity.
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

// tryDecodeSymbolOrString returns the Symbol/String value of b64Topic, or "" (SQL NULL). An index convenience only; correctness reads
// topic_0_xdr. A String goes through AsText because a contract-supplied String may carry NUL or invalid UTF-8 (SQLSTATE 22021).
func tryDecodeSymbolOrString(b64Topic string) string {
	sv, err := scval.Parse(b64Topic)
	if err != nil {
		return ""
	}
	if s, err := scval.AsSymbol(sv); err == nil {
		return s
	}
	if s, err := scval.AsText(sv); err == nil {
		return s
	}
	return ""
}

// encodeOpArgsAsScVec marshals base64 ScVal op args into one XDR ScVec, nil when empty so the
// caller writes SQL NULL. It delegates to scval to keep xdr scoped there
// (lint-imports B/xdr-scoped-to-scval).
func encodeOpArgsAsScVec(args []string) ([]byte, error) {
	return scval.EncodeArgsAsScVec(args)
}

// decodeTxHashHex parses the wire tx_hash (lowercase hex, 64 chars) into 32 raw bytes; the dispatcher stamps it with hex.EncodeToString.
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
