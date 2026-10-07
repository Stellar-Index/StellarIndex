package chainlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// feedRound is what the fake RPC answers for one feed's latestRoundData.
type feedRound struct {
	roundID, answeredInRound uint64
	updatedAt                time.Time
}

// multiFeedRPC answers decimals() = 8 for every feed and
// latestRoundData() from rounds, keyed by lowercase feed address.
func multiFeedRPC(t *testing.T, rounds map[string]*feedRound, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &call)
		}
		if call.Data == SelDecimals {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, decimalsReturn(8))
			return
		}
		mu.Lock()
		fr, ok := rounds[strings.ToLower(call.To)]
		var hexBody string
		if ok {
			hexBody = buildLatestRoundDataReturn(t, fr.roundID, big.NewInt(2_500_00000000), 0, uint64(fr.updatedAt.Unix()), fr.answeredInRound)
		}
		mu.Unlock()
		if !ok {
			t.Errorf("unexpected eth_call to %q", call.To)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, hexBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func feedPolls(pair canonical.Pair, outcome string) float64 {
	return testutil.ToFloat64(obs.ChainlinkFeedPollsTotal.WithLabelValues(pair.String(), outcome))
}

// A feed whose latest round is older than its MaxAge must not be emitted
// as a new observation, and — with a healthy sibling in the same tick —
// must still be counted per feed, since the tick itself reads as success.
func TestPollOnce_staleFeedRefusedAndCountedBesideHealthySibling(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	const staleAddr, freshAddr = "0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000a2"
	stale := testPair("ADA", "USD")
	fresh := testPair("ATOM", "USD")

	var mu sync.Mutex
	srv := multiFeedRPC(t, map[string]*feedRound{
		staleAddr: {roundID: 10, answeredInRound: 10, updatedAt: now.Add(-4 * time.Hour)},
		freshAddr: {roundID: 20, answeredInRound: 20, updatedAt: now.Add(-time.Minute)},
	}, &mu)
	feeds, _, err := BuildFeedSet(map[string]FeedSpec{
		stale.String(): {Address: staleAddr},
		fresh.String(): {Address: freshAddr},
	})
	if err != nil {
		t.Fatalf("BuildFeedSet: %v", err)
	}
	p := NewPoller(srv.URL, feeds)
	p.now = func() time.Time { return now }

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{stale, fresh})
	if err != nil {
		t.Fatalf("PollOnce: %v (a healthy sibling keeps the tick successful)", err)
	}
	if len(updates) != 1 || updates[0].Asset.Code != "ATOM" {
		t.Fatalf("updates = %+v, want exactly the fresh feed's round — a 4h-old crypto round "+
			"(3h budget) must never be written as a current observation", updates)
	}
	if got := feedPolls(stale, outcomeStale); got != 1 {
		t.Errorf("polls_total{pair=%s,outcome=stale} = %v, want 1", stale, got)
	}
	if got := testutil.ToFloat64(obs.ChainlinkFeedLastSuccessUnix.WithLabelValues(stale.String())); got != 0 {
		t.Errorf("stale feed last_success = %v, want 0 (never fresh)", got)
	}
	if got := feedPolls(fresh, outcomeEmitted); got != 1 {
		t.Errorf("polls_total{pair=%s,outcome=emitted} = %v, want 1", fresh, got)
	}
	if got := testutil.ToFloat64(obs.ChainlinkFeedLastSuccessUnix.WithLabelValues(fresh.String())); got != float64(now.Unix()) {
		t.Errorf("fresh feed last_success = %v, want %d", got, now.Unix())
	}
}

// A feed that freezes after its last round was emitted must read as
// stale, not as "unchanged": the age gate runs before round dedup.
func TestPollOnce_frozenFeedTurnsStaleNotUnchanged(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	now := start
	const addr = "0x00000000000000000000000000000000000000b1"
	pair := testPair("AVAX", "USD")

	var mu sync.Mutex
	srv := multiFeedRPC(t, map[string]*feedRound{
		addr: {roundID: 5, answeredInRound: 5, updatedAt: start.Add(-time.Minute)},
	}, &mu)
	p := NewPoller(srv.URL, map[string]FeedSpec{pair.String(): {Address: addr, MaxAge: time.Hour}})
	p.now = func() time.Time { return now }

	if _, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair}); err != nil || len(updates) != 1 {
		t.Fatalf("first poll: updates=%d err=%v, want the fresh round emitted", len(updates), err)
	}
	now = start.Add(30 * time.Minute)
	if _, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair}); err != nil || len(updates) != 0 {
		t.Fatalf("in-budget repeat: updates=%d err=%v, want an unchanged no-op", len(updates), err)
	}
	now = start.Add(2 * time.Hour)
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if !errors.Is(err, ErrStaleRound) || len(updates) != 0 {
		t.Fatalf("over-budget repeat: updates=%d err=%v, want ErrStaleRound", len(updates), err)
	}
	for outcome, want := range map[string]float64{outcomeEmitted: 1, outcomeUnchanged: 1, outcomeStale: 1} {
		if got := feedPolls(pair, outcome); got != want {
			t.Errorf("polls_total{outcome=%s} = %v, want %v", outcome, got, want)
		}
	}
	if got := testutil.ToFloat64(obs.ChainlinkFeedLastSuccessUnix.WithLabelValues(pair.String())); got != float64(start.Add(30*time.Minute).Unix()) {
		t.Errorf("last_success = %v, want the last in-budget poll %d", got, start.Add(30*time.Minute).Unix())
	}
}

// answeredInRound < roundId: the answer was computed in an earlier round,
// so projecting it would record a publication that did not happen.
func TestPollOnce_carriedForwardRoundRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	const addr = "0x00000000000000000000000000000000000000c1"
	pair := testPair("BCH", "USD")

	var mu sync.Mutex
	srv := multiFeedRPC(t, map[string]*feedRound{
		addr: {roundID: 8, answeredInRound: 7, updatedAt: now.Add(-time.Minute)},
	}, &mu)
	p := NewPoller(srv.URL, map[string]FeedSpec{pair.String(): {Address: addr, MaxAge: time.Hour}})
	p.now = func() time.Time { return now }

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if !errors.Is(err, ErrCarriedForwardRound) || len(updates) != 0 {
		t.Fatalf("updates=%d err=%v, want ErrCarriedForwardRound and no row", len(updates), err)
	}
	if got := feedPolls(pair, outcomeCarriedForward); got != 1 {
		t.Errorf("polls_total{outcome=carried_forward} = %v, want 1", got)
	}
}

// A stale sibling contributing zero updates must not flip a tick
// to "error" when another feed is CURRENT this tick — and "current"
// means emitted OR unchanged, not just emitted. Unchanged sends nothing
// to PollOnce's fan-in, so without tracking it separately the stale
// feed's error became firstErr even though the tick was healthy.
func TestPollOnce_staleFeedBesideUnchangedSiblingStaysGreen(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	now := start
	const freshAddr, staleAddr = "0x00000000000000000000000000000000000000d1", "0x00000000000000000000000000000000000000d2"
	fresh := testPair("SOL", "USD")
	stale := testPair("XRP", "USD")

	var mu sync.Mutex
	srv := multiFeedRPC(t, map[string]*feedRound{
		freshAddr: {roundID: 1, answeredInRound: 1, updatedAt: start.Add(-time.Minute)},
		staleAddr: {roundID: 1, answeredInRound: 1, updatedAt: start.Add(-264 * time.Hour)},
	}, &mu)
	p := NewPoller(srv.URL, map[string]FeedSpec{
		fresh.String(): {Address: freshAddr, MaxAge: 3 * time.Hour},
		stale.String(): {Address: staleAddr, MaxAge: 3 * time.Hour},
	})
	p.now = func() time.Time { return now }

	// Tick 1: fresh sibling emits; the stale feed already errors, but
	// the tick is green because of the fresh emit.
	if _, updates, err := p.PollOnce(context.Background(), []canonical.Pair{fresh, stale}); err != nil || len(updates) != 1 {
		t.Fatalf("tick1: updates=%d err=%v, want the fresh feed emitted", len(updates), err)
	}

	// Tick 2: the fresh feed's round hasn't advanced (unchanged, not
	// emitted) while the other feed is still stale. Zero updates, but
	// the fresh feed is still current — must not read as an error.
	now = start.Add(30 * time.Minute)
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{fresh, stale})
	if err != nil {
		t.Fatalf("tick2: err=%v, want nil — an unchanged sibling keeps the tick green", err)
	}
	if len(updates) != 0 {
		t.Fatalf("tick2: updates=%d, want 0", len(updates))
	}
	if got := feedPolls(fresh, outcomeUnchanged); got != 1 {
		t.Errorf("polls_total{pair=%s,outcome=unchanged} = %v, want 1", fresh, got)
	}
	if got := feedPolls(stale, outcomeStale); got != 2 {
		t.Errorf("polls_total{pair=%s,outcome=stale} = %v, want 2 (both ticks)", stale, got)
	}
}

// Counterpart: with NO current feed at all (every feed stale), the
// tick is a genuine failure and must still surface the error.
func TestPollOnce_allFeedsStaleReturnsError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	const addr1, addr2 = "0x00000000000000000000000000000000000000e1", "0x00000000000000000000000000000000000000e2"
	p1, p2 := testPair("LTC", "USD"), testPair("ETC", "USD")

	var mu sync.Mutex
	srv := multiFeedRPC(t, map[string]*feedRound{
		addr1: {roundID: 1, answeredInRound: 1, updatedAt: now.Add(-264 * time.Hour)},
		addr2: {roundID: 1, answeredInRound: 1, updatedAt: now.Add(-264 * time.Hour)},
	}, &mu)
	p := NewPoller(srv.URL, map[string]FeedSpec{
		p1.String(): {Address: addr1, MaxAge: 3 * time.Hour},
		p2.String(): {Address: addr2, MaxAge: 3 * time.Hour},
	})
	p.now = func() time.Time { return now }

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{p1, p2})
	if !errors.Is(err, ErrStaleRound) || len(updates) != 0 {
		t.Fatalf("updates=%d err=%v, want ErrStaleRound — no feed was current", len(updates), err)
	}
}

func TestDecodeLatestRoundData_answeredInRound(t *testing.T) {
	t.Parallel()
	now := time.Unix(1767225600, 0).Add(time.Minute)
	rnd, err := decodeLatestRoundData(buildLatestRoundDataReturn(t, 42, big.NewInt(1), 0, 1767225600, 41), "0xabc", now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rnd.AnsweredInRound == nil || rnd.AnsweredInRound.Int64() != 41 {
		t.Errorf("AnsweredInRound = %v, want 41", rnd.AnsweredInRound)
	}
}

// Per-feed budgets come from the feed's heartbeat class; an omitted one
// resolves per pair, never to one global constant.
func TestBuildFeedSet_resolvesMaxAgePerFeed(t *testing.T) {
	t.Parallel()
	feeds, _, err := BuildFeedSet(map[string]FeedSpec{
		"crypto:BTC/fiat:USD": {Address: "0x1"},
		"fiat:JPY/fiat:USD":   {Address: "0x2"},
		"fiat:CHF/fiat:USD":   {Address: "0x3"},
		"crypto:DOT/fiat:USD": {Address: "0x4"},
		"crypto:ETH/fiat:USD": {Address: "0x5", MaxAge: 90 * time.Minute},
	})
	if err != nil {
		t.Fatalf("BuildFeedSet: %v", err)
	}
	for pair, want := range map[string]time.Duration{
		"crypto:BTC/fiat:USD": DefaultMaxAgeCrypto,
		"fiat:JPY/fiat:USD":   DefaultMaxAgeFX,
		"fiat:CHF/fiat:USD":   DefaultMaxAgeFX,
		"crypto:DOT/fiat:USD": DefaultMaxAgeCrypto,
		"crypto:ETH/fiat:USD": 90 * time.Minute,
	} {
		if got := feeds[pair].MaxAge; got != want {
			t.Errorf("%s MaxAge = %s, want %s", pair, got, want)
		}
	}
	for pair, spec := range DefaultFeedMap() {
		if spec.MaxAge <= 0 {
			t.Errorf("built-in %s has no MaxAge", pair)
		}
	}
	if _, _, err := BuildFeedSet(map[string]FeedSpec{"crypto:BTC/fiat:USD": {Address: "0x1", MaxAge: -time.Hour}}); err == nil {
		t.Error("negative MaxAge accepted; it would mark every round stale")
	}
}
