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

// Event is a single Soroban contract event. JSON tags follow the stellar-rpc getEvents response
// shape, which the RPC client and fixture replays still populate.
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
	// TxHash, OperationIndex) it identifies the event, matching the soroban_events PK; without it an
	// operation emitting 2+ events (Phoenix emits 8 per swap) collapses to one row under ON CONFLICT
	// DO NOTHING. Only the dispatcher populates it; `json:"-"` keeps RPC fixture replays byte-identical.
	EventIndex int `json:"-"`

	// Topic entries are base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Topic []string `json:"topic"`
	// Value is base64-encoded SCVal. Decoders parse via
	// internal/scval.
	Value string `json:"value"`

	// OpArgs carries the base64-encoded SCVal arguments of the InvokeHostFunction operation that
	// produced this event. Empty for non-InvokeContract events, stellar-rpc events and CAP-67 transfers
	// tied to classic ops. Redstone needs it: WritePrices `updated_feeds` has no feed_id, so the op
	// args' feed_ids are zipped in at decode time.
	OpArgs []string `json:"opArgs,omitempty"`

	// StateWriteKeys carries the base64-encoded XDR LedgerKeys of contract-data entries owned by THIS
	// event's contract whose value was changed by the same operation (an identical-value rewrite does
	// not count). Filled via internal/dispatcher/state_write_keys.go, per-source opt-in like OpArgs.
	// Decoders MUST treat absence as "unknown", not "no writes".
	StateWriteKeys []string `json:"stateWriteKeys,omitempty"`
}

// EventClosedAt parses the RFC 3339 LedgerClosedAt string into a time.Time.
//
// An empty LedgerClosedAt is an error, not the zero time: a zero timestamp reaching the trades
// hypertable breaks VWAP windows and time-ordered queries.
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
