package sorobanevents

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Reconstruct projects a soroban_events Row back into an [events.Event] for a per-source decoder,
// the inverse of [Capture] for the fields backfill needs (contract_id, topics, body, op args).
// LedgerClosedAt is RFC 3339 as the dispatcher would stamp it; ID and TransactionIndex stay empty
// because no decoder uses them.
//
// The projector (including `stellarindex-ops projector-replay`, the one catch-up path per
// ADR-0032) and the completeness reconciler use it to re-feed the same decoder live ingest uses.
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

// reconstructTopics base64-re-encodes the stored topic XDR into the slice shape decoders expect.
//
// It prefers the complete ordered topics_xdr list so events with 5+ topics keep every topic. Legacy
// rows with an empty TopicsXDR fall back to the fixed topic_0..3 columns, trimmed to TopicCount; they
// still cap at 4, and a ClickHouse-lake re-project recovers the truncated tail.
//
// An empty slot ahead of a non-empty one is a storage gap, not "fewer topics". Decoders read topics
// by position, so skipping the gap would shift later topics down a slot and hand a decoder the wrong
// field. Fail closed instead of returning a misaligned slice.
func reconstructTopics(row Row) ([]string, error) {
	if len(row.TopicsXDR) > 0 {
		return dropTrailingEmpty(row.TopicsXDR, row)
	}

	// Legacy fallback: the four fixed topic columns.
	xdrs := [4][]byte{row.Topic0XDR, row.Topic1XDR, row.Topic2XDR, row.Topic3XDR}
	want := int(row.TopicCount)
	if want > 4 {
		// Legacy rows only stored topics 0-3; reflect that cap, not the original count.
		want = 4
	}
	return dropTrailingEmpty(xdrs[:want], row)
}

// dropTrailingEmpty base64-encodes a topic slot slice, allowing only a trailing run of empty slots
// (fewer topics than fixed columns). A non-empty slot after an empty one is a storage gap and
// errors rather than returning a position-shifted slice.
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
