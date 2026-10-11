package chainlink

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// decodeLatestRoundData parses the 5-tuple returned by
// AggregatorV3.latestRoundData() / .getRoundData(roundId).
//
//	(uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredInRound)
//
// Every element occupies a 32-byte ABI word (even uint80), so the response is exactly 160 bytes
// (320 hex chars plus 0x). answeredInRound is decoded so the poller can refuse a carried-forward
// round (answeredInRound < roundId).

// maxPlausibleUpdatedAtUnix caps the feed-reported updatedAt (Unix seconds) before any clock
// comparison. A value beyond the int64/timestamptz range would wrap NEGATIVE in the int64() cast
// or overflow the INSERT; bounding the raw u64 first keeps every later cast in range.
const maxPlausibleUpdatedAtUnix = 10_000_000_000 // ~year 2286

// maxUpdatedAtFutureSkew is how far past the poller's own clock a feed-reported updatedAt may sit.
// The poller clock is the only present-time anchor here. A future-dated row is refused, not
// clamped: it would win every "latest" read for the pair and drive the staleness gauge negative.
// Ethereum block timestamps never lead real time, so this only absorbs host clock skew.
const maxUpdatedAtFutureSkew = 5 * time.Minute

// checkUpdatedAt validates a raw feed-reported updatedAt against the
// absolute ceiling and against now+maxUpdatedAtFutureSkew.
func checkUpdatedAt(updatedAt uint64, now time.Time) error {
	if updatedAt == 0 || updatedAt > maxPlausibleUpdatedAtUnix {
		return fmt.Errorf("%w: out-of-range updatedAt %d", ErrMalformedResult, updatedAt)
	}
	limit := now.Add(maxUpdatedAtFutureSkew).Unix()
	if limit < 0 || updatedAt > uint64(limit) {
		return fmt.Errorf("%w: updatedAt %d is past poller clock %d + %s",
			ErrFutureUpdatedAt, updatedAt, now.Unix(), maxUpdatedAtFutureSkew)
	}
	return nil
}

func decodeLatestRoundData(rawHex, feedAddress string, now time.Time) (Round, error) {
	bytes, err := hexBytes(rawHex)
	if err != nil {
		return Round{}, fmt.Errorf("%w: latestRoundData hex: %w", ErrMalformedResult, err)
	}
	if len(bytes) != 5*32 {
		return Round{}, fmt.Errorf("%w: latestRoundData expected 160 bytes, got %d", ErrMalformedResult, len(bytes))
	}

	// Read the FULL 10-byte uint80 roundId (22..32 of word 0). The proxy roundId is
	// (phaseId<<64)|aggregatorRoundId and the low 64 bits reset on a phase upgrade, so dedup on them
	// would see round=1 <= prev and silently stop emitting; the full value stays monotonic. The
	// storage PK is idempotent, so a re-emit at a phase boundary is safe.
	roundID := decodeRoundID(bytes[22:32])

	// answer — int256, two's complement, in word 1.
	answer := decodeInt256(bytes[32:64])
	if answer.Sign() <= 0 {
		return Round{}, fmt.Errorf("%w: feed=%s round=%d answer=%s", ErrNonPositivePrice, feedAddress, roundID, answer.String())
	}

	// startedAt — uint256 in word 2 (we ignore it; updatedAt is the
	// authoritative timestamp).
	// updatedAt — uint256 in word 3.
	updatedAt, err := readUpdatedAt(bytes[96:128], now)
	if err != nil {
		return Round{}, fmt.Errorf("feed=%s round=%d: %w", feedAddress, roundID, err)
	}
	// answeredInRound — uint80 in word 4, phase-encoded like roundId.
	answeredInRound := decodeRoundID(bytes[150:160])

	return Round{
		FeedAddress:     strings.ToLower(feedAddress),
		RoundID:         roundID,
		AnsweredInRound: answeredInRound,
		Answer:          answer.String(),
		UpdatedAt:       time.Unix(int64(updatedAt), 0).UTC(),
	}, nil
}

// decodeAnswerUpdatedLog parses one eth_getLogs entry from AggregatorV3's AnswerUpdated event:
//
//	event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt)
//
// Topics are [hash, current, roundId] and data is [updatedAt]; one log yields one Round.
func decodeAnswerUpdatedLog(entry LogEntry, now time.Time) (Round, error) {
	if len(entry.Topics) < 3 {
		return Round{}, fmt.Errorf("%w: AnswerUpdated expected ≥3 topics, got %d", ErrMalformedResult, len(entry.Topics))
	}
	currentBytes, err := hexBytes(entry.Topics[1])
	if err != nil {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.current hex: %w", ErrMalformedResult, err)
	}
	if len(currentBytes) != 32 {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.current expected 32 bytes, got %d", ErrMalformedResult, len(currentBytes))
	}
	current := decodeInt256(currentBytes)
	if current.Sign() <= 0 {
		return Round{}, fmt.Errorf("%w: %s answer=%s", ErrNonPositivePrice, entry.Address, current.String())
	}

	roundIDBytes, err := hexBytes(entry.Topics[2])
	if err != nil {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.roundId hex: %w", ErrMalformedResult, err)
	}
	if len(roundIDBytes) != 32 {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.roundId expected 32 bytes, got %d", ErrMalformedResult, len(roundIDBytes))
	}
	// roundId is uint256 indexed. We read the low 10 bytes (uint80
	// width — the proxy roundId never exceeds that) into a wide id so
	// the dedup key is phase-aware, matching decodeLatestRoundData.
	roundID := decodeRoundID(roundIDBytes[22:32])

	dataBytes, err := hexBytes(entry.Data)
	if err != nil {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.data hex: %w", ErrMalformedResult, err)
	}
	if len(dataBytes) != 32 {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.data expected 32 bytes (one uint256), got %d", ErrMalformedResult, len(dataBytes))
	}
	updatedAt, err := readUpdatedAt(dataBytes, now)
	if err != nil {
		return Round{}, fmt.Errorf("%s round=%d: %w", entry.Address, roundID, err)
	}

	return Round{
		FeedAddress: strings.ToLower(entry.Address),
		RoundID:     roundID,
		Answer:      current.String(),
		UpdatedAt:   time.Unix(int64(updatedAt), 0).UTC(),
	}, nil
}

// decodeInt256 parses a 32-byte big-endian two's-complement integer
// into a *big.Int. Same semantics as the inline answer decode in
// internal/divergence/chainlink.go's decodeChainlinkRoundData.
func decodeInt256(b []byte) *big.Int {
	v := new(big.Int).SetBytes(b)
	if len(b) == 32 && b[0]&0x80 != 0 {
		twoTo256 := new(big.Int).Lsh(big.NewInt(1), 256)
		v = new(big.Int).Sub(v, twoTo256)
	}
	return v
}

// hexBytes decodes a 0x-prefixed hex string to bytes. Tolerates an
// odd-length payload by left-padding with a zero nibble (defensive
// for some RPC providers that strip leading zeros from large
// uint256 returns).
func hexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

// parseHexUint parses a 0x-prefixed hex string to uint64. Used for
// eth_blockNumber's hex-encoded scalar return.
func parseHexUint(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, fmt.Errorf("empty hex")
	}
	v := new(big.Int)
	if _, ok := v.SetString(s, 16); !ok {
		return 0, fmt.Errorf("not hex: %q", s)
	}
	if !v.IsUint64() {
		return 0, fmt.Errorf("overflows uint64: %s", v.String())
	}
	return v.Uint64(), nil // i128:ok IsUint64 range-checked above
}

// decodeRoundID reads a big-endian byte slice (the 10-byte uint80 proxy roundId, or any width up to
// a uint256) into a *big.Int. The FULL value is kept, not truncated to uint64, so the per-feed
// dedup key is phase-aware: only the wide (phaseId<<64)|aggregatorRoundId is monotonic across a
// phase rollover.
func decodeRoundID(b []byte) *big.Int {
	return new(big.Int).SetBytes(b)
}

// readUpdatedAt reads a 32-byte uint256 updatedAt word and applies
// checkUpdatedAt, rejecting any value with the high 24 bytes set (which
// the low 8 bytes alone would misread).
func readUpdatedAt(word []byte, now time.Time) (uint64, error) {
	for _, b := range word[:24] {
		if b != 0 {
			return 0, fmt.Errorf("%w: out-of-range updatedAt %s", ErrMalformedResult, new(big.Int).SetBytes(word))
		}
	}
	v := bigEndianUint64(word[24:32])
	return v, checkUpdatedAt(v, now)
}

// bigEndianUint64 reads 8 bytes as big-endian uint64. Tiny helper
// for the per-tuple-element extraction in decodeLatestRoundData /
// decodeAnswerUpdatedLog.
func bigEndianUint64(b []byte) uint64 {
	if len(b) != 8 {
		// Caller responsibility — the per-call slicing always
		// passes a length-8 slice. This guard turns an off-by-one
		// upstream into a panic at the boundary instead of a silent
		// wrong-value emit downstream.
		panic(fmt.Sprintf("chainlink: bigEndianUint64 expects 8 bytes, got %d", len(b)))
	}
	var v uint64
	for _, by := range b {
		v = (v << 8) | uint64(by)
	}
	return v
}
