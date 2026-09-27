package metadata

import (
	"context"
	"time"
)

// contextWithTimeoutMs derives a ms-bounded child of parent, so the
// lookup inherits the request's cancellation.
func contextWithTimeoutMs(parent context.Context, ms int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(ms)*time.Millisecond)
}
