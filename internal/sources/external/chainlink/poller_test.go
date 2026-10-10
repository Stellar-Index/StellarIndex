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

// TestAnswerUpdatedTopic0 pins topic0 to the literal keccak256 of
// "AnswerUpdated(int256,uint256,uint256)"; a wrong hash makes backfill match nothing, silently.
func TestAnswerUpdatedTopic0(t *testing.T) {
	t.Parallel()
	const want = "0x0559884fd3a460db3073b7fc896cc77986f16e378210ded43186175bf646fc5f"
	if AnswerUpdatedTopic0 != want {
		t.Errorf("AnswerUpdatedTopic0 = %s, want %s", AnswerUpdatedTopic0, want)
	}
}

// TestDecodeLatestRoundData_happy covers the 5-tuple ABI decode
// against a synthetic latestRoundData() return.
//
// Layout (5 × 32 bytes):
//
//	word 0: roundId   (uint80, padded)
//	word 1: answer    (int256)
//	word 2: startedAt (uint256)
//	word 3: updatedAt (uint256)
//	word 4: answeredInRound (uint80, padded)
func TestDecodeLatestRoundData_happy(t *testing.T) {
	t.Parallel()
	// Construct a known-good response: roundId=42, answer=12345e8 (~12345 USD at 8-dec),
	// startedAt=ignored, updatedAt=2026-01-01 00:00:00 UTC = 1767225600.
	answer := big.NewInt(123456789012345) // ~$1.23M at 8-dec
	updatedAt := uint64(1767225600)
	rawHex := buildLatestRoundDataReturn(t, 42, answer, 0, updatedAt, 42)

	rnd, err := decodeLatestRoundData(rawHex, "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c", decodeTestNow)
	if err != nil {
		t.Fatalf("decodeLatestRoundData: %v", err)
	}
	if rnd.RoundID == nil || rnd.RoundID.Cmp(big.NewInt(42)) != 0 {
		t.Errorf("RoundID = %v, want 42", rnd.RoundID)
	}
	if rnd.Answer != "123456789012345" {
		t.Errorf("Answer = %q, want 123456789012345 (no truncation per ADR-0003)", rnd.Answer)
	}
	if !rnd.UpdatedAt.Equal(time.Unix(int64(updatedAt), 0).UTC()) {
		t.Errorf("UpdatedAt = %v, want %v", rnd.UpdatedAt, time.Unix(int64(updatedAt), 0).UTC())
	}
	if rnd.FeedAddress != strings.ToLower("0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c") {
		t.Errorf("FeedAddress not lowercased: %q", rnd.FeedAddress)
	}
}

// TestDecodeLatestRoundData_outOfRangeUpdatedAt rejects an updatedAt
// past the plausibility ceiling — including values > math.MaxInt64 that
// would wrap NEGATIVE in the int64() cast and stamp a far-past time that
// overflows the oracle_updates timestamptz INSERT.
func TestDecodeLatestRoundData_outOfRangeUpdatedAt(t *testing.T) {
	t.Parallel()
	for name, updatedAt := range map[string]uint64{
		"justOverCeiling":  20_000_000_000,            // ~year 2603, > maxPlausibleUpdatedAtUnix
		"justOverMaxInt64": 9_223_372_036_854_775_808, // wraps negative in int64 cast
		"maxUint64":        18_446_744_073_709_551_615,
	} {
		t.Run(name, func(t *testing.T) {
			rawHex := buildLatestRoundDataReturn(t, 1, big.NewInt(100), 0, updatedAt, 1)
			_, err := decodeLatestRoundData(rawHex, "0xabc", decodeTestNow)
			if !errors.Is(err, ErrMalformedResult) {
				t.Errorf("err = %v, want ErrMalformedResult (out-of-range updatedAt must be rejected, not wrapped)", err)
			}
		})
	}
}

// TestDecodeLatestRoundData_negativeAnswer covers the
// non-positive-price guard. Some legacy Chainlink feeds did emit
// negative values during stress; we drop them rather than write a
// row that would fail the oracle_updates `CHECK (price > 0)`.
func TestDecodeLatestRoundData_negativeAnswer(t *testing.T) {
	t.Parallel()
	rawHex := buildLatestRoundDataReturn(t, 1, big.NewInt(-100), 0, 1767225600, 1)
	_, err := decodeLatestRoundData(rawHex, "0xabc", decodeTestNow)
	if !errors.Is(err, ErrNonPositivePrice) {
		t.Errorf("err = %v, want ErrNonPositivePrice", err)
	}
}

// TestDecodeLatestRoundData_zeroUpdatedAt covers the timestamp
// guard. updatedAt=0 means the feed was never written — emitting
// a row with epoch zero would falsely backdate to 1970 and break
// every CAGG.
func TestDecodeLatestRoundData_zeroUpdatedAt(t *testing.T) {
	t.Parallel()
	rawHex := buildLatestRoundDataReturn(t, 1, big.NewInt(100), 0, 0, 1)
	_, err := decodeLatestRoundData(rawHex, "0xabc", decodeTestNow)
	if !errors.Is(err, ErrMalformedResult) {
		t.Errorf("err = %v, want ErrMalformedResult", err)
	}
}

// TestDecodeLatestRoundData_wrongLength covers the malformed-input
// path — short response from a contract that doesn't implement
// AggregatorV3Interface.
func TestDecodeLatestRoundData_wrongLength(t *testing.T) {
	t.Parallel()
	_, err := decodeLatestRoundData("0xdeadbeef", "0xabc", decodeTestNow)
	if !errors.Is(err, ErrMalformedResult) {
		t.Errorf("err = %v, want ErrMalformedResult", err)
	}
}

// TestRoundCache_dedup confirms shouldEmit is idempotent across
// repeated polls of the same round and emits cleanly when a newer
// round arrives.
func TestRoundCache_dedup(t *testing.T) {
	t.Parallel()
	c := newRoundCache()
	rid := func(n int64) *big.Int { return big.NewInt(n) }
	if !c.shouldEmit("0xabc", rid(100)) {
		t.Errorf("first emit should pass")
	}
	if c.shouldEmit("0xabc", rid(100)) {
		t.Errorf("repeated round 100 should be deduped")
	}
	if c.shouldEmit("0xabc", rid(99)) {
		t.Errorf("older round 99 should be deduped (only strictly greater advances)")
	}
	if !c.shouldEmit("0xabc", rid(101)) {
		t.Errorf("newer round 101 should pass")
	}
	// Different feed: independent state.
	if !c.shouldEmit("0xdef", rid(100)) {
		t.Errorf("different feed first emit should pass")
	}
}

// TestRoundCache_phaseRollover_resumesEmission is the phase-rollover
// regression: a Chainlink proxy phase upgrade resets the aggregator-
// local roundId to ~1 while the phaseId increments. With a
// low-64-bit dedup key the post-upgrade round=1 reads as <= prev=<big>
// and the feed silently stops emitting until restart. Keying on the
// FULL uint80 (phaseId<<64|aggRound) keeps the wide id monotonic, so
// emission resumes.
func TestRoundCache_phaseRollover_resumesEmission(t *testing.T) {
	t.Parallel()
	c := newRoundCache()

	// Phase 1, aggregator round 5000 → wide id = (1<<64)+5000.
	phase1 := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(5000))
	if !c.shouldEmit("0xfeed", phase1) {
		t.Fatal("phase-1 round should emit")
	}

	// Proxy phase upgrade: aggregator round resets to 1, phaseId → 2.
	// Low 64 bits (1) are FAR below the prior low 64 bits (5000) — the
	// old code would have deduped this and wedged the feed.
	phase2 := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(2), 64), big.NewInt(1))
	if !c.shouldEmit("0xfeed", phase2) {
		t.Fatal("post-phase-bump round=1 should STILL emit — wide id increased (F-1323/G10-01)")
	}

	// And a normal advance within phase 2 keeps working.
	phase2next := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(2), 64), big.NewInt(2))
	if !c.shouldEmit("0xfeed", phase2next) {
		t.Fatal("phase-2 round=2 should emit")
	}
	// Re-poll of the same wide id is still deduped.
	if c.shouldEmit("0xfeed", phase2next) {
		t.Fatal("repeated wide id should dedupe")
	}
}

// TestDecodeLatestRoundData_phaseBits confirms the decoder reads the
// upper uint80 phase bits, not just the low 64. The wide roundId
// must equal (phaseID<<64)|aggRound.
func TestDecodeLatestRoundData_phaseBits(t *testing.T) {
	t.Parallel()
	raw := buildProxyRoundDataReturn(t, 2, 1, big.NewInt(2_500_00000000), 1767225600)
	rnd, err := decodeLatestRoundData(raw, "0xabc", decodeTestNow)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(2), 64), big.NewInt(1))
	if rnd.RoundID.Cmp(want) != 0 {
		t.Errorf("RoundID = %v, want %v (phase bits must survive decode)", rnd.RoundID, want)
	}
}

// TestPollOnce_allFeedsFailed_returnsError is the G10-02 liveness
// regression: when every feed errors and zero updates result, PollOnce
// must surface the error rather than returning (nil,nil,nil). The old
// "skip" path made the runner bump LastSuccessUnix and the staleness
// gauge stayed green while the poller was wedged.
func TestPollOnce_allFeedsFailed_returnsError(t *testing.T) {
	t.Parallel()
	// Server that always 500s → every eth_call fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "ETH"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	p := NewPoller(srv.URL, map[string]FeedSpec{
		pair.String(): {Address: "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419"},
	})

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err == nil {
		t.Fatal("all-feeds-failed cycle returned nil error — would mark poller healthy (G10-02)")
	}
	if len(updates) != 0 {
		t.Errorf("len(updates) = %d, want 0", len(updates))
	}
}

// TestPollOnce_emitsOraceUpdate runs PollOnce against an
// httptest.Server faking the Alchemy JSON-RPC. Verifies the full
// path: HTTP request, ABI decode, dedup, project to OracleUpdate.
func TestPollOnce_emitsOracleUpdate(t *testing.T) {
	t.Parallel()
	const feedAddr = "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419" // ETH/USD
	answer := big.NewInt(2_500_00000000)                          // $2,500 at 8-dec
	updatedAt := uint64(1767225600)
	respBody := buildLatestRoundDataReturn(t, 100, answer, 0, updatedAt, 100)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method != "eth_call" {
			t.Errorf("expected eth_call, got %s", req.Method)
		}
		// Dispatch on the selector: the poller verifies decimals() (8,
		// Chainlink's standard, matching the spec below) before it reads
		// latestRoundData().
		var call struct {
			Data string `json:"data"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &call)
		}
		if call.Data == SelDecimals {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, decimalsReturn(8))
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, respBody)
	}))
	defer srv.Close()

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "ETH"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	p := NewPoller(srv.URL, map[string]FeedSpec{
		pair.String(): {Address: feedAddr, Decimals: 8},
	})
	p.now = func() time.Time { return time.Unix(int64(updatedAt), 0).Add(time.Minute) }

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1", len(updates))
	}
	u := updates[0]
	if u.Source != SourceName {
		t.Errorf("Source = %q, want %q", u.Source, SourceName)
	}
	if u.Price.String() != "250000000000" {
		t.Errorf("Price = %q, want 250000000000", u.Price.String())
	}
	if u.Decimals != 8 {
		t.Errorf("Decimals = %d, want 8", u.Decimals)
	}
	if u.Asset.Code != "ETH" || u.Quote.Code != "USD" {
		t.Errorf("pair = %s/%s, want ETH/USD", u.Asset.Code, u.Quote.Code)
	}
	if !u.Timestamp.Equal(time.Unix(int64(updatedAt), 0).UTC()) {
		t.Errorf("Timestamp = %v, want %v", u.Timestamp, time.Unix(int64(updatedAt), 0).UTC())
	}
	if len(u.TxHash) != 64 {
		t.Errorf("TxHash length = %d, want 64 (hex sha256)", len(u.TxHash))
	}
	if err := u.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Second PollOnce should dedupe (same roundId) → no updates.
	_, updates2, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}
	if len(updates2) != 0 {
		t.Errorf("repeated poll emitted %d updates, want 0 (dedup)", len(updates2))
	}
}

// TestPollOnce_projectFailureDoesNotStrandRound is the RNC26
// regression: a project() failure occurring AFTER decode/resolveDecimals
// have already succeeded (here, an Invert quotient that floors to zero
// because the raw answer exceeds 10^(2*decimals)) must not mark the
// round emitted. If it does, a later, otherwise-healthy poll of the
// SAME round (on-chain answer corrected, roundId unchanged) silently
// produces zero updates forever — the feed is wedged until process
// restart even though nothing is actually wrong with it anymore.
func TestPollOnce_projectFailureDoesNotStrandRound(t *testing.T) {
	t.Parallel()
	const feedAddr = "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419"
	updatedAt := uint64(1767225600)

	// First answer is decode-valid (positive) but, once inverted at
	// 8-dec scale (10^16 / answer), floors to zero because answer
	// exceeds 10^16 — project()'s post-invert non-positive guard
	// refuses it. The SAME roundId later reports a healthy answer
	// that inverts to a positive price.
	answer := big.NewInt(200_000_000_000_000_000) // 2e17 > 1e16 → quotient floors to 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var call struct {
			Data string `json:"data"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &call)
		}
		if call.Data == SelDecimals {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, decimalsReturn(8))
			return
		}
		respBody := buildLatestRoundDataReturn(t, 100, answer, 0, updatedAt, 100)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"%s"}`, respBody)
	}))
	defer srv.Close()

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "EUR"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	p := NewPoller(srv.URL, map[string]FeedSpec{
		pair.String(): {Address: feedAddr, Decimals: 8, Invert: true},
	})
	p.now = func() time.Time { return time.Unix(int64(updatedAt), 0).Add(time.Minute) }

	// First poll: round 100 inverts to zero → project() refuses it.
	// Zero updates, an error surfaced.
	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err == nil {
		t.Fatal("PollOnce with an inverts-to-zero answer returned nil error, want project failure surfaced")
	}
	if len(updates) != 0 {
		t.Fatalf("len(updates) = %d, want 0 on the failing poll", len(updates))
	}

	// Feed corrects itself: SAME roundId (100), an answer that
	// inverts to a positive price.
	answer = big.NewInt(20_000_000) // 1e16 / 2e7 = 5e8, positive
	_, updates, err = p.PollOnce(context.Background(), []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("PollOnce after correction: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1 — round 100 must still be emittable after the earlier project() failure (RNC26)", len(updates))
	}
}

// TestProject_invert covers the Invert flag path — a feed that
// publishes EUR/USD reused as USD/EUR.
func TestProject_invert(t *testing.T) {
	t.Parallel()
	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"},
	}
	p := NewPoller("", nil)
	spec := FeedSpec{Address: "0xb49f677943BC038e9857d61E7d053CaA2C1734C1", Decimals: 8, Invert: true}
	// Raw feed answer: 1.10 (EUR/USD), 8-dec → 110000000.
	rnd := Round{
		FeedAddress: spec.Address,
		RoundID:     big.NewInt(1),
		Answer:      "110000000",
		UpdatedAt:   time.Unix(1767225600, 0).UTC(),
	}
	u, err := p.project(pair, spec, rnd)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Inverted: 10^16 / 110000000 = 90909090.909… rounds half-up to
	// 90909091, matching scale.InvertScaled; truncation would give …090.
	got := u.Price.String()
	if got != "90909091" {
		t.Errorf("inverted price = %q, want 90909091", got)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

// buildLatestRoundDataReturn assembles a 160-byte ABI-encoded
// 5-tuple as a 0x-prefixed hex string. Mirrors the byte layout
// the chainlink AggregatorV3 contract returns from
// latestRoundData() / getRoundData().
func buildLatestRoundDataReturn(t *testing.T, roundID uint64, answer *big.Int, startedAt, updatedAt uint64, answeredInRound uint64) string {
	t.Helper()
	var buf [160]byte
	// word 0: roundId (uint80 left-padded to 32 bytes)
	putUint64BE(buf[24:32], roundID)
	// word 1: answer (int256, two's complement big-endian)
	encodeInt256(buf[32:64], answer)
	// word 2: startedAt (uint256)
	putUint64BE(buf[88:96], startedAt)
	// word 3: updatedAt (uint256)
	putUint64BE(buf[120:128], updatedAt)
	// word 4: answeredInRound (uint80 padded)
	putUint64BE(buf[152:160], answeredInRound)
	return "0x" + hex.EncodeToString(buf[:])
}

// buildProxyRoundDataReturn assembles the same 5-tuple but encodes a
// FULL uint80 proxy roundId = (phaseID<<64)|aggRound across bytes
// 22..32 of word 0. Used by the phase-rollover test
// to reproduce the case where aggRound resets to ~1 but phaseID
// increments — the wide id still strictly increases.
func buildProxyRoundDataReturn(t *testing.T, phaseID uint16, aggRound uint64, answer *big.Int, updatedAt uint64) string {
	t.Helper()
	var buf [160]byte
	// word 0: roundId (uint80) — phaseID in bytes 22..24, aggRound in
	// bytes 24..32. Together that's the 10-byte big-endian uint80.
	buf[22] = byte(phaseID >> 8)
	buf[23] = byte(phaseID)
	putUint64BE(buf[24:32], aggRound)
	encodeInt256(buf[32:64], answer)     // word 1: answer
	putUint64BE(buf[120:128], updatedAt) // word 3: updatedAt
	return "0x" + hex.EncodeToString(buf[:])
}

func putUint64BE(dst []byte, v uint64) {
	if len(dst) != 8 {
		panic("putUint64BE expects 8 bytes")
	}
	for i := 7; i >= 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
}

func encodeInt256(dst []byte, v *big.Int) {
	if len(dst) != 32 {
		panic("encodeInt256 expects 32 bytes")
	}
	if v.Sign() >= 0 {
		bs := v.Bytes()
		copy(dst[32-len(bs):], bs)
		return
	}
	// Negative: two's complement = 2^256 + v.
	twoTo256 := new(big.Int).Lsh(big.NewInt(1), 256)
	enc := new(big.Int).Add(twoTo256, v).Bytes()
	// Pad up to 32 bytes (defensive — for very negative values
	// enc.Bytes() is exactly 32 bytes already).
	if len(enc) < 32 {
		copy(dst[32-len(enc):], enc)
	} else {
		copy(dst, enc[len(enc)-32:])
	}
}

// publishedPriceOf reads published_price off the row's JSON form (runtime,
// not compile-time, failure on a build that never records it).
func publishedPriceOf(t *testing.T, u canonical.OracleUpdate) (string, bool) {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := m["published_price"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("published_price %s is not a decimal string: %v", raw, err)
	}
	return s, true
}

// TestProject_invertRecordsPublishedAnswer pins published_price for chainlink:
// an Invert feed keeps the reciprocal as price and the feed's own answer
// verbatim as published_price; a non-Invert feed records none.
func TestProject_invertRecordsPublishedAnswer(t *testing.T) {
	t.Parallel()
	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"},
	}
	p := NewPoller("", nil)
	for _, tc := range []struct {
		invert        bool
		answer        string
		wantPrice     string
		wantPublished string
	}{
		{invert: true, answer: "110000000", wantPrice: "90909091", wantPublished: "110000000"},
		{invert: true, answer: "110000001", wantPrice: "90909090", wantPublished: "110000001"},
		{invert: false, answer: "110000000", wantPrice: "110000000"},
	} {
		spec := FeedSpec{Address: "0xb49f677943BC038e9857d61E7d053CaA2C1734C1", Decimals: 8, Invert: tc.invert}
		u, err := p.project(pair, spec, Round{
			FeedAddress: spec.Address,
			RoundID:     big.NewInt(1),
			Answer:      tc.answer,
			UpdatedAt:   time.Unix(1767225600, 0).UTC(),
		})
		if err != nil {
			t.Fatalf("project(%s): %v", tc.answer, err)
		}
		if got := u.Price.String(); got != tc.wantPrice {
			t.Errorf("invert=%v answer=%s: price = %s, want %s", tc.invert, tc.answer, got, tc.wantPrice)
		}
		got, ok := publishedPriceOf(t, u)
		if tc.wantPublished == "" {
			if ok {
				t.Errorf("non-Invert feed recorded published_price %q; want absent", got)
			}
			continue
		}
		if got != tc.wantPublished {
			t.Errorf("invert answer=%s: published_price = %q (present=%v), want %s", tc.answer, got, ok, tc.wantPublished)
		}
	}
}

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
	// Just after the fixtures' updatedAt (1767225600), inside every MaxAge.
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)}
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
// a config-trusting poller would stamp DefaultDecimals (8) on the row, so every
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

// NS10 — the per-feed fan-out's panic guard must RELEASE what the feed
// holds, not merely contain the panic.
//
// PollOnce acquires the concurrency semaphore in the CALLER's frame
// (`sem <- struct{}{}` before the `go`) and a WaitGroup slot with it, so a
// recover that swallows the panic without running the `<-sem` and wg.Done
// defers first converts a loud whole-process crash into a silent permanent
// hang: the loop blocks forever handing out the next slot, the join
// goroutine never closes `results`, and the runner's tick never returns.
// That is strictly worse than the crash it replaces, and it is invisible to
// a single-feed test — with one feed the slot is never re-acquired and the
// WaitGroup is never waited on under contention.
//
// So: more feeds than slots, and every one of them panics. The panic is the
// real one a struct-literal Poller produces (resolveDecimals dereferences
// p.decimals, which only NewPoller populates).
func TestPollOnce_PanickingFeedReleasesItsSlotAndWaiter(t *testing.T) {
	const (
		workerName = "external-chainlink-feed-poll"
		feeds      = 4
	)
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	pairs := make([]canonical.Pair, 0, feeds)
	feedMap := make(map[string]FeedSpec, feeds)
	for i := 0; i < feeds; i++ {
		pr := testPair(fmt.Sprintf("TK%d", i), "USD")
		pairs = append(pairs, pr)
		feedMap[pr.String()] = FeedSpec{Address: fmt.Sprintf("0xfeed%d", i), Decimals: 8}
	}

	p := &Poller{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		FeedMap: feedMap,
		Cache:   newRoundCache(),
		// One slot, four feeds: feed 2 cannot start until feed 1's
		// guard has given the slot back.
		Concurrency: 1,
		// decimals deliberately left nil — the mis-construction whose
		// nil dereference stands in for any panic inside a feed read.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		trades  []canonical.Trade
		updates []canonical.OracleUpdate
		err     error
	)
	go func() {
		trades, updates, err = p.PollOnce(ctx, pairs)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PollOnce never returned with a panicking feed and fewer slots than feeds: " +
			"the guard contained the panic but did not release the concurrency slot / " +
			"WaitGroup slot, so the fan-out wedges the poll tick forever")
	}

	if err == nil {
		t.Fatalf("a tick in which every feed panicked returned err=nil (trades=%v updates=%v): "+
			"the runner reads that as a healthy skip", trades, updates)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("PollOnce err = %v, want the per-feed panic surfaced as that feed's failure", err)
	}
	if len(updates) != 0 || len(trades) != 0 {
		t.Errorf("PollOnce returned %d update(s) / %d trade(s) from panicking feeds, want 0/0",
			len(updates), len(trades))
	}

	// Every feed must be accounted for. A guard that releases the slot but
	// drops the later feeds (or one that lets a feed vanish from `results`)
	// still leaves dead price feeds invisible.
	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+feeds {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — every one of "+
			"the %d panicking feeds must be reported, not just the first",
			workerName, after, before+feeds, feeds)
	}
}

// PollOnce fans out one detached goroutine per feed plus a join
// goroutine, and an unrecovered panic in ANY goroutine terminates the WHOLE
// process. Without the guard this test does not fail, it CRASHES the test
// binary; that crash is the production harm.
//
// The assertion is not merely "it survived". A panicking feed that is only
// CONTAINED disappears from `results` entirely, so the tick ends with zero
// updates and zero errors — which PollOnce's own G10-02 comment calls a
// healthy skip, bumping ExternalPollerLastSuccessUnix and leaving the
// staleness gauge green over a poller that is actually dead. So the panic
// must be reported as a per-feed FAILURE, and an all-feeds-panicking tick
// must surface an error.
//
// The panic is the real one a struct-literal Poller produces: resolveDecimals
// dereferences p.decimals, which only NewPoller populates.
func TestPollOnce_PanickingFeedIsReportedNotSilentlySkipped(t *testing.T) {
	const workerName = "external-chainlink-feed-poll"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	pair := testPair("BTC", "USD")
	p := &Poller{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		FeedMap:     map[string]FeedSpec{pair.String(): {Address: "0xfeed", Decimals: 8}},
		Cache:       newRoundCache(),
		Concurrency: 1,
		// decimals deliberately left nil — the mis-construction whose
		// nil dereference stands in for any panic inside a feed read.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		trades  []canonical.Trade
		updates []canonical.OracleUpdate
		err     error
	)
	go func() {
		trades, updates, err = p.PollOnce(ctx, []canonical.Pair{pair})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PollOnce never returned: the join goroutine contained its panic without " +
			"closing the results channel, so the fan-in ranges forever — close from a defer")
	}

	if err == nil {
		t.Fatalf("a tick in which every feed panicked returned err=nil (trades=%v updates=%v): "+
			"the runner reads that as a healthy skip and keeps the staleness gauge green "+
			"over a poller that reported nothing at all", trades, updates)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("PollOnce err = %v, want the per-feed panic surfaced as that feed's failure", err)
	}
	if len(updates) != 0 {
		t.Errorf("PollOnce returned %d update(s) from a panicking feed, want 0", len(updates))
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a recover "+
			"that does not move the counter turns a loud crash into a silent dead feed",
			workerName, after, before+1)
	}
}
