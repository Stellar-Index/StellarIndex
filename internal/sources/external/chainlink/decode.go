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
// Per the Solidity ABI, every fixed-size element occupies a 32-byte
// word in the return data — even uint80 (Solidity left-pads to
// 32 bytes for the return slot). So the response is exactly 5 × 32
// = 160 bytes = 320 hex chars (plus the 0x prefix).
//
// We don't decode answeredInRound — Chainlink itself only uses it
// for liveness checks ("did the answer get carried forward across
// aggregators or was it freshly computed in this round"). For our
// purposes the round is uniquely identified by RoundID + the feed
// address; answeredInRound adds no information for ingestion.
// maxPlausibleUpdatedAtUnix caps the feed-reported updatedAt (Unix
// seconds). Real Chainlink rounds carry a recent block timestamp
// (~1.7e9); anything past ~year 2286 is garbage. Critically, a value
// that exceeds the int64 / timestamptz range would wrap NEGATIVE in the
// int64() cast (→ far-past time) or land beyond Postgres's timestamptz
// max and overflow the oracle_updates INSERT — the class guarded across
// the band / reflector / redstone decoders. Chainlink data comes from a
// trusted feed via trusted RPC, so this is defence-in-depth, but it
// reaches the same timestamptz column with no downstream clamp.
const maxPlausibleUpdatedAtUnix = 10_000_000_000 // ~year 2286

func decodeLatestRoundData(rawHex, feedAddress string) (Round, error) {
	bytes, err := hexBytes(rawHex)
	if err != nil {
		return Round{}, fmt.Errorf("%w: latestRoundData hex: %w", ErrMalformedResult, err)
	}
	if len(bytes) != 5*32 {
		return Round{}, fmt.Errorf("%w: latestRoundData expected 160 bytes, got %d", ErrMalformedResult, len(bytes))
	}

	// roundId — uint80 padded to 32 bytes. We read the FULL 10-byte
	// (80-bit) value from bytes 22..32 of word 0, not just the low
	// 64 bits.
	//
	// F-1323/G10-01: the proxy contract's roundId is
	// (phaseId<<64)|aggregatorRoundId. Reading only the low 64 bits
	// (bytes[24:32]) discards the phase. On a proxy phase upgrade the
	// aggregatorRoundId resets to ~1 while the phaseId increments, so
	// the FULL roundId still strictly increases even though the low
	// 64 bits regress. Dedup keyed on the low 64 bits would see
	// round=1 <= prev=<big> and silently stop emitting until restart.
	// Keying on the full uint80 keeps monotonicity across phase
	// rollovers. The storage PK (source, ledger, tx_hash, op_index,
	// ts) is idempotent, so even a re-emit on phase boundary is safe.
	roundID := decodeRoundID(bytes[22:32])

	// answer — int256, two's complement, in word 1.
	answer := decodeInt256(bytes[32:64])
	if answer.Sign() <= 0 {
		return Round{}, fmt.Errorf("%w: feed=%s round=%d answer=%s", ErrNonPositivePrice, feedAddress, roundID, answer.String())
	}

	// startedAt — uint256 in word 2 (we ignore it; updatedAt is the
	// authoritative timestamp).
	// updatedAt — uint256 in word 3.
	updatedAt := bigEndianUint64(bytes[120:128])
	if updatedAt == 0 || updatedAt > maxPlausibleUpdatedAtUnix {
		return Round{}, fmt.Errorf("%w: feed=%s round=%d out-of-range updatedAt %d", ErrMalformedResult, feedAddress, roundID, updatedAt)
	}
	// answeredInRound — uint80 in word 4 (ignored).

	return Round{
		FeedAddress: strings.ToLower(feedAddress),
		RoundID:     roundID,
		Answer:      answer.String(),
		UpdatedAt:   time.Unix(int64(updatedAt), 0).UTC(),
	}, nil
}

// decodeAnswerUpdatedLog parses one eth_getLogs entry from
// AggregatorV3's AnswerUpdated event. The event is:
//
//	event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt)
//
// Topics layout: [topic0=hash, topic1=current, topic2=roundId];
// Data layout: [updatedAt (uint256, 32 bytes)].
//
// One log row → one Round suitable for canonical.OracleUpdate
// projection (same as decodeLatestRoundData).
func decodeAnswerUpdatedLog(entry LogEntry) (Round, error) {
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
	// the dedup key is phase-aware, matching decodeLatestRoundData
	// (F-1323/G10-01).
	roundID := decodeRoundID(roundIDBytes[22:32])

	dataBytes, err := hexBytes(entry.Data)
	if err != nil {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.data hex: %w", ErrMalformedResult, err)
	}
	if len(dataBytes) != 32 {
		return Round{}, fmt.Errorf("%w: AnswerUpdated.data expected 32 bytes (one uint256), got %d", ErrMalformedResult, len(dataBytes))
	}
	updatedAt := bigEndianUint64(dataBytes[24:32])
	if updatedAt == 0 || updatedAt > maxPlausibleUpdatedAtUnix {
		return Round{}, fmt.Errorf("%w: %s round=%d out-of-range updatedAt %d", ErrMalformedResult, entry.Address, roundID, updatedAt)
	}

	return Round{
		FeedAddress: strings.ToLower(entry.Address),
		RoundID:     roundID,
		Answer:      current.String(),
		UpdatedAt:   time.Unix(int64(updatedAt), 0).UTC(),
	}, nil
}

// decodeInt256 parses a 32-byte big-endian two's-complement integer
// into a *big.Int. Identical semantics to
// internal/divergence/chainlink.go's decodeChainlinkInt256 — kept
// local to avoid a cross-package dep just for one function.
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
	return v.Uint64(), nil
}

// decodeRoundID reads a big-endian byte slice (the 10-byte uint80
// proxy roundId, or any width ≤ a uint256) into a *big.Int. We keep
// the FULL value rather than truncating to uint64 so the per-feed
// dedup key (RoundID) is phase-aware: the proxy roundId is
// (phaseId<<64)|aggregatorRoundId and only the wide value is
// monotonic across a phase rollover (F-1323/G10-01). new(big.Int)
// .SetBytes treats the input as an unsigned big-endian integer,
// which is exactly the uint80 layout Solidity left-pads into the
// return slot.
func decodeRoundID(b []byte) *big.Int {
	return new(big.Int).SetBytes(b)
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
