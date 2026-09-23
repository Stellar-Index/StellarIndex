package chainlink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// pollOnceAtClock runs one PollOnce for ETH/USD against a fake RPC whose
// latestRoundData reports updatedAt, with the poller clock pinned to now.
func pollOnceAtClock(t *testing.T, now time.Time, updatedAt uint64) ([]canonical.OracleUpdate, error) {
	t.Helper()
	respBody := buildLatestRoundDataReturn(t, 7, big.NewInt(2_500_00000000), 0, updatedAt, 7)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(SelDecimals)) {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, decimalsReturn(8))
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, respBody)
	}))
	t.Cleanup(srv.Close)

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "ETH"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	p := NewPoller(srv.URL, map[string]FeedSpec{
		pair.String(): {Address: "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419", Decimals: 8},
	})
	p.now = func() time.Time { return now }
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	return updates, err
}

// TestPollOnce_refusesFutureDatedUpdatedAt: a feed reporting an updatedAt
// an hour past the poller's clock must not produce a row. Such a row
// would win every "latest" read for the pair and hold the staleness gauge
// negative until the wall clock caught up with it.
func TestPollOnce_refusesFutureDatedUpdatedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	updates, err := pollOnceAtClock(t, now, uint64(now.Add(time.Hour).Unix()))
	if len(updates) != 0 {
		t.Fatalf("emitted %d update(s) dated %v with the poller clock at %v, want 0",
			len(updates), updates[0].Timestamp, now)
	}
	if err == nil {
		t.Error("PollOnce returned nil error for a refused-only cycle; the refusal must be visible")
	}
}

// TestPollOnce_acceptsUpdatedAtWithinSkew keeps the guard from being
// over-tight: a round a minute ahead of the poller clock (host skew) and
// one in the past both still emit.
func TestPollOnce_acceptsUpdatedAtWithinSkew(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	for name, at := range map[string]time.Time{
		"oneMinuteAhead": now.Add(time.Minute),
		"oneMinuteAgo":   now.Add(-time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			updates, err := pollOnceAtClock(t, now, uint64(at.Unix()))
			if err != nil {
				t.Fatalf("PollOnce: %v", err)
			}
			if len(updates) != 1 || !updates[0].Timestamp.Equal(at) {
				t.Fatalf("updates = %+v, want one row at %v", updates, at)
			}
		})
	}
}
