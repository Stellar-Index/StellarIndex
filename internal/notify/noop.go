package notify

import (
	"context"
	"sync"
)

// NoopSender is a test double: it records every Send (readable through Sent / Last) without
// dispatching, and reports success.
//
// A running binary must never fall back to it when the provider credential is missing: nothing
// outside the process can read Sent, so the mail is lost while the nil return is counted as
// result="sent". A deployment with no credential wires [UnconfiguredSender], whose Send errors.
//
// Validation still runs, so tests cannot pass with a template that would fail at first real send.
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
