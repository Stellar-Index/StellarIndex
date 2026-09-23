// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chainlink

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
)

var twoTo255 = new(big.Int).Lsh(big.NewInt(1), 255)

func FuzzDecodeInt256RoundTrip(f *testing.F) {
	f.Add([]byte{0x01}, false)
	f.Add([]byte{0x01}, true)
	f.Add(twoTo255.Bytes(), true) // int256 min
	f.Add(new(big.Int).Sub(twoTo255, big.NewInt(1)).Bytes(), false)
	f.Add(big.NewInt(17_582_000).Bytes(), false)
	f.Fuzz(func(t *testing.T, mag []byte, neg bool) {
		v := new(big.Int).SetBytes(mag)
		if neg {
			v.Neg(v)
		}
		if v.Cmp(new(big.Int).Neg(twoTo255)) < 0 || v.Cmp(twoTo255) >= 0 {
			return // outside int256
		}
		var buf [32]byte
		encodeInt256(buf[:], v)
		if got := decodeInt256(buf[:]); got.Cmp(v) != 0 {
			t.Fatalf("decodeInt256(encode(%s)) = %s", v, got)
		}
	})
}

// uint256At reads a full 32-byte word as an unsigned integer.
func uint256At(b []byte, word int) *big.Int {
	return new(big.Int).SetBytes(b[word*32 : word*32+32])
}

func FuzzDecodeLatestRoundData(f *testing.F) {
	var ok [160]byte
	ok[31] = 7
	ok[63] = 0x10
	ok[127] = 0x01
	f.Add(ok[:])
	hi := ok
	hi[100] = 0x01 // updatedAt = 2^184 + 1: only the low 8 bytes are plausible
	f.Add(hi[:])
	negAns := ok
	for i := 32; i < 64; i++ {
		negAns[i] = 0xff
	}
	f.Add(negAns[:])
	f.Add([]byte{0xde, 0xad})
	f.Fuzz(func(t *testing.T, raw []byte) {
		rnd, err := decodeLatestRoundData("0x"+hex.EncodeToString(raw), "0xABC")
		if err != nil {
			return
		}
		if len(raw) != 160 {
			t.Fatalf("decoded a %d-byte return, want only 160", len(raw))
		}
		ans, okAns := new(big.Int).SetString(rnd.Answer, 10)
		if !okAns || ans.Sign() <= 0 || ans.Cmp(decodeInt256(raw[32:64])) != 0 {
			t.Fatalf("answer %q, want positive int256 word 1 = %s", rnd.Answer, decodeInt256(raw[32:64]))
		}
		if rnd.RoundID.Cmp(new(big.Int).SetBytes(raw[22:32])) != 0 {
			t.Fatalf("round id %s, want uint80 %x", rnd.RoundID, raw[22:32])
		}
		upd := uint256At(raw, 3)
		if upd.Sign() <= 0 || upd.Cmp(big.NewInt(maxPlausibleUpdatedAtUnix)) > 0 {
			t.Fatalf("accepted out-of-range uint256 updatedAt %s as %s", upd, rnd.UpdatedAt)
		}
		if rnd.UpdatedAt.Unix() != upd.Int64() {
			t.Fatalf("updatedAt %d, want %s", rnd.UpdatedAt.Unix(), upd)
		}
		if rnd.FeedAddress != "0xabc" {
			t.Fatalf("feed address %q not lower-cased", rnd.FeedAddress)
		}
	})
}

func FuzzDecodeAnswerUpdatedLog(f *testing.F) {
	word := func(b ...byte) []byte {
		w := make([]byte, 32)
		copy(w[32-len(b):], b)
		return w
	}
	f.Add(word(0x10), word(0x07), word(0x01))
	hiData := word(0x01)
	hiData[0] = 0x01 // updatedAt = 2^248 + 1
	f.Add(word(0x10), word(0x07), hiData)
	f.Add(word(0x10), word(0x07), []byte{0x01})
	f.Fuzz(func(t *testing.T, current, roundID, data []byte) {
		rnd, err := decodeAnswerUpdatedLog(LogEntry{
			Address: "0xFEED",
			Topics:  []string{"0x00", "0x" + hex.EncodeToString(current), "0x" + hex.EncodeToString(roundID)},
			Data:    "0x" + hex.EncodeToString(data),
		})
		if err != nil {
			return
		}
		if len(current) != 32 || len(roundID) != 32 || len(data) != 32 {
			t.Fatalf("decoded non-word shapes %d/%d/%d", len(current), len(roundID), len(data))
		}
		ans, _ := new(big.Int).SetString(rnd.Answer, 10)
		if ans == nil || ans.Sign() <= 0 || ans.Cmp(decodeInt256(current)) != 0 {
			t.Fatalf("answer %q, want positive int256 %s", rnd.Answer, decodeInt256(current))
		}
		upd := new(big.Int).SetBytes(data)
		if upd.Sign() <= 0 || upd.Cmp(big.NewInt(maxPlausibleUpdatedAtUnix)) > 0 {
			t.Fatalf("accepted out-of-range uint256 updatedAt %s as %s", upd, rnd.UpdatedAt)
		}
		if rnd.UpdatedAt.Unix() != upd.Int64() {
			t.Fatalf("updatedAt %d, want %s", rnd.UpdatedAt.Unix(), upd)
		}
	})
}

// updatedAt is a uint256 word: a value whose low 8 bytes look like a
// plausible unix time but whose high bytes are set is out of range, not
// that time.
func TestDecode_RejectsUpdatedAtAboveUint64(t *testing.T) {
	raw := buildLatestRoundDataReturn(t, 7, big.NewInt(17_582_000), 1_745_000_000, 1_745_000_000, 7)
	b, err := hex.DecodeString(raw[2:])
	if err != nil {
		t.Fatal(err)
	}
	b[119] = 0x01 // updatedAt = 2^64 + 1_745_000_000
	if rnd, err := decodeLatestRoundData("0x"+hex.EncodeToString(b), "0xabc"); err == nil {
		t.Fatalf("latestRoundData: accepted updatedAt 2^64+1745000000 as %s", rnd.UpdatedAt)
	}

	word := func(v int64) []byte {
		w := make([]byte, 32)
		big.NewInt(v).FillBytes(w)
		return w
	}
	data := word(1_745_000_000)
	data[0] = 0x01
	entry := LogEntry{
		Address: "0xabc",
		Topics:  []string{"0x00", "0x" + hex.EncodeToString(word(17_582_000)), "0x" + hex.EncodeToString(word(7))},
		Data:    "0x" + hex.EncodeToString(data),
	}
	if rnd, err := decodeAnswerUpdatedLog(entry); err == nil {
		t.Fatalf("AnswerUpdated: accepted updatedAt with high bytes set as %s", rnd.UpdatedAt)
	}
}

// A zero answer is not a price: oracle_updates has CHECK (price > 0).
func TestDecode_ZeroAnswerIsNonPositive(t *testing.T) {
	raw := buildLatestRoundDataReturn(t, 1, big.NewInt(0), 0, 1_745_000_000, 1)
	if _, err := decodeLatestRoundData(raw, "0xabc"); !errors.Is(err, ErrNonPositivePrice) {
		t.Errorf("latestRoundData zero answer: err = %v, want ErrNonPositivePrice", err)
	}
	zero := "0x" + hex.EncodeToString(make([]byte, 32))
	data := make([]byte, 32)
	big.NewInt(1_745_000_000).FillBytes(data)
	entry := LogEntry{Address: "0xabc", Topics: []string{"0x00", zero, zero}, Data: "0x" + hex.EncodeToString(data)}
	if _, err := decodeAnswerUpdatedLog(entry); !errors.Is(err, ErrNonPositivePrice) {
		t.Errorf("AnswerUpdated zero answer: err = %v, want ErrNonPositivePrice", err)
	}
}

// maxPlausibleUpdatedAtUnix is an inclusive bound.
func TestDecode_UpdatedAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		updatedAt uint64
		ok        bool
	}{
		{1, true},
		{maxPlausibleUpdatedAtUnix, true},
		{maxPlausibleUpdatedAtUnix + 1, false},
	} {
		raw := buildLatestRoundDataReturn(t, 1, big.NewInt(100), 0, tc.updatedAt, 1)
		rnd, err := decodeLatestRoundData(raw, "0xabc")
		if (err == nil) != tc.ok {
			t.Fatalf("updatedAt=%d: err = %v, want ok=%v", tc.updatedAt, err, tc.ok)
		}
		if tc.ok && uint64(rnd.UpdatedAt.Unix()) != tc.updatedAt {
			t.Fatalf("updatedAt=%d decoded as %d", tc.updatedAt, rnd.UpdatedAt.Unix())
		}
	}
}

func FuzzDecodeDecimals(f *testing.F) {
	w := make([]byte, 32)
	w[31] = 8
	f.Add(w)
	f.Add(make([]byte, 32))
	f.Add([]byte{0x08})
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := decodeDecimals("0x" + hex.EncodeToString(raw))
		if err != nil {
			return
		}
		full := new(big.Int).SetBytes(raw)
		if len(raw) != 32 || d == 0 || full.Cmp(big.NewInt(int64(d))) != 0 {
			t.Fatalf("decodeDecimals(%x) = %d, want the exact 1..255 word", raw, d)
		}
	})
}
