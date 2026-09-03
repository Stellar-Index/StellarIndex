package redstone

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The RedStone payload parser is the only place in the ingest path that
// walks an ATTACKER-SHAPED binary format by hand: parsePayload reads the
// third op-arg of write_prices backwards from its end, deriving every
// offset from length fields inside the buffer itself
// (internal/sources/redstone/payload.go:110-162). Signatures are
// deliberately not verified, so nothing upstream constrains the bytes
// beyond "the adapter accepted them"; a backfill replays whatever the
// lake holds.
//
// Its two failure modes are exactly the two a fuzz target is for:
//
//   - an unchecked offset (a slice expression on a length the payload
//     chose) panics the ingest goroutine;
//   - a partial read succeeds and ATTRIBUTES A PRICE TO THE WRONG FEED,
//     which is the misattribution this file's whole design — refuse
//     rather than guess — exists to prevent.
//
// Both fuzz targets run as plain seed-corpus tests under `go test`; the
// generative runs are
//
//	go test -run=^$ -fuzz=FuzzParsePayload    -fuzztime=20s ./internal/sources/redstone/
//	go test -run=^$ -fuzz=FuzzAttributeSubset -fuzztime=20s ./internal/sources/redstone/

// fuzzSeedPayloads returns the seed corpus: the REAL ledger-59,258,375
// payload (the subset-filtered fixture the parser was written for) plus
// synthetic ones, and truncations of the real payload at each structural
// boundary — the offsets where an unchecked read would land.
func fuzzSeedPayloads(f *testing.F) [][]byte {
	fixture, err := payloadFromOpArgs(subsetFixtureEvent().OpArgs)
	if err != nil {
		f.Fatalf("real fixture payload: %v", err)
	}
	seeds := [][]byte{
		fixture,
		nil,
		{},
		append([]byte{}, redstoneMarker...),
	}
	// Head truncations: drop the marker, the metadata size, the package
	// count, and progressively more of the last package's trailer.
	for _, cut := range []int{1, markerLen, markerLen + metaSizeLen, markerLen + metaSizeLen + pkgCountLen, 100, 500} {
		if cut < len(fixture) {
			seeds = append(seeds, append([]byte{}, fixture[:len(fixture)-cut]...))
		}
	}
	// Tail truncations: keep the trailer, lose the packages it points at.
	for _, cut := range []int{1, 40, 400} {
		if cut < len(fixture) {
			seeds = append(seeds, append([]byte{}, fixture[cut:]...))
		}
	}
	return seeds
}

func FuzzParsePayload(f *testing.F) {
	for _, s := range fuzzSeedPayloads(f) {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		// Property 1 — totality: no offset in the backwards walk may
		// panic, whatever the embedded length fields claim.
		byFeed, err := parsePayload(payload)
		// Property 2 — one refusal class. The doc contract is that every
		// structural failure wraps ErrMalformedRedstonePayload so the
		// caller can treat "payload unparseable" as a single honest-blind
		// outcome; a bare error would fall through the caller's
		// errors.Is and be reported as a decode bug instead.
		if err != nil {
			if !errors.Is(err, ErrMalformedRedstonePayload) {
				t.Fatalf("parse error does not wrap ErrMalformedRedstonePayload: %v", err)
			}
			if byFeed != nil {
				t.Fatalf("error return also produced %d feeds; a partial result is "+
					"exactly what must not escape (it would attribute a price to "+
					"the wrong asset)", len(byFeed))
			}
			return
		}

		// Property 3 — a success is never empty and never degenerate.
		// pkgCount ≥ 1 and dpCount ≥ 1 are enforced, and an empty feed id
		// is rejected, so a successful parse carries at least one named
		// feed with at least one package.
		if len(byFeed) == 0 {
			t.Fatal("parse succeeded with zero feeds; the caller would then " +
				"match no candidate and refuse a payload that parsed")
		}
		total := 0
		for feed, pkgs := range byFeed {
			if feed == "" {
				t.Fatal("parse produced an empty feed id")
			}
			// NOTE: no assertion that the id is printable. The 32-byte
			// slot is zero-RIGHT-padded and parsePayload trims exactly
			// that; a slot holding arbitrary bytes yields an id that
			// simply matches no op-args feed_id, which is a refusal, not
			// a misattribution. Asserting otherwise would be asserting a
			// property of the INPUT.
			if len(pkgs) == 0 {
				t.Fatalf("feed %q has zero packages", feed)
			}
			for _, p := range pkgs {
				if p.Value == nil {
					t.Fatalf("feed %q has a nil value; medianAt would panic on it", feed)
				}
				if p.Value.Sign() < 0 {
					t.Fatalf("feed %q value %s is negative; values are decoded from "+
						"unsigned magnitude bytes", feed, p.Value)
				}
			}
			total += len(pkgs)
		}

		// Property 4 — bounded. The defensive ceilings cap a payload at
		// maxPayloadPkgs × maxPkgDataPts packages however large the
		// declared counts are; without them one op-arg sizes the map.
		if ceiling := maxPayloadPkgs * maxPkgDataPts; total > ceiling {
			t.Fatalf("parsed %d packages (> %d); the defensive ceilings are not holding", total, ceiling)
		}

		// Property 5 — determinism. Attribution compares medians derived
		// from this map; a parse that varied run to run would make the
		// same event attribute differently on replay.
		again, err2 := parsePayload(payload)
		if err2 != nil {
			t.Fatalf("second parse of the same bytes failed: %v", err2)
		}
		if len(again) != len(byFeed) {
			t.Fatalf("parse is not deterministic: %d feeds then %d", len(byFeed), len(again))
		}
		for feed, pkgs := range byFeed {
			other := again[feed]
			if len(other) != len(pkgs) {
				t.Fatalf("feed %q: %d packages then %d", feed, len(pkgs), len(other))
			}
			for i := range pkgs {
				if pkgs[i].TimestampMS != other[i].TimestampMS || pkgs[i].Value.Cmp(other[i].Value) != 0 {
					t.Fatalf("feed %q package %d differs between parses", feed, i)
				}
			}
		}
	})
}

// FuzzAttributeSubset fuzzes the attribution itself: the order-preserving
// DP that decides which feed_id each surviving price belongs to.
//
// The invariant under test is the one the F1 caveat in payload.go is
// about — a returned assignment must be JUSTIFIED, not merely
// well-formed. Every price it assigns must equal that feed's payload
// median at that price's own package_timestamp EXACTLY, and the
// assignment must be strictly order-preserving in feed_ids (the adapter
// builds updated_feeds in one pass, which is why the equal-arity case
// zips positionally). Anything else is a misattribution: a price
// published under another asset's name.
func FuzzAttributeSubset(f *testing.F) {
	realPayload, err := payloadFromOpArgs(subsetFixtureEvent().OpArgs)
	if err != nil {
		f.Fatalf("real fixture payload: %v", err)
	}
	// The real subset-filtered case: BTC survived, ETH was dropped.
	f.Add(realPayload, "BTC,ETH", int64(12449969251710), int64(0), uint64(1759758520000), false)
	f.Add(realPayload, "ETH,BTC", int64(460561235000), int64(12449969251710), uint64(1759758520000), true)
	f.Add(realPayload, "", int64(1), int64(2), uint64(0), true)
	f.Add([]byte(nil), "BTC", int64(1), int64(2), uint64(1), false)
	// The cross-feed median collision that the ORDER constraint resolves.
	f.Add(realPayload, "BTC,BTC,ETH", int64(12449969251710), int64(460561235000), uint64(1759758520000), true)
	// Prices that are NOT either feed's median, at a timestamp both feeds
	// DO have packages for. The only correct answer is a refusal; a
	// matcher that keyed on the timestamp and forgot the value would find
	// a unique order-preserving alignment here and publish 1 and 2 as
	// BTC/USD and ETH/USD.
	f.Add(realPayload, "BTC,ETH", int64(1), int64(2), uint64(1759758520000), true)

	f.Fuzz(func(t *testing.T, payload []byte, feedsCSV string, p0, p1 int64, ts uint64, two bool) {
		feedIDs := fuzzFeedIDs(feedsCSV)
		prices := []priceDataDecoded{
			{Price: canonical.NewAmount(big.NewInt(p0)), PackageTimestamp: ts},
		}
		if two {
			prices = append(prices, priceDataDecoded{
				Price: canonical.NewAmount(big.NewInt(p1)), PackageTimestamp: ts,
			})
		}

		// Property 1 — totality, including the DP walk that "cannot" lose
		// uniqueness.
		assigned, err := attributeSubset(prices, feedIDs, payload)
		// Property 2 — refusals stay inside the two declared classes, so
		// the completeness verifier's honest-blind accounting holds.
		if err != nil {
			if !errors.Is(err, ErrAmbiguousSubset) && !errors.Is(err, ErrMalformedRedstonePayload) {
				t.Fatalf("refusal is neither ambiguous nor malformed: %v", err)
			}
			if assigned != nil {
				t.Fatalf("refusal also returned an assignment %v", assigned)
			}
			return
		}

		// Property 3 — arity. One feed per surviving price, no more.
		if len(assigned) != len(prices) {
			t.Fatalf("assigned %d feeds for %d prices", len(assigned), len(prices))
		}

		// Property 4 — every assignment is JUSTIFIED and ORDER-PRESERVING.
		byFeed, perr := parsePayload(payload)
		if perr != nil {
			t.Fatalf("attribution succeeded on a payload that does not parse: %v", perr)
		}
		prev := -1
		for i, feed := range assigned {
			idx := indexOfFeed(feedIDs, feed)
			if idx < 0 {
				t.Fatalf("price %d assigned to %q, which is not among the op-args feed_ids %v",
					i, feed, feedIDs)
			}
			if idx <= prev {
				t.Fatalf("assignment is not strictly order-preserving: price %d → feed_ids[%d] "+
					"after price %d → feed_ids[%d]. updated_feeds is a SUBSEQUENCE of "+
					"feed_ids; a non-monotonic assignment names the wrong asset",
					i, idx, i-1, prev)
			}
			prev = idx
			mv, ok := medianAt(byFeed[feed], prices[i].PackageTimestamp)
			if !ok {
				t.Fatalf("price %d assigned to %q, which has no package at timestamp %d",
					i, feed, prices[i].PackageTimestamp)
			}
			if mv.Cmp(prices[i].Price.BigInt()) != 0 {
				t.Fatalf("price %d (%s) assigned to %q whose median is %s — an "+
					"UNJUSTIFIED attribution: the published price is not that feed's",
					i, prices[i].Price, feed, mv)
			}
		}
	})
}

// fuzzFeedIDs turns the fuzzer's string into a small, unique feed_ids
// vector. Unique because the property under test indexes back into it
// (duplicates make "which slot was this?" unanswerable, and real op-args
// feed_ids are unique); small because the alignment DP is O(n×m) and the
// interesting cases are batch-sized, not pathological.
func fuzzFeedIDs(csv string) []string {
	const maxFeeds = 8
	seen := make(map[string]bool)
	out := make([]string, 0, maxFeeds)
	// Walk with Cut rather than Split: the fuzzer will hand this a
	// megabyte of commas, and materialising that whole slice costs more
	// per exec than the function under test does.
	for rest := csv; rest != "" && len(out) < maxFeeds; {
		var f string
		var more bool
		f, rest, more = strings.Cut(rest, ",")
		if !more {
			rest = ""
		}
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func indexOfFeed(feedIDs []string, feed string) int {
	for i, f := range feedIDs {
		if f == feed {
			return i
		}
	}
	return -1
}
