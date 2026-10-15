package chainlink_stellar

import (
	"errors"
	"math/big"
	"testing"
)

// Generative run (the seed corpus also runs under plain `go test`):
//
//	go test -run=NONE -fuzz=FuzzDecodeFeedUpdated -fuzztime=60s -parallel=2 ./internal/sources/chainlink_stellar/

// FuzzDecodeFeedUpdated checks the answer round-trips through I256 exactly, and a non-positive
// one is refused rather than written.
func FuzzDecodeFeedUpdated(f *testing.F) {
	f.Add(int64(12134109), uint64(1_000_000_000_000), uint64(1792073549))
	f.Add(int64(-1), uint64(1), uint64(0))
	f.Add(int64(0), uint64(0), uint64(1792073549))
	f.Fuzz(func(t *testing.T, hi int64, lo uint64, ts uint64) {
		answer := new(big.Int).Lsh(big.NewInt(hi), 64)
		answer.Add(answer, new(big.Int).SetUint64(lo))
		ev := feedEvent(topicLINKID, encodeBody(t, answer, ts))
		out, err := NewDecoder(MainnetCacheContract).Decode(ev)
		if answer.Sign() <= 0 {
			if !errors.Is(err, ErrNonPositiveAnswer) {
				t.Fatalf("answer %s: err = %v, want ErrNonPositiveAnswer", answer, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("answer %s: %v", answer, err)
		}
		if got := out[0].(UpdateEvent).Update.Price.String(); got != answer.String() {
			t.Fatalf("price = %s, want %s", got, answer)
		}
	})
}
