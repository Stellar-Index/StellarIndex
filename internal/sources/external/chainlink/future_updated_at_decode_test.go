package chainlink

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
	"time"
)

// decodeTestNow is the poller clock the decoder unit tests run at; every
// fixed updatedAt fixture in this package predates it.
var decodeTestNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func word32(v uint64) string {
	var b [32]byte
	putUint64BE(b[24:32], v)
	return "0x" + hex.EncodeToString(b[:])
}

// answerUpdatedLog builds one AnswerUpdated(current, roundId, updatedAt) log.
func answerUpdatedLog(current, roundID, updatedAt uint64) LogEntry {
	return LogEntry{
		Address: "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419",
		Topics:  []string{AnswerUpdatedTopic0, word32(current), word32(roundID)},
		Data:    word32(updatedAt),
	}
}

// TestDecoders_refuseFutureUpdatedAt pins the forward bound on both decode
// paths (live latestRoundData and the backfill AnswerUpdated log): an
// updatedAt an hour past the poller clock is refused as ErrFutureUpdatedAt.
func TestDecoders_refuseFutureUpdatedAt(t *testing.T) {
	t.Parallel()
	future := uint64(decodeTestNow.Add(time.Hour).Unix())

	raw := buildLatestRoundDataReturn(t, 1, big.NewInt(100), 0, future, 1)
	if _, err := decodeLatestRoundData(raw, "0xabc", decodeTestNow); !errors.Is(err, ErrFutureUpdatedAt) {
		t.Errorf("decodeLatestRoundData err = %v, want ErrFutureUpdatedAt", err)
	}
	if _, err := decodeAnswerUpdatedLog(answerUpdatedLog(100, 1, future), decodeTestNow); !errors.Is(err, ErrFutureUpdatedAt) {
		t.Errorf("decodeAnswerUpdatedLog err = %v, want ErrFutureUpdatedAt", err)
	}
}

// TestDecoders_acceptUpdatedAtAtSkewEdge: exactly now+maxUpdatedAtFutureSkew
// is still accepted on both paths and keeps its exact timestamp.
func TestDecoders_acceptUpdatedAtAtSkewEdge(t *testing.T) {
	t.Parallel()
	edge := decodeTestNow.Add(maxUpdatedAtFutureSkew)
	want := time.Unix(edge.Unix(), 0).UTC()

	raw := buildLatestRoundDataReturn(t, 1, big.NewInt(100), 0, uint64(edge.Unix()), 1)
	rnd, err := decodeLatestRoundData(raw, "0xabc", decodeTestNow)
	if err != nil || !rnd.UpdatedAt.Equal(want) {
		t.Errorf("decodeLatestRoundData = (%v, %v), want UpdatedAt %v", rnd.UpdatedAt, err, want)
	}
	rnd, err = decodeAnswerUpdatedLog(answerUpdatedLog(100, 1, uint64(edge.Unix())), decodeTestNow)
	if err != nil || !rnd.UpdatedAt.Equal(want) {
		t.Errorf("decodeAnswerUpdatedLog = (%v, %v), want UpdatedAt %v", rnd.UpdatedAt, err, want)
	}
}
