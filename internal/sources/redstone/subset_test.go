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

// ── Order-preserving alignment (the 2026-07-30 residual class) ────────

// Real lake fixture: ledger 60104689's subset batch (7 feed_ids →
// 5 updated_feeds) where one price matches TWO candidates' medians
// (iBENJI_ETHEREUM_FUNDAMENTAL and SolvBTC.BBN_FUNDAMENTAL) — refused by
// the unordered rule, uniquely resolved by the order-preserving
// subsequence alignment (the adapter builds updated_feeds in one pass
// over feed_ids, so order is guaranteed).
func TestDecode_OrderPreservingAlignment_DisambiguatesSharedMedians(t *testing.T) {
	e := &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     60104689,
		TxHash:     "3333e95cc53cfe9c3fec32c8455d9c4c189b625dd34c66cb3bb4fcfa822e5bfc",
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAAAygAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAUAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZra6xHwAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfUdCwAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGa2utQcAAAABEAAAABAAAAAwAAAA8AAAARcGFja2FnZV90aW1lc3RhbXAAAAAAAAAFAAABmtrrEfAAAAAPAAAABXByaWNlAAAAAAAACwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9a/ZAAAADwAAAA93cml0ZV90aW1lc3RhbXAAAAAABQAAAZra61BwAAAAEQAAAAEAAAADAAAADwAAABFwYWNrYWdlX3RpbWVzdGFtcAAAAAAAAAUAAAGa2usR8AAAAA8AAAAFcHJpY2UAAAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX2ArsAAAAPAAAAD3dyaXRlX3RpbWVzdGFtcAAAAAAFAAABmtrrUHAAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZra6xHwAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGa2utQcAAAABEAAAABAAAAAwAAAA8AAAARcGFja2FnZV90aW1lc3RhbXAAAAAAAAAFAAABmtrrEfAAAAAPAAAABXByaWNlAAAAAAAACwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9eEAAAAADwAAAA93cml0ZV90aW1lc3RhbXAAAAAABQAAAZra61BwAAAADwAAAAd1cGRhdGVyAAAAABIAAAAAAAAAACOeYtRzBVeWeEd+3c4Ep57O6iY28c8Orgh5M25m+l0S",
		OpArgs:     []string{"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"},
	}
	out, err := decodeWritePrices(e, time.Unix(1750000000, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("got %d updates, want 5 (7 requested, 2 freshness-dropped)", len(out))
	}
	// Pin the exact attribution, not just the count: the unique
	// order-preserving alignment drops ETH and BTC (the first two
	// candidates) and maps the five surviving prices onto the
	// remaining feed_ids IN ORDER. A regression that found a different
	// (wrong) unique alignment would still pass a len-only assertion.
	wantPairs := []struct{ asset, quote string }{
		{"crypto:PYUSD", "fiat:USD"},
		{"crypto:USDC", "fiat:USD"},
		{"crypto:EUROC", "fiat:EUR"}, // the EUROC/EUR feed — EUR-quoted
		{"rwa:iBENJI", "fiat:USD"},
		{"crypto:SolvBTC.BBN_FUNDAMENTAL", "fiat:USD"},
	}
	for i, want := range wantPairs {
		if got := out[i].Asset.String(); got != want.asset {
			t.Errorf("out[%d].Asset = %s, want %s", i, got, want.asset)
		}
		if got := out[i].Quote.String(); got != want.quote {
			t.Errorf("out[%d].Quote = %s, want %s", i, got, want.quote)
		}
	}
}

func TestAttributeSubset_OrderDisambiguates(t *testing.T) {
	// Two prices each matching BOTH candidates' medians: unordered
	// attribution is ambiguous, but only ONE order-preserving complete
	// alignment exists (the diagonal), so it attributes [A, B].
	const ts = uint64(1700000000000)
	payload := buildTestPayload(t, ts, map[string][]int64{
		"USDA": {100},
		"USDB": {100},
	})
	got, err := attributeSubset([]priceDataDecoded{pd(t, 100, ts), pd(t, 100, ts)}, []string{"USDA", "USDB"}, payload)
	if err != nil || len(got) != 2 || got[0] != "USDA" || got[1] != "USDB" {
		t.Fatalf("got %v err=%v, want [USDA USDB]", got, err)
	}
	// One price, two identical-median candidates: STILL ambiguous under
	// order (two alignments) — must refuse.
	if _, err := attributeSubset([]priceDataDecoded{pd(t, 100, ts)}, []string{"USDA", "USDB"}, payload); !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset", err)
	}
}
