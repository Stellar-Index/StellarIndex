package redstone

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// This file parses the RedStone signed-payload wire format — the third
// argument of every write_prices call — far enough to recover, per feed,
// the signer value set and package timestamp. It exists for exactly one
// case: SUBSET-FILTERED batches, where the adapter's freshness verifier
// dropped ≥1 of the requested feeds, so the event's updated_feeds is
// SHORTER than the op-args feed_ids and the positional zip in
// decodeWritePrices cannot attribute prices to feeds.
//
// That class is real and ongoing, not a legacy quirk: the 2026-07-29 full
// completeness run left 1,626 events blind across [59258375, tip], and the
// count grows by a few per day at tip. The payload closes it rigorously:
// the adapter stores the MEDIAN of the signer values for each feed it
// accepts, so a surviving updated_feeds entry's price MUST equal one
// candidate feed's payload median at that entry's package_timestamp —
// verified byte-exact against the real ledger-59258375 event (price
// 12,449,969,251,710 == BTC's median of three signer values; ETH was the
// dropped feed). Attribution demands a UNIQUE such feed and a bijection
// overall; anything ambiguous refuses the whole event, which keeps it in
// the completeness verifier's honest-blind class rather than guessing.
//
// Wire layout (all integers big-endian; parsed from the END, per the
// RedStone protocol docs):
//
//	[dataPackage 1]…[dataPackage N] [packagesCount 2B]
//	[unsignedMetadataSize 3B] [unsignedMetadata] [redstoneMarker 9B]
//
// each dataPackage, also from its end:
//
//	[dataPoint 1]…[dataPoint M] [timestampMS 6B]
//	[valueByteSize 4B] [dataPointsCount 3B] [signature 65B]
//
// each dataPoint: [feedID 32B zero-right-padded] [value valueByteSize B]
//
// Signatures are deliberately NOT verified here: the payload was already
// accepted on-chain by the adapter (the event is the proof); this parser
// only needs the (feed, value, timestamp) triples the contract aggregated.
var redstoneMarker = []byte{0x00, 0x00, 0x02, 0xed, 0x57, 0x01, 0x1e, 0x00, 0x00}

const (
	markerLen       = 9
	metaSizeLen     = 3
	pkgCountLen     = 2
	signatureLen    = 65
	dpCountLen      = 3
	valueSizeLen    = 4
	timestampLen    = 6
	feedIDLen       = 32
	maxPayloadPkgs  = 256  // defensive ceiling; real batches carry signers×feeds ≈ tens
	maxPkgDataPts   = 256  // defensive ceiling per package
	maxPayloadValue = 1024 // defensive ceiling on valueByteSize
)

// ErrMalformedRedstonePayload wraps every structural failure below so
// callers can treat "payload unparseable" as one refusal class.
var ErrMalformedRedstonePayload = errors.New("redstone: malformed payload")

// payloadPackage is one signed data point recovered from the payload:
// a single signer's value for a single feed at a batch timestamp.
type payloadPackage struct {
	Value       *big.Int
	TimestampMS uint64
}

// parsePayload recovers feed → signer packages from a raw RedStone
// payload. Every offset is bounds-checked; any inconsistency returns
// [ErrMalformedRedstonePayload] rather than a partial result, because the
// caller uses the output to ATTRIBUTE prices — a truncated read that
// silently dropped one feed's packages could attribute a price to the
// wrong asset, which is the exact failure this parser exists to prevent.
func parsePayload(payload []byte) (map[string][]payloadPackage, error) {
	n := len(payload)
	if n < markerLen+metaSizeLen+pkgCountLen {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the fixed trailer", ErrMalformedRedstonePayload, n)
	}
	if !bytes.Equal(payload[n-markerLen:], redstoneMarker) {
		return nil, fmt.Errorf("%w: trailing marker mismatch", ErrMalformedRedstonePayload)
	}
	metaSize := int(be24(payload[n-markerLen-metaSizeLen : n-markerLen]))
	cntOff := n - markerLen - metaSizeLen - metaSize - pkgCountLen
	if cntOff < 0 {
		return nil, fmt.Errorf("%w: unsigned metadata size %d exceeds payload", ErrMalformedRedstonePayload, metaSize)
	}
	pkgCount := int(binary.BigEndian.Uint16(payload[cntOff : cntOff+pkgCountLen]))
	if pkgCount == 0 || pkgCount > maxPayloadPkgs {
		return nil, fmt.Errorf("%w: package count %d", ErrMalformedRedstonePayload, pkgCount)
	}

	out := make(map[string][]payloadPackage)
	end := cntOff
	for p := 0; p < pkgCount; p++ {
		trailer := signatureLen + dpCountLen + valueSizeLen + timestampLen
		if end < trailer {
			return nil, fmt.Errorf("%w: package %d trailer overruns start", ErrMalformedRedstonePayload, p)
		}
		dpCountOff := end - signatureLen - dpCountLen
		dpCount := int(be24(payload[dpCountOff : dpCountOff+dpCountLen]))
		vsOff := dpCountOff - valueSizeLen
		valueSize := int(binary.BigEndian.Uint32(payload[vsOff : vsOff+valueSizeLen]))
		tsOff := vsOff - timestampLen
		ts := be48(payload[tsOff : tsOff+timestampLen])
		if dpCount == 0 || dpCount > maxPkgDataPts || valueSize == 0 || valueSize > maxPayloadValue {
			return nil, fmt.Errorf("%w: package %d dpCount=%d valueSize=%d", ErrMalformedRedstonePayload, p, dpCount, valueSize)
		}
		dpLen := feedIDLen + valueSize
		dpStart := tsOff - dpCount*dpLen
		if dpStart < 0 {
			return nil, fmt.Errorf("%w: package %d data points overrun start", ErrMalformedRedstonePayload, p)
		}
		for d := 0; d < dpCount; d++ {
			o := dpStart + d*dpLen
			feed := string(bytes.TrimRight(payload[o:o+feedIDLen], "\x00"))
			if feed == "" {
				return nil, fmt.Errorf("%w: package %d data point %d has empty feed id", ErrMalformedRedstonePayload, p, d)
			}
			out[feed] = append(out[feed], payloadPackage{
				Value:       new(big.Int).SetBytes(payload[o+feedIDLen : o+feedIDLen+valueSize]),
				TimestampMS: ts,
			})
		}
		end = dpStart
	}
	return out, nil
}

// medianAt returns the adapter-equivalent aggregate of pkgs' values whose
// timestamp equals tsMS: the exact middle element for an odd count, the
// truncated mean of the two middle elements for an even count. ok=false
// when no package matches the timestamp. The even-count rule matching the
// on-chain adapter is unverified against a real even-signer event — if it
// ever disagrees, the price simply matches NO candidate and the event
// stays in the honest-blind class (conservative by construction, never a
// misattribution).
func medianAt(pkgs []payloadPackage, tsMS uint64) (*big.Int, bool) {
	vals := make([]*big.Int, 0, len(pkgs))
	for _, p := range pkgs {
		if p.TimestampMS == tsMS {
			vals = append(vals, p.Value)
		}
	}
	if len(vals) == 0 {
		return nil, false
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i].Cmp(vals[j]) < 0 })
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		return vals[mid], true
	}
	sum := new(big.Int).Add(vals[mid-1], vals[mid])
	return sum.Rsh(sum, 1), true
}

// attributeSubset maps each surviving updated_feeds entry to exactly one
// of the op-args feed_ids by matching the entry's price against each
// candidate feed's payload median at the entry's package_timestamp.
// Returns one feed id per price, positionally aligned with prices.
//
// Refusal cases (error → the event stays honest-blind):
//   - a price matches zero candidates (unknown aggregation rule or a
//     feed outside the payload), or
//   - a price matches MORE than one candidate (two feeds with identical
//     medians — e.g. two USD-stables pinned to 1e8), or
//   - two prices resolve to the same feed (bijection violation).
func attributeSubset(prices []priceDataDecoded, feedIDs []string, payload []byte) ([]string, error) {
	byFeed, err := parsePayload(payload)
	if err != nil {
		return nil, err
	}
	assigned := make([]string, len(prices))
	used := make(map[string]bool, len(prices))
	for i, pd := range prices {
		match := ""
		for _, feed := range feedIDs {
			m, ok := medianAt(byFeed[feed], pd.PackageTimestamp)
			if !ok || m.Cmp(pd.Price.BigInt()) != 0 {
				continue
			}
			if match != "" {
				return nil, fmt.Errorf("%w: updated_feeds[%d] price matches both %q and %q",
					ErrAmbiguousSubset, i, match, feed)
			}
			match = feed
		}
		if match == "" {
			return nil, fmt.Errorf("%w: updated_feeds[%d] price matches no candidate feed", ErrAmbiguousSubset, i)
		}
		if used[match] {
			return nil, fmt.Errorf("%w: feed %q matched twice", ErrAmbiguousSubset, match)
		}
		used[match] = true
		assigned[i] = match
	}
	return assigned, nil
}

// ErrAmbiguousSubset is returned when a subset-filtered batch cannot be
// attributed uniquely from its payload. Deliberately an ERROR, not a
// skip: the completeness verifier must keep counting these ledgers as
// blind rather than certifying a range where prices exist but could not
// be safely assigned to assets.
var ErrAmbiguousSubset = errors.New("redstone: subset-filtered batch attribution ambiguous")

// be24 / be48 read big-endian 3- and 6-byte unsigned integers.
func be24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func be48(b []byte) uint64 {
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 |
		uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
}
