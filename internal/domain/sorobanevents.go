package domain

import "time"

// SorobanEventRow is one soroban_events row (ADR-0029), columns 1:1 with
// migration 0041.
type SorobanEventRow struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          []byte // 32-byte raw
	OpIndex         int16
	EventIndex      int16

	ContractID    string // C-strkey
	ContractIDHex []byte // 32-byte raw

	TopicCount int16

	// Topic0Sym is the decoded Symbol/String of topic[0] when it's
	// of one of those types; "" otherwise (sink writes SQL NULL).
	Topic0Sym string

	// The first four topics, kept for the topic_0_sym index and existing SQL
	// readers; TopicsXDR holds the complete list. Topic1..3 are nil when absent.
	Topic0XDR []byte
	Topic1XDR []byte
	Topic2XDR []byte
	Topic3XDR []byte

	// TopicsXDR is every topic's raw XDR in emit order (migration 0114). Empty on
	// older rows, where readers fall back to Topic0..3XDR; any real event has a
	// topic, so non-empty means captured whole.
	TopicsXDR [][]byte

	// BodyXDR is the raw XDR of the event body SCVal.
	BodyXDR []byte

	// OpArgsXDR is the originating InvokeContract op's args as an ScVec; nil otherwise.
	OpArgsXDR []byte
}
