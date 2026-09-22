package sorobanevents

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Reconstruct projects a soroban_events Row back into an
// [events.Event] suitable for feeding into a per-source decoder.
// This is the inverse of [Capture] for the fields that the
// historical-backfill path needs: contract_id, topics 0-3 (base64-
// re-encoded so the standard scval.Parse pathway works), body, and
// the originating op args when present.
//
// The returned Event has Type="contract", LedgerClosedAt formatted
// as RFC 3339 (matching what the dispatcher would have stamped),
// and OpArgs reconstructed from the stored ScVec when non-nil.
// ID + TransactionIndex are left empty — they're metadata for
// stellar-rpc replays, not used by any decoder.
//
// Used by the projector (`internal/projector`, including
// `stellarindex-ops projector-replay` — the one catch-up path per
// ADR-0032) and the completeness reconciler to walk soroban_events
// for a range and re-feed the same Go decoder live ingest uses,
// persisting to the per-source hypertable.
func Reconstruct(row Row) (events.Event, error) {
	if row.ContractID == "" {
		return events.Event{}, fmt.Errorf("sorobanevents.Reconstruct: empty contract_id (ledger=%d)", row.Ledger)
	}
	if len(row.Topic0XDR) == 0 {
		return events.Event{}, fmt.Errorf("sorobanevents.Reconstruct: missing topic_0_xdr (ledger=%d, contract=%s)", row.Ledger, row.ContractID)
	}
	if len(row.TxHash) != 32 {
		return events.Event{}, fmt.Errorf("sorobanevents.Reconstruct: tx_hash is %d bytes, want 32 (ledger=%d)", len(row.TxHash), row.Ledger)
	}

	topics, err := reconstructTopics(row)
	if err != nil {
		return events.Event{}, err
	}

	var opArgs []string
	if len(row.OpArgsXDR) > 0 {
		out, err := scval.DecodeScVecToArgs(row.OpArgsXDR)
		if err != nil {
			return events.Event{}, fmt.Errorf("sorobanevents.Reconstruct: op_args: %w", err)
		}
		opArgs = out
	}

	return events.Event{
		Type:           "contract",
		Ledger:         row.Ledger,
		LedgerClosedAt: row.LedgerCloseTime.UTC().Format(time.RFC3339),
		ContractID:     row.ContractID,
		OperationIndex: int(row.OpIndex),
		EventIndex:     int(row.EventIndex),
		TxHash:         hex.EncodeToString(row.TxHash),
		Topic:          topics,
		Value:          base64.StdEncoding.EncodeToString(row.BodyXDR),
		OpArgs:         opArgs,
	}, nil
}

// reconstructTopics base64-re-encodes the stored topic XDR bytes
// into the slice shape decoders expect.
//
// Prefers the COMPLETE ordered topics_xdr list (migration 0114,
// audit-2026-07-16 C2-11) so events with 5+ topics reconstruct with
// every topic instead of the pre-fix cap of 4. Rows written before
// 0114 (or by a pre-0114 binary) carry an empty TopicsXDR — fall back
// to the fixed topic_0..3 columns, trimmed to TopicCount so events
// that legitimately had fewer than 4 topics don't carry empty
// trailing strings. Those legacy rows still cap at 4 (that is all
// their storage kept); a ClickHouse-lake re-project recovers the
// truncated tail.
//
// An empty slot ahead of a non-empty one is not a "fewer topics"
// case (those trail off with nothing after them) — it's a storage
// gap. Decoders dispatch on topic[0] and read the rest by position
// (Q123), so silently skipping the gap would shift every later topic
// down one slot and hand a decoder the wrong field at the wrong
// index. Fail closed instead of returning a misaligned slice.
func reconstructTopics(row Row) ([]string, error) {
	if len(row.TopicsXDR) > 0 {
		return dropTrailingEmpty(row.TopicsXDR, row)
	}

	// Legacy fallback: the four fixed topic columns.
	xdrs := [4][]byte{row.Topic0XDR, row.Topic1XDR, row.Topic2XDR, row.Topic3XDR}
	want := int(row.TopicCount)
	if want > 4 {
		// Pre-0114 rows only stored topics 0-3; reflect that cap
		// rather than the higher count the original event had.
		want = 4
	}
	return dropTrailingEmpty(xdrs[:want], row)
}

// dropTrailingEmpty base64-encodes a topic slot slice, allowing only
// a trailing run of empty slots (the legitimate "fewer topics than
// the fixed column count" case). A non-empty slot found after an
// empty one is a storage gap and returns an error rather than a
// position-shifted slice.
func dropTrailingEmpty(xdrs [][]byte, row Row) ([]string, error) {
	out := make([]string, 0, len(xdrs))
	gapAt := -1
	for i, x := range xdrs {
		if len(x) == 0 {
			if gapAt < 0 {
				gapAt = i
			}
			continue
		}
		if gapAt >= 0 {
			return nil, fmt.Errorf(
				"sorobanevents.Reconstruct: topic gap at index %d, non-empty topic %d follows it (ledger=%d, contract=%s)",
				gapAt, i, row.Ledger, row.ContractID)
		}
		out = append(out, base64.StdEncoding.EncodeToString(x))
	}
	return out, nil
}
