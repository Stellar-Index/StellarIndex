package notify

import (
	"context"
	"sync"
)

// NoopSender records every Send call without dispatching to a
// real provider. Used by:
//
//   - Tests that want to assert "an email was sent" without
//     pretending to be Resend
//   - Dev environments before the operator has configured
//     STELLARINDEX_RESEND_API_KEY (lets the auth flow run end-
//     to-end with magic-link tokens viewable via the in-memory
//     Sent slice)
//
// Validation still runs — a NoopSender that accepts a
// missing-Subject Message would let test harnesses ship broken
// templates that pass tests but fail at first send in prod.
type NoopSender struct {
	mu   sync.Mutex
	Sent []Message
}

// Send records the message after running the same validation
// the real provider would.
func (n *NoopSender) Send(_ context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Sent = append(n.Sent, msg)
	return nil
}

// SentCount is a test convenience.
func (n *NoopSender) SentCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.Sent)
}

// Last returns the most-recently-sent message; ok=false if
// none yet.
func (n *NoopSender) Last() (Message, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.Sent) == 0 {
		return Message{}, false
	}
	return n.Sent[len(n.Sent)-1], true
}

// Reset clears the recorded slice — for tests that mint
// multiple users and want to inspect each round in isolation.
func (n *NoopSender) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Sent = nil
}
