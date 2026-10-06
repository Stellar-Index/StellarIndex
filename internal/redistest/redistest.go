// Package redistest provides Redis test doubles whose outage cannot be
// undone by another process on the machine.
package redistest

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Server is a miniredis plus a client whose dialer refuses once Kill runs.
type Server struct {
	*miniredis.Miniredis
	Client *redis.Client
	down   atomic.Bool
}

// Run starts a miniredis and a client for it, both cleaned up with t.
func Run(t testing.TB) *Server {
	t.Helper()
	s := &Server{Miniredis: miniredis.RunT(t)}
	var d net.Dialer
	s.Client = redis.NewClient(&redis.Options{
		Addr: s.Addr(),
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if s.down.Load() {
				return nil, errors.New("redistest: server killed")
			}
			return d.DialContext(ctx, network, addr)
		},
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = s.Client.Close() })
	return s
}

// Kill takes the server down. Unlike closing a miniredis and keeping its
// address, no later dial can reach a different process that has bound the
// freed ephemeral port.
func (s *Server) Kill() {
	s.down.Store(true)
	s.Close()
}
