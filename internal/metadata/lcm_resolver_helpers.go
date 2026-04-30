package metadata

import (
	"context"
	"time"
)

// contextWithTimeoutMs is a small helper isolated to keep the
// import of "time" out of [lcm_resolver.go] (which only deals
// with stable interface contracts and benefits from a tighter
// import surface).
func contextWithTimeoutMs(ms int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(ms)*time.Millisecond)
}
