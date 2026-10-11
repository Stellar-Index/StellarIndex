package redstone

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// This file parses the RedStone signed payload (write_prices' third argument) to recover per-feed
// signer values and timestamps for SUBSET-FILTERED batches (updated_feeds shorter than feed_ids).
// The adapter stores each feed's signer MEDIAN, so a surviving price must equal a unique
// candidate's median at its package_timestamp; anything ambiguous refuses the event.
//
// Wire layout (big-endian, parsed from the END):
//
//	package set: [dataPackage 1]…[dataPackage N] [packagesCount 2B]
//	             [unsignedMetadataSize 3B] [unsignedMetadata] [redstoneMarker 9B]
//	dataPackage: [dataPoint 1]…[dataPoint M] [timestampMS 6B]
//	             [valueByteSize 4B] [dataPointsCount 3B] [signature 65B]
//	dataPoint:   [feedID 32B zero-right-padded] [value valueByteSize B]
//
// Signatures are NOT verified (the event proves the adapter accepted the payload). Without signer
// filtering every package is aggregated, so medians can disagree with the adapter's.
//
// F1 CAVEAT (fallback path only): it misattributes only when signer-filter divergence hits a
// surviving feed and a dropped feed's median equals that price (the BENJI twins in decode_test.go).
// corroborateFallback refuses it when state writes name the op's feeds.
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

// ErrMalformedRedstonePayload wraps every structural failure, one "payload unparseable" refusal class.
var ErrMalformedRedstonePayload = errors.New("redstone: malformed payload")

// payloadPackage is one signer's value for one feed at a batch timestamp.
type payloadPackage struct {
	Value       *big.Int
	TimestampMS uint64
}

// parsePayload recovers feed → signer packages. Any inconsistency returns
// [ErrMalformedRedstonePayload], never a partial result: a dropped feed could attribute a price to the
// wrong asset, the failure this parser exists to prevent.
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

// medianAt returns the adapter-equivalent aggregate of pkgs' values at tsMS: the middle element, or
// floor((a+b)/2) for an even count (verified against redstone-rust-sdk's Avg; not a divergence source).
// ok=false when none matches. Signer filtering can still diverge; see the F1 caveat above.
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

// attributeSubset maps each surviving updated_feeds entry to one feed_id whose payload median equals
// its price, ORDER-PRESERVING: updated_feeds is a subsequence of feed_ids, so the constraint only
// disambiguates (it resolved 170 ledgers from 60104689 where a price matched two medians). A DP counts
// alignments; zero or more than one refuses, leaving the event honest-blind.
func attributeSubset(prices []priceDataDecoded, feedIDs []string, payload []byte) ([]string, error) {
	byFeed, err := parsePayload(payload)
	if err != nil {
		return nil, err
	}
	n, m := len(prices), len(feedIDs)
	match := alignmentMatches(prices, feedIDs, byFeed)
	ways := alignmentWays(match, n, m)
	switch ways[0][0] {
	case 0:
		return nil, fmt.Errorf("%w: no order-preserving alignment of %d prices onto %d feed_ids",
			ErrAmbiguousSubset, n, m)
	case 1:
		// Unique: walk the DP. ways[i][j] == 1 = take + skip along the walk, so exactly one branch is live.
		assigned := make([]string, n)
		i, j := 0, 0
		for i < n {
			if j >= m {
				return nil, fmt.Errorf("%w: alignment walk overran candidates", ErrAmbiguousSubset)
			}
			take := 0
			if match[i][j] {
				take = ways[i+1][j+1]
			}
			switch {
			case take > 0 && ways[i][j+1] == 0:
				assigned[i] = feedIDs[j]
				i, j = i+1, j+1
			case take == 0 && ways[i][j+1] > 0:
				j++
			default:
				// Unreachable when ways[0][0]==1; refuse rather than guess.
				return nil, fmt.Errorf("%w: alignment walk lost uniqueness", ErrAmbiguousSubset)
			}
		}
		return assigned, nil
	default:
		return nil, fmt.Errorf("%w: %d order-preserving alignments of %d prices onto %d feed_ids",
			ErrAmbiguousSubset, ways[0][0], n, m)
	}
}

// ErrAmbiguousSubset — a subset-filtered batch could not be attributed uniquely. An ERROR, not a
// skip, so the completeness verifier keeps counting the ledger as blind.
var ErrAmbiguousSubset = errors.New("redstone: subset-filtered batch attribution ambiguous")

// be24 / be48 read big-endian 3- and 6-byte unsigned integers.
func be24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func be48(b []byte) uint64 {
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 |
		uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
}

// alignmentMatches builds match[i][j]: feedIDs[j]'s median at prices[i]'s package_timestamp equals its price.
func alignmentMatches(prices []priceDataDecoded, feedIDs []string, byFeed map[string][]payloadPackage) [][]bool {
	match := make([][]bool, len(prices))
	for i, pd := range prices {
		match[i] = make([]bool, len(feedIDs))
		for j, feed := range feedIDs {
			mv, ok := medianAt(byFeed[feed], pd.PackageTimestamp)
			match[i][j] = ok && mv.Cmp(pd.Price.BigInt()) == 0
		}
	}
	return match
}

// alignmentWays counts order-preserving alignments of prices[i:] onto feedIDs[j:], capped at 2.
func alignmentWays(match [][]bool, n, m int) [][]int {
	ways := make([][]int, n+1)
	for i := range ways {
		ways[i] = make([]int, m+1)
	}
	for j := 0; j <= m; j++ {
		ways[n][j] = 1 // no prices left: exactly one (empty) alignment
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			w := ways[i][j+1] // skip feedIDs[j] (freshness-dropped)
			if match[i][j] {
				w += ways[i+1][j+1] // assign prices[i] = feedIDs[j]
			}
			if w > 2 {
				w = 2
			}
			ways[i][j] = w
		}
	}
	return ways
}
