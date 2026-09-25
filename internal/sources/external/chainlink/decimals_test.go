package chainlink

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// decimalsReturn encodes a decimals() uint8 result as the 32-byte
// ABI word the RPC returns.
func decimalsReturn(v uint8) string {
	var buf [32]byte
	buf[31] = v
	return "0x" + hex.EncodeToString(buf[:])
}

// fakeRPC is a selector-dispatching JSON-RPC fake: eth_call
// latestRoundData() answers roundHex, eth_call decimals() answers the
// current decimalsValue (or the configured HTTP status when != 200),
// eth_getLogs answers an empty list and eth_blockNumber a small head.
// Call kinds are counted so tests can assert the read cadence.
type fakeRPC struct {
	srv            *httptest.Server
	roundHex       string
	decimalsValue  atomic.Int64
	decimalsStatus atomic.Int64
	decimalsCalls  atomic.Int64
	roundCalls     atomic.Int64
	getLogsCalls   atomic.Int64
}

func newFakeRPC(t *testing.T, roundHex string, decimals uint8, decimalsStatus int) *fakeRPC {
	t.Helper()
	f := &fakeRPC{roundHex: roundHex}
	f.decimalsValue.Store(int64(decimals))
	f.decimalsStatus.Store(int64(decimalsStatus))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "eth_blockNumber":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
		case "eth_getLogs":
			f.getLogsCalls.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":[]}`)
		case "eth_call":
			var call struct {
				Data string `json:"data"`
			}
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params[0], &call)
			}
			if call.Data == SelDecimals {
				f.decimalsCalls.Add(1)
				if st := int(f.decimalsStatus.Load()); st != http.StatusOK {
					w.WriteHeader(st)
					fmt.Fprint(w, `{"error":{"code":-32603,"message":"internal"}}`)
					return
				}
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, decimalsReturn(uint8(f.decimalsValue.Load())))
				return
			}
			f.roundCalls.Add(1)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, f.roundHex)
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeClock is a mutex-guarded injectable clock for the refresh/retry
// cadence tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newDecimalsTestPoller builds a Poller against the fake with one feed
// at the given configured decimals, an injected clock and a captured
// logger.
func newDecimalsTestPoller(f *fakeRPC, pair canonical.Pair, address string, configured uint8) (*Poller, *fakeClock, *bytes.Buffer) {
	var logBuf bytes.Buffer
	clock := &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	p := NewPoller(f.srv.URL, map[string]FeedSpec{
		pair.String(): {Address: address, Decimals: configured},
	})
	p.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	p.now = clock.Now
	return p, clock, &logBuf
}

func testPair(base, quote string) canonical.Pair {
	return canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: base},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: quote},
	}
}

// TestPollOnce_decimalsAbsentAdoptsOnChain — a feed spec with no
// decimals adopts the contract's decimals(). The fake publishes at 18;
// the pre-fix poller stamped DefaultDecimals (8) on the row, so every
// downstream scale of this feed would have been 10^10 off.
func TestPollOnce_decimalsAbsentAdoptsOnChain(t *testing.T) {
	t.Parallel()
	pair := testPair("ETH", "USD")
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 100, big.NewInt(2_500_00000000), 0, 1767225600, 100), 18, http.StatusOK)
	p, _, _ := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a1", 0)

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1", len(updates))
	}
	if updates[0].Decimals != 18 {
		t.Errorf("Decimals = %d, want the on-chain 18 (absent config must adopt the chain, not the built-in 8)", updates[0].Decimals)
	}
	if updates[0].Price.String() != "250000000000" {
		t.Errorf("Price = %q, want the raw answer 250000000000 (rows store raw integers, scale travels in Decimals)", updates[0].Price.String())
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times, want 1", n)
	}
}

// TestPollOnce_decimalsEqualFlows — configured 8, chain 8: rows flow at
// 8 and nothing is counted as a mismatch.
func TestPollOnce_decimalsEqualFlows(t *testing.T) {
	t.Parallel()
	pair := testPair("BTC", "USD")
	before := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("ingest", pair.String()))
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 7, big.NewInt(6_543_210_000_000), 0, 1767225600, 7), 8, http.StatusOK)
	p, _, _ := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a2", 8)

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 1 || updates[0].Decimals != 8 {
		t.Fatalf("updates = %+v, want one row at 8 decimals", updates)
	}
	if after := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("ingest", pair.String())); after != before {
		t.Errorf("mismatch counter moved %v → %v on an agreeing feed", before, after)
	}
}

// TestPollOnce_decimalsMismatchRefusedAndCounted — configured 8 but
// the chain says 18: the feed is refused every tick (ErrDecimalsMismatch,
// no row, no price read), each refusal increments the counter, decimals()
// is re-read only after the retry interval, and rows resume once config
// and chain agree.
func TestPollOnce_decimalsMismatchRefusedAndCounted(t *testing.T) {
	t.Parallel()
	pair := testPair("LINK", "USD")
	counter := obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("ingest", pair.String())
	before := testutil.ToFloat64(counter)
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 9, big.NewInt(1_500_000_000), 0, 1767225600, 9), 18, http.StatusOK)
	p, clock, logBuf := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a3", 8)

	for i := 1; i <= 2; i++ {
		_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
		if !errors.Is(err, ErrDecimalsMismatch) {
			t.Fatalf("tick %d: err = %v, want ErrDecimalsMismatch (all-feeds-failed surfaces the cause)", i, err)
		}
		if len(updates) != 0 {
			t.Fatalf("tick %d: emitted %d rows from a refused feed, want 0", i, len(updates))
		}
		if !strings.Contains(err.Error(), "configured decimals=8") || !strings.Contains(err.Error(), "decimals()=18") {
			t.Errorf("tick %d: error %q must name both values", i, err)
		}
	}
	if got := testutil.ToFloat64(counter) - before; got != 2 {
		t.Errorf("mismatch counter advanced by %v across two refused ticks, want 2", got)
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times while refused, want 0", n)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times within the retry window, want 1", n)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "decimals mismatch") {
		t.Errorf("no ERROR mismatch line logged; got:\n%s", logBuf.String())
	}

	// Chain and config agree again → rows resume at the agreed scale.
	f.decimalsValue.Store(8)
	clock.Advance(decimalsRetryInterval + time.Second)
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("after agreement: %v", err)
	}
	if len(updates) != 1 || updates[0].Decimals != 8 {
		t.Fatalf("after agreement updates = %+v, want one row at 8", updates)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}
}

// TestPollOnce_decimalsRPCErrorKeepsConfiguredWithRetry — decimals()
// fails (500) but latestRoundData() works: the configured value is kept
// with a WARN, the read is retried only after the retry interval, and
// nothing crashes.
func TestPollOnce_decimalsRPCErrorKeepsConfiguredWithRetry(t *testing.T) {
	t.Parallel()
	pair := testPair("ETH", "EUR")
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 3, big.NewInt(2_300_00000000), 0, 1767225600, 3), 8, http.StatusInternalServerError)
	p, clock, logBuf := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a4", 8)

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce with failing decimals(): %v — configured value must be kept", err)
	}
	if len(updates) != 1 || updates[0].Decimals != 8 {
		t.Fatalf("updates = %+v, want one row at the configured 8", updates)
	}
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "decimals() read failed") {
		t.Errorf("no WARN line for the failed decimals() read; got:\n%s", logBuf.String())
	}

	// Within the retry window: no re-read (the round dedups, that is fine).
	if _, _, err := p.PollOnce(context.Background(), []canonical.Pair{pair}); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times inside the retry window, want 1", n)
	}
	clock.Advance(decimalsRetryInterval + time.Second)
	f.decimalsStatus.Store(http.StatusOK)
	if _, _, err := p.PollOnce(context.Background(), []canonical.Pair{pair}); err != nil {
		t.Fatalf("PollOnce after retry: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}
}

// TestPollOnce_decimalsRPCErrorWithoutConfigRefuses — no configured
// value AND decimals() unavailable: no scale is known, so the feed is
// refused (ErrDecimalsUnresolved), never projected at a guess.
func TestPollOnce_decimalsRPCErrorWithoutConfigRefuses(t *testing.T) {
	t.Parallel()
	pair := testPair("BTC", "EUR")
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 5, big.NewInt(6_000_000_000_000), 0, 1767225600, 5), 8, http.StatusInternalServerError)
	p, _, _ := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a5", 0)

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if !errors.Is(err, ErrDecimalsUnresolved) {
		t.Fatalf("err = %v, want ErrDecimalsUnresolved", err)
	}
	if len(updates) != 0 {
		t.Errorf("emitted %d rows with no known scale, want 0", len(updates))
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times with no known scale, want 0", n)
	}
}

// TestBackfill_decimalsMismatchRefusesFeed — the historical walk goes
// through the same gate: a mis-scaled feed is not walked at all.
func TestBackfill_decimalsMismatchRefusesFeed(t *testing.T) {
	t.Parallel()
	pair := testPair("LINK", "EUR")
	f := newFakeRPC(t, buildLatestRoundDataReturn(t, 1, big.NewInt(1), 0, 1767225600, 1), 18, http.StatusOK)
	p, _, _ := newDecimalsTestPoller(f, pair, "0x00000000000000000000000000000000000000a6", 8)

	out := make(chan canonical.OracleUpdate, 16)
	err := p.Backfill(context.Background(), []canonical.Pair{pair}, BackfillOptions{FromBlock: 1, ToBlock: 50}, out)
	if !errors.Is(err, ErrDecimalsMismatch) {
		t.Fatalf("Backfill err = %v, want ErrDecimalsMismatch", err)
	}
	n := 0
	for range out {
		n++
	}
	if n != 0 {
		t.Errorf("backfill emitted %d rows from a refused feed, want 0", n)
	}
	if calls := f.getLogsCalls.Load(); calls != 0 {
		t.Errorf("eth_getLogs called %d times for a refused feed, want 0", calls)
	}
}

// TestProject_zeroDecimalsRefused — the last gate: project never
// substitutes a scale for a literal 0.
func TestProject_zeroDecimalsRefused(t *testing.T) {
	t.Parallel()
	p := NewPoller("", nil)
	_, err := p.project(testPair("ETH", "USD"), FeedSpec{Address: "0xabc"}, Round{
		FeedAddress: "0xabc", RoundID: big.NewInt(1), Answer: "100", UpdatedAt: time.Unix(1767225600, 0).UTC(),
	})
	if !errors.Is(err, ErrDecimalsUnresolved) {
		t.Fatalf("err = %v, want ErrDecimalsUnresolved", err)
	}
}

// TestBuildFeedSet_zeroDecimalsStaysAbsent — the config adapter no
// longer substitutes 8 for an omitted decimals; 0 is the resolver's
// "adopt on-chain" signal.
func TestBuildFeedSet_zeroDecimalsStaysAbsent(t *testing.T) {
	t.Parallel()
	feeds, _, err := BuildFeedSet(map[string]FeedSpec{
		"crypto:ETH/fiat:USD": {Address: "0x00000000000000000000000000000000000000a7"},
	})
	if err != nil {
		t.Fatalf("BuildFeedSet: %v", err)
	}
	if got := feeds["crypto:ETH/fiat:USD"].Decimals; got != 0 {
		t.Errorf("omitted decimals became %d, want 0 (absent → adopt on-chain decimals())", got)
	}
}

// TestDecodeDecimals pins the uint8 word decode and its fail-loud
// shape checks.
func TestDecodeDecimals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    uint8
		wantErr bool
	}{
		{"eight", decimalsReturn(8), 8, false},
		{"eighteen", decimalsReturn(18), 18, false},
		{"max uint8", decimalsReturn(255), 255, false},
		{"zero is a broken contract", decimalsReturn(0), 0, true},
		{"256 overflows uint8", "0x" + strings.Repeat("0", 61) + "100", 0, true},
		{"round-data shape is not a word", buildLatestRoundDataReturn(t, 1, big.NewInt(1), 0, 1, 1), 0, true},
		{"empty", "0x", 0, true},
		{"not hex", "0xzz", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDecimals(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedResult) {
					t.Fatalf("err = %v, want ErrMalformedResult", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveDecimals_sharedAddressVerdictIsPerPair — two canonical
// pairs mapped to one feed address (the invert pattern) with different
// configured decimals must each get the verdict for THEIR OWN spec,
// whichever resolves first. The on-chain value is cached per address;
// a mismatch verdict must not be.
func TestResolveDecimals_sharedAddressVerdictIsPerPair(t *testing.T) {
	t.Parallel()
	const feedAddr = "0x00000000000000000000000000000000000000c7"
	agreeing := testPair("LINK", "USD")
	wrong := testPair("EUR", "USD")
	agreeSpec := FeedSpec{Address: feedAddr, Decimals: 8}
	wrongSpec := FeedSpec{Address: feedAddr, Decimals: 18}

	type step struct {
		pair canonical.Pair
		spec FeedSpec
	}
	orders := map[string][]step{
		"mismatching_first": {{wrong, wrongSpec}, {agreeing, agreeSpec}},
		"agreeing_first":    {{agreeing, agreeSpec}, {wrong, wrongSpec}},
	}
	for name, steps := range orders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRPC(t, buildLatestRoundDataReturn(t, 1, big.NewInt(1), 0, 1767225600, 1), 8, http.StatusOK)
			p, _, _ := newDecimalsTestPoller(f, agreeing, feedAddr, 8)
			for _, s := range steps {
				got, err := p.resolveDecimals(context.Background(), s.pair, s.spec)
				if s.spec.Decimals == 8 {
					if err != nil || got != 8 {
						t.Errorf("%s (configured 8, chain 8): got (%d, %v), want (8, nil)", s.pair, got, err)
					}
					continue
				}
				if !errors.Is(err, ErrDecimalsMismatch) {
					t.Errorf("%s (configured 18, chain 8): got (%d, %v), want ErrDecimalsMismatch", s.pair, got, err)
				}
			}
			if n := f.decimalsCalls.Load(); n != 1 {
				t.Errorf("decimals() read %d times, want 1 (second pair must hit the per-address cache)", n)
			}
		})
	}
}
