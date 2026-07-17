package domain

import "time"

// SorobanEventRow is one captured soroban_events row, ready for
// batched insert into the ADR-0029 catch-all landing zone. Fields
// map 1:1 to the columns in migration 0041; *string / *[]byte
// represent nullable columns. Canonical home of
// internal/sources/sorobanevents.Row — see doc.go.
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

	// Topic0XDR is always populated; Topic1XDR..Topic3XDR are nil
	// when the event has fewer topics. These four fixed columns are
	// RETAINED for back-compat (the topic_0_sym index fast-path and
	// existing SQL readers) — the COMPLETE ordered topic list lives in
	// TopicsXDR (migration 0114) so events with 5+ topics don't
	// truncate.
	Topic0XDR []byte
	Topic1XDR []byte
	Topic2XDR []byte
	Topic3XDR []byte

	// TopicsXDR is the COMPLETE ordered list of every topic's raw XDR
	// bytes, in emit order (migration 0114, audit-2026-07-16 C2-11).
	// Authoritative for the full topic set; Topic0XDR..Topic3XDR hold
	// the first four for back-compat. Empty for legacy rows written
	// before 0114 — the reader falls back to Topic0XDR..Topic3XDR in
	// that case (every real contract event has >=1 topic, so a
	// non-empty TopicsXDR unambiguously means "captured whole").
	TopicsXDR [][]byte

	// BodyXDR is the raw XDR of the event body SCVal.
	BodyXDR []byte

	// OpArgsXDR is the XDR-marshalled ScVec of the originating
	// InvokeContract op's args, or nil when the event didn't come
	// from an InvokeContract op.
	OpArgsXDR []byte
}
