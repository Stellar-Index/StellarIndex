// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// Every ops-side opener in this package must put the ops-batch identity
// ON THE WIRE when STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD are set, and
// the `default` user when they are not (2026-08-28 r1: ch-rebuild as
// `default` starved the aggregator's supply refresher; the whole fix is
// that these openers authenticate as the low-priority `ops_batch` user).
//
// TestOpsAuthFrom pins the resolution rules; this pins that the openers
// USE them. A fake ClickHouse listener accepts the driver's dial and
// decodes the native-protocol client hello — the first packet, which
// carries database/username/password in the clear on the loopback — so
// the value asserted is exactly what clickhouse-server would see, not a
// mock's record of an Auth struct. No CH server is needed: the listener
// closes the socket after the hello, the opener's Ping fails, and only
// the captured identity matters.
func TestOpsOpenersAuthenticateFromEnv(t *testing.T) {
	openers := []struct {
		name string
		open func(ctx context.Context, addr string) error
	}{
		{"Sink.Open", func(ctx context.Context, addr string) error {
			_, err := Open(ctx, addr, 0)
			return err
		}},
		{"openRead (gate/verify/reconcile readers)", func(ctx context.Context, addr string) error {
			_, err := openRead(ctx, addr)
			return err
		}},
		{"NewExplorerReader", func(ctx context.Context, addr string) error {
			_, err := NewExplorerReader(ctx, addr)
			return err
		}},
		{"NewSupplyReader", func(ctx context.Context, addr string) error {
			_, err := NewSupplyReader(ctx, addr)
			return err
		}},
		{"openParticipantWrite", func(ctx context.Context, addr string) error {
			_, err := openParticipantWrite(ctx, addr)
			return err
		}},
		{"openAccountMovementsWrite", func(ctx context.Context, addr string) error {
			_, err := openAccountMovementsWrite(ctx, addr)
			return err
		}},
		{"InsertEntryChanges", func(ctx context.Context, addr string) error {
			// One row so the function reaches its dial (zero rows
			// returns before opening anything).
			_, err := InsertEntryChanges(ctx, addr, []LedgerEntryChangeRow{{}}, 0)
			return err
		}},
	}

	cases := []struct {
		name         string
		user, pass   string
		wantUser     string
		wantPassword string
	}{
		// The password deliberately carries the characters
		// run-heavy-job.sh must hand through verbatim (see
		// scripts/ci/run-heavy-job-test.sh): the Go side is byte-exact too.
		{"pair set → ops_batch on the wire", "ops_batch", `p=a$s"s w0rd'#x=`, "ops_batch", `p=a$s"s w0rd'#x=`},
		// clickhouse-go substitutes CH's literal `default` user for an
		// empty Auth.Username, so that is what the server sees.
		{"pair unset → CH default user (pre-fix behaviour)", "", "", "default", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(OpsUserEnv, tc.user)
			t.Setenv(OpsPasswordEnv, tc.pass)
			for _, op := range openers {
				t.Run(op.name, func(t *testing.T) {
					addr, hellos := listenForClientHello(t)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := op.open(ctx, addr); err == nil {
						t.Fatalf("%s succeeded against a listener that never answers — the test is not observing the dial", op.name)
					}
					var got clientHello
					select {
					case got = <-hellos:
					case <-time.After(5 * time.Second):
						t.Fatalf("%s: no client hello reached the listener", op.name)
					}
					if got.Database != "stellar" {
						t.Errorf("%s: database on the wire = %q, want %q", op.name, got.Database, "stellar")
					}
					if got.Username != tc.wantUser {
						t.Errorf("%s: username on the wire = %q, want %q", op.name, got.Username, tc.wantUser)
					}
					if got.Password != tc.wantPassword {
						t.Errorf("%s: password on the wire = %q, want %q", op.name, got.Password, tc.wantPassword)
					}
				})
			}
		})
	}
}

// clientHello is the identity triple from the ClickHouse native-protocol
// client hello (clickhouse-go v2 conn_handshake.go: ClientHello byte,
// client name, version major/minor, protocol revision, then database,
// username, password — strings are uvarint-length-prefixed).
type clientHello struct {
	Database, Username, Password string
}

// listenForClientHello starts a loopback listener that decodes the client
// hello of every connection it receives, delivers it on the returned
// channel and then closes the socket. Buffered so a driver that redials
// after the EOF cannot block the accept loop.
func listenForClientHello(t *testing.T) (addr string, hellos <-chan clientHello) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan clientHello, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				h, err := readClientHello(bufio.NewReader(c))
				if err != nil {
					t.Logf("listener: decoding client hello: %v", err)
					return
				}
				ch <- h
			}(c)
		}
	}()
	return ln.Addr().String(), ch
}

func readClientHello(r *bufio.Reader) (clientHello, error) {
	readString := func() (string, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return "", err
		}
		if n > 1<<16 {
			return "", fmt.Errorf("implausible string length %d", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	}
	pkt, err := r.ReadByte()
	if err != nil {
		return clientHello{}, err
	}
	if pkt != 0 { // proto.ClientHello
		return clientHello{}, fmt.Errorf("first packet type %d, want ClientHello (0)", pkt)
	}
	if _, err := readString(); err != nil { // client name
		return clientHello{}, err
	}
	for i := 0; i < 3; i++ { // version major, minor, protocol revision
		if _, err := binary.ReadUvarint(r); err != nil {
			return clientHello{}, err
		}
	}
	var h clientHello
	if h.Database, err = readString(); err != nil {
		return clientHello{}, err
	}
	if h.Username, err = readString(); err != nil {
		return clientHello{}, err
	}
	if h.Password, err = readString(); err != nil {
		return clientHello{}, err
	}
	return h, nil
}
