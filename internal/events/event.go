// Package events holds transport-neutral Soroban contract-event types, identical whether the event
// came from stellar-rpc getEvents (fixture capture) or from ledger meta via internal/dispatcher
// (production).
//
// Decoder packages read events.Event and nothing else: importing internal/stellarrpc would pull in
// the JSON-RPC client surface, and importing xdr would bypass the internal/scval wrapper. The
// dependency points one way: stellarrpc embeds []events.Event in its getEvents response.
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

	// EventIndex is the event's position in its operation's contract-event list. With (Ledger,
	// TxHash, OperationIndex) it uniquely identifies the event, matching the soroban_events PK.
	//
	// CRITICAL: without it, an operation emitting 2+ contract events (Phoenix emits 8 per swap)
	// collapses to one row under ON CONFLICT DO NOTHING and the raw landing zone silently loses
	// the rest.
	//
	// Populated only by the production dispatcher path. It is not part of the stellar-rpc wire
	// shape (RPC encodes position in the opaque `ID`), so `json:"-"` keeps RPC fixture replays
	// byte-identical.
	EventIndex int `json:"-"`

	// Topic entries are base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Topic []string `json:"topic"`
	// Value is base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Value string `json:"value"`

	// OpArgs carries the base64-encoded SCVal arguments of the InvokeHostFunction operation that
	// produced this event, populated by internal/dispatcher.
	//
	// Empty for events not produced by an InvokeContract call, events from stellar-rpc getEvents
	// (which omits the op args), and CAP-67 transfer events tied to classic ops.
	//
	// Redstone needs it: the WritePrices `updated_feeds` vec carries price and timestamps but no
	// feed_id, so the feed_ids from the op args are zipped in at decode time. `omitempty` keeps
	// RPC fixture replays round-tripping unchanged.
	OpArgs []string `json:"opArgs,omitempty"`

	// StateWriteKeys carries the base64-encoded XDR LedgerKeys of the contract-data entries owned
	// by THIS event's contract whose VALUE was changed by the same operation (created, or updated
	// with a post-image differing from the pre-image; an identical-value rewrite does not count).
	// The live path fills it from tx meta and the ClickHouse re-derive path from
	// ledger_entry_changes, both via internal/dispatcher/state_write_keys.go.
	//
	// Empty for stellar-rpc events and for readers that did not opt in (the fetch is per-source
	// opt-in, like OpArgs). Decoders MUST treat absence as "unknown", not "no writes".
	StateWriteKeys []string `json:"stateWriteKeys,omitempty"`
}

// EventClosedAt parses the RFC 3339 LedgerClosedAt string into a time.Time.
//
// An empty LedgerClosedAt returns an error rather than the zero time: a zero timestamp reaching
// the trades hypertable breaks VWAP windows and time-ordered queries, so failing closed at the
// decoder boundary is safer.
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
