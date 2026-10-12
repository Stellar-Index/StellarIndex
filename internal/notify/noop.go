package notify

import (
	"context"
	"sync"
)

// NoopSender is a test double that records every Send without dispatching and reports success; a running binary must NEVER fall back to it when the credential is missing (wire UnconfiguredSender, whose Send errors), and validation still runs.
type NoopSender struct {
	mu   sync.Mutex
	Sent []Message
}

// Send records the message after running the same validation the real provider would.
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

// Last returns the most-recently-sent message; ok=false if none yet.
func (n *NoopSender) Last() (Message, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.Sent) == 0 {
		return Message{}, false
	}
	return n.Sent[len(n.Sent)-1], true
}

// Reset clears the recorded slice, for tests that inspect each round in isolation.
func (n *NoopSender) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Sent = nil
}
