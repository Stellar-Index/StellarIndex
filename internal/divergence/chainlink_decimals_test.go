package divergence

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// chainlinkFakeRPC is a selector-dispatching JSON-RPC fake for the
// on-chain decimals() verification tests: latestRoundData() answers
// roundHex; decimals() answers the current decimalsValue as a 32-byte
// word, or the configured HTTP status when decimalsStatus != 200. Both
// call kinds are counted so the cadence tests can assert how often the
// reference actually re-reads the chain.
type chainlinkFakeRPC struct {
	srv            *httptest.Server
	roundHex       string
	decimalsValue  atomic.Int64
	decimalsStatus atomic.Int64
	decimalsCalls  atomic.Int64
	roundCalls     atomic.Int64
}

func newChainlinkFakeRPC(t *testing.T, roundHex string, decimals int, decimalsStatus int) *chainlinkFakeRPC {
	t.Helper()
	f := &chainlinkFakeRPC{roundHex: roundHex}
	f.decimalsValue.Store(int64(decimals))
	f.decimalsStatus.Store(int64(decimalsStatus))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ethCallData(r) == chainlinkDecimalsSelector {
			f.decimalsCalls.Add(1)
			if st := int(f.decimalsStatus.Load()); st != http.StatusOK {
				w.WriteHeader(st)
				_, _ = w.Write([]byte(`{"error":{"code":-32603,"message":"internal"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + bigInt256Hex(big.NewInt(f.decimalsValue.Load())) + `"}`))
			return
		}
		f.roundCalls.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + f.roundHex + `"}`))
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

// newDecimalsTestRef builds a reference against the fake with one feed
// for pair, the given configured decimals, an injected clock and a
// captured logger.
func newDecimalsTestRef(t *testing.T, f *chainlinkFakeRPC, pairKey, address string, configured int) (*ChainlinkReference, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	clock := &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: f.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		FeedMap: map[string]ChainlinkFeed{
			pairKey: {Address: address, Decimals: configured, MaxAge: 76 * time.Hour},
		},
	})
	ref.now = clock.Now
	return ref, clock, &logBuf
}

// TestChainlink_Decimals_AbsentAdoptsOnChain — an operator entry with
// no decimals adopts the contract's decimals(). The fake feed publishes
// at 18 decimals with an answer of 1.08 × 10^18; the pre-fix constructor
// coerced the absent value to 8 and would have served 1.08 × 10^10.
func TestChainlink_Decimals_AbsentAdoptsOnChain(t *testing.T) {
	t.Parallel()
	answer, _ := new(big.Int).SetString("1080000000000000000", 10) // 1.08 × 10^18
	f := newChainlinkFakeRPC(t, roundDataHex(answer, time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 18, http.StatusOK)
	ref, _, _ := newDecimalsTestRef(t, f, "fiat:EUR/fiat:USD", "0x00000000000000000000000000000000000000e1", 0)
	pair := mustPair(t, "fiat:EUR", "fiat:USD")

	got, err := ref.LookupPrice(context.Background(), pair, ref.now())
	if err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	if want := 1.08; abs(got-want) > 1e-9 {
		t.Fatalf("price = %.12g, want %.12g — absent config must scale by the on-chain 18, not a default 8", got, want)
	}
	// Second read within the refresh window: served from the verified
	// state, no second decimals() round-trip.
	if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); err != nil {
		t.Fatalf("second LookupPrice: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times across two lookups, want 1 (verified value is cached)", n)
	}
}

// TestChainlink_Decimals_EqualFlows — configured 8, chain 8: readings
// flow and nothing is counted as a mismatch.
func TestChainlink_Decimals_EqualFlows(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "fiat:GBP", "fiat:USD")
	before := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String()))
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(127_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)
	ref, _, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e2", 8)

	got, err := ref.LookupPrice(context.Background(), pair, ref.now())
	if err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	if want := 1.27; abs(got-want) > 1e-9 {
		t.Errorf("price = %.12g, want %.12g", got, want)
	}
	if after := testutil.ToFloat64(obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String())); after != before {
		t.Errorf("mismatch counter moved %v → %v on an agreeing feed", before, after)
	}
}

// TestChainlink_Decimals_MismatchRefusedAndCounted — configured 8 but
// the chain says 18: every reading is refused as ErrPriceUnavailable +
// ErrChainlinkDecimalsMismatch, each refusal increments the counter, the
// price is never even read, decimals() is re-read only after the retry
// interval, and readings resume once config and chain agree.
func TestChainlink_Decimals_MismatchRefusedAndCounted(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:LINK", "fiat:USD")
	counter := obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("divergence", pair.String())
	before := testutil.ToFloat64(counter)
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(1_500_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 18, http.StatusOK)
	ref, clock, logBuf := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e3", 8)

	for i := 1; i <= 2; i++ {
		_, err := ref.LookupPrice(context.Background(), pair, ref.now())
		if !errors.Is(err, ErrPriceUnavailable) {
			t.Fatalf("lookup %d: err = %v, want ErrPriceUnavailable (Compare must classify the refusal as price_unavailable)", i, err)
		}
		if !errors.Is(err, ErrChainlinkDecimalsMismatch) {
			t.Fatalf("lookup %d: err = %v, want ErrChainlinkDecimalsMismatch", i, err)
		}
		if !strings.Contains(err.Error(), "configured decimals=8") || !strings.Contains(err.Error(), "decimals()=18") {
			t.Errorf("lookup %d: error %q must name both values", i, err)
		}
	}
	if got := testutil.ToFloat64(counter) - before; got != 2 {
		t.Errorf("mismatch counter advanced by %v across two refused lookups, want 2", got)
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times while refused, want 0 — refuse before reading a price", n)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times within the retry window, want 1", n)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "decimals mismatch") {
		t.Errorf("no ERROR mismatch line logged; got:\n%s", logBuf.String())
	}

	// Past the retry interval the chain is re-read; still 18 → still refused.
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); !errors.Is(err, ErrChainlinkDecimalsMismatch) {
		t.Fatalf("after retry interval: err = %v, want ErrChainlinkDecimalsMismatch", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}

	// Chain and config agree again → readings resume at the agreed scale.
	f.decimalsValue.Store(8)
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	got, err := ref.LookupPrice(context.Background(), pair, ref.now())
	if err != nil {
		t.Fatalf("after agreement: %v", err)
	}
	if want := 15.0; abs(got-want) > 1e-9 {
		t.Errorf("price after agreement = %.12g, want %.12g", got, want)
	}
}

// TestChainlink_Decimals_MismatchDoesNotBleedAcrossPairsSharingAddress
// — two canonical pairs mapped to the SAME feed address, one whose
// configured decimals disagrees with the chain and one that agrees
// (asserts none): the verification state is keyed by address only, so
// it must never let the first pair's refusal leak into the second
// pair's verdict on a cache-hit call within the same retry window.
func TestChainlink_Decimals_MismatchDoesNotBleedAcrossPairsSharingAddress(t *testing.T) {
	t.Parallel()
	const address = "0x00000000000000000000000000000000000000e7"
	answer := big.NewInt(1_500_000_000)
	f := newChainlinkFakeRPC(t, roundDataHex(answer, time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)

	// Distinct codes from every other decimals test in this file — the
	// mismatch counter is a global Prometheus metric keyed by pair
	// string, so reusing a pair here would race with another test's
	// before/after counter snapshot.
	mismatchPair := mustPair(t, "crypto:SOL", "fiat:USD")
	okPair := mustPair(t, "crypto:AVAX", "fiat:USD")

	var logBuf bytes.Buffer
	clock := &fakeClock{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	ref := NewChainlinkReference(ChainlinkOptions{
		RPCURL: f.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
		FeedMap: map[string]ChainlinkFeed{
			// Configured 6 vs on-chain 8: refused.
			mismatchPair.String(): {Address: address, Decimals: 6, MaxAge: 76 * time.Hour},
			// No configured value: adopts on-chain 8, must NOT be
			// refused just because it shares an address with the
			// mismatching pair above.
			okPair.String(): {Address: address, Decimals: 0, MaxAge: 76 * time.Hour},
		},
	})
	ref.now = clock.Now

	// First call establishes the shared per-address state as a
	// mismatch (and caches it for chainlinkDecimalsRetryInterval).
	if _, err := ref.LookupPrice(context.Background(), mismatchPair, ref.now()); !errors.Is(err, ErrChainlinkDecimalsMismatch) {
		t.Fatalf("mismatchPair: err = %v, want ErrChainlinkDecimalsMismatch", err)
	}

	// Second call, different pair, same address, well within the
	// retry window (cache hit path in resolveDecimals): must adopt
	// the on-chain 8 and succeed, not inherit the other pair's
	// mismatch verdict.
	got, err := ref.LookupPrice(context.Background(), okPair, ref.now())
	if err != nil {
		t.Fatalf("okPair: LookupPrice = %v, want success — its own config (none asserted) agrees with on-chain 8, "+
			"a sibling pair's mismatch on the same feed address must not bleed into this verdict", err)
	}
	if want := 15.0; abs(got-want) > 1e-9 {
		t.Errorf("okPair: price = %.12g, want %.12g (scaled by on-chain 8)", got, want)
	}
}

// TestChainlink_Decimals_RPCErrorKeepsConfiguredWithRetry — decimals()
// fails (500) but latestRoundData() works: the configured value is kept
// with a WARN, the fail-open counter advances so the failure is
// alertable, the read is retried only after the retry interval, and
// nothing crashes.
func TestChainlink_Decimals_RPCErrorKeepsConfiguredWithRetry(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "fiat:JPY", "fiat:USD")
	failCounter := obs.ChainlinkFeedDecimalsVerifyFailedTotal.WithLabelValues("divergence", pair.String())
	before := testutil.ToFloat64(failCounter)
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(670_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusInternalServerError)
	ref, clock, logBuf := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e4", 8)

	got, err := ref.LookupPrice(context.Background(), pair, ref.now())
	if err != nil {
		t.Fatalf("LookupPrice with failing decimals(): %v — configured value must be kept", err)
	}
	if want := 0.0067; abs(got-want) > 1e-12 {
		t.Errorf("price = %.12g, want %.12g at the configured 8", got, want)
	}
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "decimals() read failed") {
		t.Errorf("no WARN line for the failed decimals() read; got:\n%s", logBuf.String())
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v after one failed decimals() read, want 1", got)
	}

	// Within the retry window: no re-read, counter unmoved.
	if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); err != nil {
		t.Fatalf("second LookupPrice: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Errorf("decimals() called %d times inside the retry window, want 1", n)
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v inside the retry window, want 1 (no re-read)", got)
	}
	// Past it: retried, and once the chain answers it is verified equal.
	clock.Advance(chainlinkDecimalsRetryInterval + time.Second)
	f.decimalsStatus.Store(http.StatusOK)
	if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); err != nil {
		t.Fatalf("LookupPrice after retry: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the retry interval, want 2", n)
	}
	if got := testutil.ToFloat64(failCounter) - before; got != 1 {
		t.Errorf("fail-open counter advanced by %v once the retry succeeded, want 1 (no further failure)", got)
	}
}

// TestChainlink_Decimals_RPCErrorWithoutConfigRefuses — no configured
// value AND decimals() unavailable: there is no scale to serve at, so
// the feed is refused (never guessed), as ErrPriceUnavailable.
func TestChainlink_Decimals_RPCErrorWithoutConfigRefuses(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:ETH", "fiat:USD")
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(250_000_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusInternalServerError)
	ref, _, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e5", 0)

	_, err := ref.LookupPrice(context.Background(), pair, ref.now())
	if !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("err = %v, want ErrPriceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "decimals unresolved") {
		t.Errorf("error %q should say the decimals are unresolved", err)
	}
	if n := f.roundCalls.Load(); n != 0 {
		t.Errorf("latestRoundData() called %d times with no known scale, want 0", n)
	}
}

// TestChainlink_Decimals_RefreshedDaily — a verified value is re-read
// after the refresh interval, so a proxy re-pointed at a differently
// scaled aggregator is caught within a day.
func TestChainlink_Decimals_RefreshedDaily(t *testing.T) {
	t.Parallel()
	pair := mustPair(t, "crypto:BTC", "fiat:USD")
	f := newChainlinkFakeRPC(t, roundDataHex(big.NewInt(6_543_210_000_000), time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)), 8, http.StatusOK)
	ref, clock, _ := newDecimalsTestRef(t, f, pair.String(), "0x00000000000000000000000000000000000000e6", 8)

	for i := 0; i < 3; i++ {
		if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); err != nil {
			t.Fatalf("LookupPrice %d: %v", i, err)
		}
	}
	if n := f.decimalsCalls.Load(); n != 1 {
		t.Fatalf("decimals() called %d times inside the refresh window, want 1", n)
	}
	// Staleness is measured against observedAt, which the clock also
	// drives: 25h is inside the feed's 76h MaxAge, so the round still
	// reads fresh after the jump.
	clock.Advance(chainlinkDecimalsRefreshInterval + time.Second)
	if _, err := ref.LookupPrice(context.Background(), pair, ref.now()); err != nil {
		t.Fatalf("LookupPrice after refresh interval: %v", err)
	}
	if n := f.decimalsCalls.Load(); n != 2 {
		t.Errorf("decimals() called %d times after the refresh interval, want 2", n)
	}
}

// TestDecodeChainlinkDecimals pins the uint8 word decode and its
// fail-loud shape checks.
func TestDecodeChainlinkDecimals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr string
	}{
		{"eight", bigInt256Hex(big.NewInt(8)), 8, ""},
		{"eighteen", bigInt256Hex(big.NewInt(18)), 18, ""},
		{"max uint8", bigInt256Hex(big.NewInt(255)), 255, ""},
		{"zero is a broken contract", bigInt256Hex(big.NewInt(0)), 0, "outside uint8 range"},
		{"256 overflows uint8", bigInt256Hex(big.NewInt(256)), 0, "outside uint8 range"},
		{"round-data shape is not a word", roundDataHex(big.NewInt(1), time.Unix(1, 0)), 0, "want 32 bytes"},
		{"empty", "0x", 0, "want 32 bytes"},
		{"not hex", "0xzz", 0, "decimals hex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeChainlinkDecimals(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
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
