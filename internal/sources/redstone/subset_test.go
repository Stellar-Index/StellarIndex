package redstone

import (
	"encoding/binary"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ── Subset-filtered batch attribution (payload medians) ────────────────
//
// Real lake fixture: the REDSTONE event at ledger 59,258,375 (tx
// 1e9ddc61675fe641…), captured from stellar.contract_events on
// 2026-07-29. Its op args request feed_ids ["BTC","ETH"] but the
// adapter's freshness verifier dropped ETH, so updated_feeds carries ONE
// entry — the class that left 1,626 events honest-blind on the full
// completeness verify. The payload holds 3 signer packages per feed at
// timestamp 1,759,758,520,000; BTC's median is 12,449,969,251,710, which
// equals the surviving price byte-exactly, so attribution must yield
// exactly one BTC/USD update.

const subsetFixtureTx = "1e9ddc61675fe6419761781b68bc9ff8ef7cdb8be66df34d97b1c4bbe90897a4"

func subsetFixtureEvent() *events.Event {
	return &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     59258375,
		TxHash:     subsetFixtureTx,
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAABJgAAAARAAAAAQAAAAMAAAAPAAAAB3BheWxvYWQAAAAADQAAA4dFVEgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGs7lrw4AZm5yA7AAAAAIAAAAaVStYasO93sFR4XN3hj0Tqch8vL+PEAfPZ8z4bI4daKeGHe5Sz0UgZUwUAZ6Pd8+KnQ8mMc4m/foDSQUFNr5EgbRVRIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABrO51sAQGZucgOwAAAACAAAAGN2Q2jDaZZNaGuJ7MFv83lF7G+6QG2IBjrmlpqGcM641m+2AxQyp9Ztaunyf+PfmzrirNPqPPJH/gvhzXur/1KHEVUSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAazuPGtIBmbnIDsAAAAAgAAAB4oTMFgY7HPnZcy7NGFFcVrsowRPcB/rjE0zR9gAhUq1qg91uimzCEQCTo7unVkY8FRyXvw0lANCBQycJpgkL0xtCVEMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC1K7/qV+AZm5yA7AAAAAIAAAAY1vKe63LkgwTwhXqVIQt6TpoEEL/z6S0RbtkpCKaHvnJUwVb4jP6pJKt0Y3bJ4PYh2X83uFUApWjRNO16p4MDkbQlRDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAtSu/6lfgGZucgOwAAAACAAAAGA8R3odW/hVVVObKDzOE5yIGH5YX1foGyCnzwnP3GFLDs75iBv7lJ4hPrl8u7tcNn+Rq8veDNHmksLZULk0x5uG0JUQwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrvBmk4BmbnIDsAAAAAgAAAB8bzqT8gZ897nnzSBJ2LTamOw+UHMAbkSB7gqqMNXTXhV8n8coNMwqn8nBZubYPevrh7gyugGMrirX+jwvpwrHhwABjE3NTk3NTg1MzE2MzEjMC45LjAjc3RlbGxhci1jb25uZWN0b3IAACUAAALtVwEeAAAAAAAADwAAAA11cGRhdGVkX2ZlZWRzAAAAAAAAEAAAAAEAAAABAAAAEQAAAAEAAAADAAAADwAAABFwYWNrYWdlX3RpbWVzdGFtcAAAAAAAAAUAAAGZucgOwAAAAA8AAAAFcHJpY2UAAAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrv+pX4AAAAPAAAAD3dyaXRlX3RpbWVzdGFtcAAAAAAFAAABmbnITUAAAAAPAAAAB3VwZGF0ZXIAAAAAEgAAAAAAAAAA+pL1D+PjBeECGvNmk0i7fe2N6WSxRQLZP99ae7FY7Wk=",
		OpArgs:     []string{"AAAAEgAAAAAAAAAA+pL1D+PjBeECGvNmk0i7fe2N6WSxRQLZP99ae7FY7Wk=", "AAAAEAAAAAEAAAACAAAADgAAAANFVEgAAAAADgAAAANCVEMA", "AAAADQAAA4dFVEgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGs7lrw4AZm5yA7AAAAAIAAAAaVStYasO93sFR4XN3hj0Tqch8vL+PEAfPZ8z4bI4daKeGHe5Sz0UgZUwUAZ6Pd8+KnQ8mMc4m/foDSQUFNr5EgbRVRIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABrO51sAQGZucgOwAAAACAAAAGN2Q2jDaZZNaGuJ7MFv83lF7G+6QG2IBjrmlpqGcM641m+2AxQyp9Ztaunyf+PfmzrirNPqPPJH/gvhzXur/1KHEVUSAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAazuPGtIBmbnIDsAAAAAgAAAB4oTMFgY7HPnZcy7NGFFcVrsowRPcB/rjE0zR9gAhUq1qg91uimzCEQCTo7unVkY8FRyXvw0lANCBQycJpgkL0xtCVEMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC1K7/qV+AZm5yA7AAAAAIAAAAY1vKe63LkgwTwhXqVIQt6TpoEEL/z6S0RbtkpCKaHvnJUwVb4jP6pJKt0Y3bJ4PYh2X83uFUApWjRNO16p4MDkbQlRDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAtSu/6lfgGZucgOwAAAACAAAAGA8R3odW/hVVVObKDzOE5yIGH5YX1foGyCnzwnP3GFLDs75iBv7lJ4hPrl8u7tcNn+Rq8veDNHmksLZULk0x5uG0JUQwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAALUrvBmk4BmbnIDsAAAAAgAAAB8bzqT8gZ897nnzSBJ2LTamOw+UHMAbkSB7gqqMNXTXhV8n8coNMwqn8nBZubYPevrh7gyugGMrirX+jwvpwrHhwABjE3NTk3NTg1MzE2MzEjMC45LjAjc3RlbGxhci1jb25uZWN0b3IAACUAAALtVwEeAAAA"},
	}
}

func TestDecode_SubsetFilteredBatch_AttributedViaPayloadMedian(t *testing.T) {
	out, err := decodeWritePrices(subsetFixtureEvent(), time.Unix(1759758525, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d updates, want 1", len(out))
	}
	u := out[0]
	if u.Asset.String() != "crypto:BTC" || u.Quote.String() != "fiat:USD" {
		t.Errorf("attributed to %s/%s, want crypto:BTC/fiat:USD (ETH was the dropped feed)", u.Asset, u.Quote)
	}
	if u.Price.String() != "12449969251710" {
		t.Errorf("price = %s, want 12449969251710 (BTC payload median)", u.Price)
	}
	if got := u.Timestamp.UnixMilli(); got != 1759758520000 {
		t.Errorf("timestamp = %d, want package_timestamp 1759758520000", got)
	}
}

func TestParsePayload_RealFixture(t *testing.T) {
	payload, err := payloadFromOpArgs(subsetFixtureEvent().OpArgs)
	if err != nil {
		t.Fatal(err)
	}
	byFeed, err := parsePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(byFeed) != 2 || len(byFeed["BTC"]) != 3 || len(byFeed["ETH"]) != 3 {
		t.Fatalf("byFeed shape = %v", byFeed)
	}
	m, ok := medianAt(byFeed["BTC"], 1759758520000)
	if !ok || m.String() != "12449969251710" {
		t.Errorf("BTC median = %v ok=%v, want 12449969251710", m, ok)
	}
	m, ok = medianAt(byFeed["ETH"], 1759758520000)
	if !ok || m.String() != "460561235000" {
		t.Errorf("ETH median = %v ok=%v, want 460561235000", m, ok)
	}
	if _, ok := medianAt(byFeed["BTC"], 42); ok {
		t.Error("medianAt with a non-matching timestamp must report ok=false")
	}
}

// buildTestPayload constructs a syntactically-valid RedStone payload:
// one package per (feed, value) pair, all at timestamp ts.
func buildTestPayload(t *testing.T, ts uint64, points map[string][]int64) []byte {
	t.Helper()
	var out []byte
	pkgs := 0
	for feed, vals := range points {
		for _, v := range vals {
			// one data point: feedID(32) + value(8)
			var dp [40]byte
			copy(dp[:32], feed)
			binary.BigEndian.PutUint64(dp[32:], uint64(v))
			pkg := append([]byte{}, dp[:]...)
			var ts6 [6]byte
			ts6[0] = byte(ts >> 40)
			ts6[1] = byte(ts >> 32)
			binary.BigEndian.PutUint32(ts6[2:], uint32(ts))
			pkg = append(pkg, ts6[:]...)
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], 8)
			pkg = append(pkg, vs[:]...)
			pkg = append(pkg, 0, 0, 1) // dpCount=1
			pkg = append(pkg, make([]byte, 65)...)
			out = append(out, pkg...)
			pkgs++
		}
	}
	var cnt [2]byte
	binary.BigEndian.PutUint16(cnt[:], uint16(pkgs))
	out = append(out, cnt[:]...)
	out = append(out, 0, 0, 0) // unsignedMetadataSize = 0
	out = append(out, redstoneMarker...)
	return out
}

func pd(t *testing.T, price int64, ts uint64) priceDataDecoded {
	t.Helper()
	return priceDataDecoded{Price: amountFromInt64(t, price), PackageTimestamp: ts}
}

func TestAttributeSubset_Adversarial(t *testing.T) {
	const ts = uint64(1700000000000)
	payload := buildTestPayload(t, ts, map[string][]int64{
		"BTC":  {100, 101, 102},
		"USDC": {1_0000_0000},
		"USDT": {1_0000_0000},
	})

	t.Run("unique match attributes", func(t *testing.T) {
		got, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts)}, []string{"BTC", "USDC"}, payload)
		if err != nil || len(got) != 1 || got[0] != "BTC" {
			t.Fatalf("got %v err=%v, want [BTC]", got, err)
		}
	})
	t.Run("two identical-median candidates refuse", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 1_0000_0000, ts)}, []string{"USDC", "USDT"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("no matching candidate refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 999, ts)}, []string{"BTC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("bijection violation refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts), pd(t, 101, ts)}, []string{"BTC", "USDC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("timestamp mismatch refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts+1)}, []string{"BTC"}, payload)
		if !errors.Is(err, ErrAmbiguousSubset) {
			t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
		}
	})
	t.Run("malformed payload refuses", func(t *testing.T) {
		_, err := attributeSubset([]priceDataDecoded{pd(t, 101, ts)}, []string{"BTC"}, []byte("garbage"))
		if !errors.Is(err, ErrMalformedRedstonePayload) {
			t.Fatalf("err = %v, want ErrMalformedRedstonePayload", err)
		}
	})
}

func TestMedianAt_EvenCountTruncatedMean(t *testing.T) {
	pkgs := []payloadPackage{
		{Value: big.NewInt(10), TimestampMS: 1},
		{Value: big.NewInt(13), TimestampMS: 1},
	}
	m, ok := medianAt(pkgs, 1)
	if !ok || m.Int64() != 11 { // (10+13)/2 truncated
		t.Fatalf("even median = %v ok=%v, want 11", m, ok)
	}
}

func amountFromInt64(t *testing.T, v int64) canonical.Amount {
	t.Helper()
	return canonical.NewAmount(big.NewInt(v))
}
