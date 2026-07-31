// Package events holds transport-neutral Soroban contract-event
// types — identical whether the event was observed via stellar-rpc
// getEvents (development / fixture capture) or extracted from
// ledger-meta via internal/dispatcher (production).
//
// Per docs/architecture/ingest-pipeline.md, decoder packages read
// events.Event and nothing else. They don't import internal/stellarrpc
// (which would pull in the JSON-RPC client surface) and they don't
// import xdr (which would bypass the internal/scval wrapper). The
// one shape, one boundary.
//
// Historically this type lived in internal/stellarrpc. It now lives
// here and stellarrpc depends on it (its getEvents response embeds
// []events.Event) — the old re-export alias and the RPC-based
// consumer loops it served are gone; stellarrpc is a diagnostics /
// fixture-capture client only, not a production ingest path.
package events

import (
	"fmt"
	"time"
)

// Event is a single Soroban contract event. JSON tags are preserved
// from the stellar-rpc getEvents response shape — the JSON-RPC
// client still populates this type, and RPC fixture replays in the
// dev scripts hit the same struct.
type Event struct {
	Type                     string `json:"type"` // contract | system | diagnostic
	Ledger                   uint32 `json:"ledger"`
	LedgerClosedAt           string `json:"ledgerClosedAt"` // RFC 3339
	ContractID               string `json:"contractId"`
	ID                       string `json:"id"`
	OperationIndex           int    `json:"operationIndex"`
	TransactionIndex         int    `json:"transactionIndex"`
	TxHash                   string `json:"txHash"`
	InSuccessfulContractCall bool   `json:"inSuccessfulContractCall"`

	// EventIndex is the position of this event within its operation's
	// contract-event list — the slice index the dispatcher walked when
	// flattening tx meta (internal/dispatcher/dispatcher.go). Combined
	// with (Ledger, TxHash, OperationIndex) it uniquely identifies the
	// event, which is exactly the soroban_events PK
	// (ledger_close_time, ledger, tx_hash, op_index, event_index).
	//
	// CRITICAL: without this, an operation that emits ≥2 contract
	// events (Phoenix emits 8 per swap) collapses to one row in
	// soroban_events under ON CONFLICT DO NOTHING — the raw landing
	// zone silently loses 7 of 8. See ADR-0033.
	//
	// Populated only by the production dispatcher path from the LCM.
	// NOT part of the stellar-rpc getEvents wire shape (RPC encodes
	// position in the opaque `ID` string instead), so it's never
	// marshalled — `json:"-"` keeps RPC fixture replays byte-identical.
	EventIndex int `json:"-"`

	// Topic entries are base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Topic []string `json:"topic"`
	// Value is base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Value string `json:"value"`

	// OpArgs carries the base64-encoded SCVal arguments of the
	// InvokeHostFunction operation that produced this event, when
	// that op invoked a contract via HostFunctionTypeInvokeContract.
	// Populated by internal/dispatcher from
	// tx.Envelope.Operations()[OperationIndex].Body.InvokeHostFunctionOp
	// .HostFunction.InvokeContract.Args.
	//
	// Empty for events not produced by an InvokeContract call, for
	// events observed via stellar-rpc getEvents (which doesn't
	// surface the invoking op's args), and for CAP-67 transfer
	// events tied to classic ops.
	//
	// Redstone is the current primary user: the WritePrices event's
	// `updated_feeds` vec carries price+timestamps but no feed_id;
	// the feed_ids live in the op args and need to be zipped in at
	// decode time. See docs/discovery/oracles/redstone.md.
	//
	// NOT serialized in the stellar-rpc JSON shape — `omitempty` so
	// fixture replays from RPC round-trip unchanged.
	OpArgs []string `json:"opArgs,omitempty"`

	// StateWriteKeys carries the base64-encoded XDR LedgerKeys of the
	// contract-data entries whose VALUE was CHANGED by the same
	// operation that emitted this event (created, or updated with
	// post-image Val != the op's pre-image Val — an identical-value
	// rewrite does not count), filtered to entries owned by THIS
	// event's contract. Populated by internal/dispatcher from the tx
	// meta's per-operation LedgerEntryChanges (production LCM path) and
	// by the ClickHouse event reader from the lake's
	// ledger_entry_changes table (re-derive path); both apply the same
	// rule (internal/dispatcher/state_write_keys.go) so decoders see
	// one shape.
	//
	// Redstone is the current primary user: write_prices REWRITES every
	// requested feed's entry but only ACCEPTED feeds' stored PriceData
	// actually changes (r1 ground truth, ledger 62056824), so when the
	// freshness verifier drops feeds (updated_feeds SHORTER than the
	// op-args feed_ids) the value-changed keys name the accepted subset
	// EXACTLY — no median heuristics. See
	// internal/sources/redstone/decode.go resolveFeedAttribution.
	//
	// Empty for events observed via stellar-rpc getEvents (which does
	// not surface entry changes) and for readers that did not opt in
	// (the fetch is per-source opt-in, like OpArgs). Decoders MUST
	// treat absence as "unknown", not "no writes".
	//
	// NOT serialized in the stellar-rpc JSON shape — `omitempty` so
	// fixture replays from RPC round-trip unchanged.
	StateWriteKeys []string `json:"stateWriteKeys,omitempty"`
}

// EventClosedAt parses the RFC 3339 LedgerClosedAt string into a
// time.Time. Centralised so every source package doesn't redo the
// parse+error-handling dance.
//
// An empty LedgerClosedAt returns an error rather than the zero
// time.Time: a zero-valued timestamp flowing through to the trades
// hypertable breaks VWAP windows and time-ordered queries, so
// surfacing the parse failure at the decoder boundary is the fail-
// closed choice.
func (e *Event) EventClosedAt() (time.Time, error) {
	if e.LedgerClosedAt == "" {
		return time.Time{}, fmt.Errorf("events: event %s has empty ledgerClosedAt", e.ID)
	}
	t, err := time.Parse(time.RFC3339, e.LedgerClosedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: event %s ledgerClosedAt %q: %w", e.ID, e.LedgerClosedAt, err)
	}
	return t, nil
}
