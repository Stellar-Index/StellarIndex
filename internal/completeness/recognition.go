// Package completeness implements the ADR-0033 verification model: data-derived checks that turn
// "100% coverage" into three independently provable claims.
//
//   - Substrate continuity (ledger_ingest_log: contiguity + hash chain).
//   - Recognition (this file): every on-chain event shape is one a decoder handles.
//   - Projection reconciliation (reconcile.go): every captured event became a row.
//
// It consumes internal/storage/timescale and internal/dispatcher through narrow interfaces; nothing
// imports it back.
package completeness

import (
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Recognizer decides whether any decoder claims an event, without decoding it; *dispatcher.Dispatcher satisfies it via Recognize.
type Recognizer interface {
	Recognize(events.Event) (string, bool)
}

// RecognitionGap is a distinct on-chain event shape that NO decoder recognizes (seen in soroban_events but silently dropped on decode), surfaced loudly instead of vanishing.
type RecognitionGap struct {
	ContractID string
	Topic0Sym  string // "" when topic[0] is not a Symbol/String
	Count      int64  // events of this shape in the audited range
	MinLedger  uint32
	MaxLedger  uint32
	Reason     string // "no decoder matches" | "unreconstructable: <err>"
}

// AuditRecognition reconstructs a representative event per distinct (contract, topic) shape and returns those the Recognizer rejects; a sample that cannot be reconstructed is itself a gap.
func AuditRecognition(samples []timescale.TopicSample, r Recognizer) []RecognitionGap {
	var gaps []RecognitionGap
	for i := range samples {
		s := samples[i]
		ev, err := sorobanevents.Reconstruct(s.Row)
		if err != nil {
			gaps = append(gaps, gapFromSample(s, "unreconstructable: "+err.Error()))
			continue
		}
		if _, ok := r.Recognize(ev); !ok {
			gaps = append(gaps, gapFromSample(s, "no decoder matches"))
		}
	}
	return gaps
}

func gapFromSample(s timescale.TopicSample, reason string) RecognitionGap {
	return RecognitionGap{
		ContractID: s.Row.ContractID,
		Topic0Sym:  s.Row.Topic0Sym,
		Count:      s.Count,
		MinLedger:  s.MinLedger,
		MaxLedger:  s.MaxLedger,
		Reason:     reason,
	}
}
