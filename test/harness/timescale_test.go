//go:build integration

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestTimescaleWaitStrategyGatesOnMappedPort reproduces the flake the
// port check exists to close, without Docker.
//
// lateBind is a container that is ready by LOG from the first poll but
// whose host port binding only lands one inspect later — the ordering
// Docker Desktop produces under parallel shards. A readiness gate that
// only reads the log returns while ConnectionString still cannot
// resolve a port, which surfaces to the caller as
// `conn str: port "5432/tcp" not found`.
//
// Reverting TimescaleWaitStrategy to the bare
// wait.ForLog(...).WithOccurrence(2) turns this red on exactly that
// message.
func TestTimescaleWaitStrategyGatesOnMappedPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	target := newLateBindTarget(t)
	if err := TimescaleWaitStrategy().WaitUntilReady(ctx, target); err != nil {
		t.Fatalf("wait until ready: %v", err)
	}

	// What tcpostgres.ConnectionString does with the container the
	// strategy just declared ready.
	if _, err := target.MappedPort(ctx, "5432/tcp"); err != nil {
		t.Fatalf("conn str: %v — the readiness gate returned before the host port binding "+
			"existed, so the DSN handed to every integration test cannot be built", err)
	}
}

// lateBindTarget is a wait.StrategyTarget whose logs say "ready" from
// the outset while its port binding appears only after the first
// MappedPort call. A real listener stands in for the published port so
// the strategy's external dial has something to connect to.
type lateBindTarget struct {
	mu          sync.Mutex
	mappedCalls int

	host string
	port network.Port
	logs []byte
}

func newLateBindTarget(t *testing.T) *lateBindTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	num, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	port, ok := network.PortFrom(uint16(num), network.TCP)
	if !ok {
		t.Fatalf("port from %d/tcp", num)
	}

	// Both occurrences of the readiness line are present from the first
	// poll: the log tells the truth about postgres and nothing about
	// the port publication, which is the whole point.
	const ready = "database system is ready to accept connections\n"
	return &lateBindTarget{
		host: "127.0.0.1",
		port: port,
		logs: []byte(ready + ready),
	}
}

func (c *lateBindTarget) Host(context.Context) (string, error) { return c.host, nil }

func (c *lateBindTarget) MappedPort(_ context.Context, port string) (network.Port, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mappedCalls++
	if c.mappedCalls < 2 {
		// Verbatim the message testcontainers' MappedPort returns while
		// NetworkSettings.Ports holds the exposed port with an empty
		// binding list.
		return network.Port{}, fmt.Errorf("port %q not found", port)
	}
	return c.port, nil
}

func (c *lateBindTarget) bound() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mappedCalls >= 2
}

func (c *lateBindTarget) Inspect(context.Context) (*container.InspectResponse, error) {
	ports := network.PortMap{network.MustParsePort("5432/tcp"): nil}
	if c.bound() {
		ports[network.MustParsePort("5432/tcp")] = []network.PortBinding{
			{HostIP: netip.MustParseAddr(c.host), HostPort: c.port.Port()},
		}
	}
	return &container.InspectResponse{
		HostConfig:      &container.HostConfig{},
		NetworkSettings: &container.NetworkSettings{Ports: ports},
	}, nil
}

func (c *lateBindTarget) Ports(ctx context.Context) (network.PortMap, error) {
	inspect, err := c.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	return inspect.NetworkSettings.Ports, nil
}

func (c *lateBindTarget) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(c.logs)), nil
}

// Exec answers the strategy's in-container "is the port listening"
// probe with success; this fake's whole subject is the HOST binding.
func (c *lateBindTarget) Exec(context.Context, []string, ...tcexec.ProcessOption) (int, io.Reader, error) {
	return 0, bytes.NewReader(nil), nil
}

func (c *lateBindTarget) State(context.Context) (*container.State, error) {
	return &container.State{Status: container.StateRunning, Running: true}, nil
}

func (c *lateBindTarget) CopyFileFromContainer(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

var _ wait.StrategyTarget = (*lateBindTarget)(nil)
