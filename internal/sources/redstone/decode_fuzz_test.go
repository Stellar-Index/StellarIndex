package redstone

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Generative runs (seed corpora also run under plain `go test`):
//
//	go test -run=NONE -fuzz=FuzzSdkDecodeBodyPrice      -fuzztime=60s -parallel=2 ./internal/sources/redstone/
//	go test -run=NONE -fuzz=FuzzReciprocalAtScale       -fuzztime=60s -parallel=2 ./internal/sources/redstone/
//	go test -run=NONE -fuzz=FuzzDecodeWritePricesSingle -fuzztime=60s -parallel=2 ./internal/sources/redstone/

// ref256 is the reference U256 value of four big-endian 64-bit words,
// built independently of canonical.FromUInt256Parts.
func ref256(hh, hl, lh, ll uint64) *big.Int {
	v := new(big.Int)
	for _, w := range []uint64{hh, hl, lh, ll} {
		v.Lsh(v, 64)
		v.Or(v, new(big.Int).SetUint64(w))
	}
	return v
}

// bytesWrap re-emits an XDR ScVal body as ScVal::Bytes(xdr(body)), the
// shape the mainnet adapter actually publishes.
func bytesWrap(t *testing.T, b64 string) string {
	t.Helper()
	inner, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	b := xdr.ScBytes(inner)
	raw, err := xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &b}.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal bytes body: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// FuzzSdkDecodeBodyPrice round-trips a full-width U256 price and both
// timestamps through the body decoder, in both the bare-Map and the
// Bytes-wrapped body shapes, against the reference 256-bit value.
func FuzzSdkDecodeBodyPrice(f *testing.F) {
	f.Add(uint64(0), uint64(0), uint64(0), uint64(oneBTCAt8), uint64(1_745_000_000_000), uint64(1_745_000_060_000), true)
	f.Add(uint64(0), uint64(0), uint64(1), uint64(0), uint64(0), ^uint64(0), false) // 2^64
	f.Add(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), uint64(0), true)
	f.Add(uint64(1<<63), uint64(0), uint64(0), uint64(0), uint64(1), uint64(2), false)

	f.Fuzz(func(t *testing.T, hh, hl, lh, ll, pkg, wr uint64, wrap bool) {
		want := ref256(hh, hl, lh, ll)
		body := encodeWritePricesBody(t, relayerG, []*big.Int{want}, pkg, wr)
		if wrap {
			body = bytesWrap(t, body)
		}
		got, updater, err := sdkDecodeBody(body)
		if err != nil {
			t.Fatalf("well-formed body rejected: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d prices for one PriceData", len(got))
		}
		if got[0].Price.BigInt().Cmp(want) != 0 {
			t.Fatalf("price = %s, want the exact u256 %s (ADR-0003)", got[0].Price, want)
		}
		if got[0].PackageTimestamp != pkg || got[0].WriteTimestamp != wr {
			t.Fatalf("timestamps = (%d, %d), want (%d, %d)", got[0].PackageTimestamp, got[0].WriteTimestamp, pkg, wr)
		}
		if updater != relayerG {
			t.Fatalf("updater = %q, want %q", updater, relayerG)
		}
	})
}

// refReciprocal is round-half-up(10^(2d) / r), computed with QuoRem so
// it shares no arithmetic with reciprocalAtScale.
func refReciprocal(r *big.Int, d uint8) *big.Int {
	num := new(big.Int).Exp(big.NewInt(10), big.NewInt(2*int64(d)), nil)
	q, rem := new(big.Int).QuoRem(num, r, new(big.Int))
	if new(big.Int).Lsh(rem, 1).Cmp(r) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func FuzzReciprocalAtScale(f *testing.F) {
	f.Add(big.NewInt(1_739_110_000).Bytes(), uint8(8))
	f.Add([]byte{8}, uint8(1)) // 100/8 = 12.5: the half-up tie
	f.Add([]byte{3}, uint8(1))
	f.Add([]byte{1}, uint8(0))
	f.Add(ref256(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)).Bytes(), uint8(8))

	f.Fuzz(func(t *testing.T, rb []byte, d uint8) {
		if len(rb) > 64 {
			rb = rb[:64]
		}
		r := new(big.Int).SetBytes(rb)
		if r.Sign() == 0 {
			t.Skip("caller guarantees r > 0")
		}
		d %= 40
		got := reciprocalAtScale(canonical.NewAmount(r), d).BigInt()
		if want := refReciprocal(r, d); got.Cmp(want) != 0 {
			t.Fatalf("reciprocal(%s @%d) = %s, want %s", r, d, got, want)
		}
		// Monotone non-increasing in r: a larger units-per-USD value
		// can never produce a larger USD value.
		next := reciprocalAtScale(canonical.NewAmount(new(big.Int).Add(r, big.NewInt(1))), d).BigInt()
		if next.Cmp(got) > 0 {
			t.Fatalf("reciprocal(%s) = %s < reciprocal(%s+1) = %s", r, got, r, next)
		}
		if r.Sign() != 0 && got.Sign() < 0 {
			t.Fatalf("negative reciprocal %s", got)
		}
	})
}

// FuzzDecodeWritePricesSingle drives decodeWritePrices end-to-end with
// one priced feed and checks the emitted row: a registry feed's raw
// value passes through exactly unless the feed is Invert, in which
// case it is the exact reciprocal; a row is never emitted with a
// non-positive price; and the package timestamp is used only inside
// the plausible window.
func FuzzDecodeWritePricesSingle(f *testing.F) {
	f.Add(uint64(0), uint64(0), uint64(0), uint64(oneBTCAt8), uint64(1_745_000_000_000), uint8(0))
	f.Add(uint64(0), uint64(0), uint64(0), uint64(1_739_110_000), uint64(1_745_000_000_000), uint8(1))
	f.Add(uint64(0), uint64(0), uint64(0), uint64(3e16), uint64(1_745_000_000_000), uint8(1)) // reciprocal rounds to 0
	f.Add(uint64(0), uint64(0), uint64(0), uint64(0), uint64(0), uint8(0))
	f.Add(uint64(1), uint64(0), uint64(0), uint64(0), ^uint64(0), uint8(2))

	feeds := []string{"BTC", "MXNe", "ZZZ_UNLISTED/EUR"}
	f.Fuzz(func(t *testing.T, hh, hl, lh, ll, pkg uint64, feedSel uint8) {
		feed := feeds[int(feedSel)%len(feeds)]
		raw := ref256(hh, hl, lh, ll)
		closedAt := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
		ev := &events.Event{
			Topic:  []string{TopicSymbolRedstone},
			Value:  encodeWritePricesBody(t, relayerG, []*big.Int{raw}, pkg, pkg),
			OpArgs: []string{encodeAddressArg(t, relayerG), encodeStringVecArg(t, []string{feed}), encodePayloadArg(t)},

			ContractID: adapterC,
			Ledger:     52_000_000,
			TxHash:     "fuzz",
		}
		updates, err := decodeWritePrices(ev, closedAt)
		if err != nil {
			if !errors.Is(err, ErrEmptyUpdates) {
				t.Fatalf("well-formed single-feed event: unexpected error %v", err)
			}
			// The only positive price that may be dropped is an Invert
			// feed whose reciprocal rounds to zero at the row's scale.
			if raw.Sign() > 0 && (feed != "MXNe" || refReciprocal(raw, DefaultDecimals).Sign() > 0) {
				t.Fatalf("positive %s price %s dropped: %v", feed, raw, err)
			}
			return
		}
		if len(updates) != 1 {
			t.Fatalf("got %d updates for one feed", len(updates))
		}
		u := updates[0]
		if u.Price.Sign() <= 0 {
			t.Fatalf("%s row emitted with non-positive price %s (raw %s)", feed, u.Price, raw)
		}
		want := raw
		if entry, ok := lookupFeed(feed); ok && entry.Invert {
			want = refReciprocal(raw, DefaultDecimals)
		}
		if u.Price.BigInt().Cmp(want) != 0 {
			t.Fatalf("%s price = %s, want %s (raw %s)", feed, u.Price, want, raw)
		}
		ceilMs := uint64(closedAt.Add(canonical.SafeUnixFutureWindow).UnixMilli())
		wantTs := closedAt
		if pkg >= 1_000_000_000_000 && pkg <= ceilMs {
			wantTs = time.UnixMilli(int64(pkg)).UTC()
		}
		if !u.Timestamp.Equal(wantTs) {
			t.Fatalf("ts = %s for package_timestamp %d, want %s", u.Timestamp, pkg, wantTs)
		}
	})
}
