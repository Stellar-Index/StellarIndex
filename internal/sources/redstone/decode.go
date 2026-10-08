package redstone

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// opIndexFanoutStride spaces the synthetic op_index values of one batch (at most one entry per
// feed, a few dozen today); 1024 holds the feed set with headroom.
const opIndexFanoutStride = 1024

// eventFanoutStride bounds the contract events ONE operation can emit before their OpIndex blocks
// collide: OperationIndex alone collides two events in one op, EventIndex alone (per-operation) collides
// two ops. Same rationale and bound arithmetic as internal/sources/reflector/decode.go.
const eventFanoutStride = 64

// opIndexFanoutMax bounds e.OperationIndex so the packed op_index stays within uint32:
// 2^32 / (64*1024) = 65536.
const opIndexFanoutMax = (1 << 32) / (eventFanoutStride * opIndexFanoutStride)

// classify reports whether this is a Redstone "REDSTONE" event.
// topic[0] is byte-compared against the pre-encoded constant.
func classify(e *events.Event) bool {
	if len(e.Topic) < 1 {
		return false
	}
	return e.Topic[0] == TopicSymbolRedstone
}

// decodeWritePrices converts one REDSTONE event into one canonical.OracleUpdate per (feed_id, price),
// each with an OpIndex from its vector position. The body is Map{updater, updated_feeds: Vec<PriceData>};
// feed_ids are not in it but come from write_prices' args via events.Event.OpArgs.
func decodeWritePrices(e *events.Event, closedAt time.Time) ([]canonical.OracleUpdate, error) {
	if !classify(e) {
		return nil, ErrNotRedstoneEvent
	}

	// Body before op args: an empty batch is a no-op whether or not args were captured (often they
	// are not); checking it below the OpArgs gate left 1,624 ledgers "undecodable-but-matched".
	prices, bodyUpdater, err := sdkDecodeBody(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	if len(prices) == 0 {
		// A push whose freshness verifier dropped every feed still emits `{updated_feeds: [], updater}`
		// (~1.5% of all REDSTONE events). As errors they held redstone at projection_ok=false; a recognised
		// no-op projects zero rows and reconciles.
		return nil, nil
	}

	// Non-empty batch: feed attribution requires the producing op's
	// args (the event body carries no feed_ids).
	if len(e.OpArgs) == 0 {
		return nil, ErrMissingOpArgs
	}
	feedIDs, updater, err := feedIDsFromOpArgs(e.OpArgs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	// Bind the args to THIS event: write_prices publishes its updater argument in the body. A body
	// without it (unknown historical WASM) is tolerated; the current adapter always emits it, so an
	// attacker cannot suppress the check.
	if bodyUpdater != "" && bodyUpdater != updater {
		return nil, fmt.Errorf("%w: body %s, args[0] %s", ErrUpdaterMismatch, bodyUpdater, updater)
	}
	// redstone-core refuses duplicate feeds before the adapter emits (ErrDuplicateFeedIDs); refusing
	// here also removes the duplicate-inflation lever over the subset arity math.
	if dup, has := firstDuplicate(feedIDs); has {
		return nil, fmt.Errorf("%w: %q", ErrDuplicateFeedIDs, dup)
	}

	// Positional zip needs matching arity. A freshness-filtered subset (updated_feeds shorter; 1,626
	// events blind at last verify) is recovered from the op's state writes or the signed payload's
	// medians; anything non-unique refuses the event — better honest-blind than misattributed.
	attributed, err := resolveFeedAttribution(prices, feedIDs, e)
	if err != nil {
		return nil, err
	}
	if err := checkFanoutBounds(e, len(prices)); err != nil {
		return nil, err
	}

	observer := updater // relayer address from op args — strkey form already

	out := make([]canonical.OracleUpdate, 0, len(prices))
	for i, pd := range prices {
		entry, err := resolveFeedEntry(attributed[i])
		if err != nil {
			// Off-registry AND unrepresentable as raw: drop THIS SLOT, not the event. feed_ids are arbitrary
			// ScString, so this is reachable, and one event batches every updated feed, so refusing it would take
			// every feed dark until a code change. Refuse the smallest unusable unit.
			noteUnrepresentableFeed(e, i, attributed[i], err)
			continue
		}
		if pd.Price.Sign() <= 0 {
			// Non-zero by construction; defensive, and guarantees a positive divisor for Invert.
			continue
		}
		price, published, ok := orientedPrice(entry, pd.Price)
		if !ok {
			slog.Warn("redstone: skipping inverted price that rounds to zero",
				"source", SourceName,
				"contract_id", e.ContractID,
				"ledger", e.Ledger,
				"tx_hash", e.TxHash,
				"feed_id", attributed[i],
				"raw_price", pd.Price.String(),
			)
			continue
		}
		u := canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: e.ContractID,
			Ledger:     e.Ledger,
			TxHash:     e.TxHash,
			// Packs (OperationIndex, EventIndex, position); see eventFanoutStride.
			OpIndex: (uint32(e.OperationIndex)*eventFanoutStride+uint32(e.EventIndex))*opIndexFanoutStride + uint32(i),
			// SafeUnixMillis prefers PackageTimestamp but clamps 0 / sentinel / far-future values to the ledger close.
			Timestamp: canonical.SafeUnixMillis(pd.PackageTimestamp, closedAt),
			Asset:     entry.Base,
			// Per-feed quote (ADR-0028): USD, EUR for EUROC/EUR, the reserve ASSET for `_FUNDAMENTAL` NAV
			// ratios. A hardcoded USD quote would mislabel those.
			Quote:          entry.Quote,
			Price:          price,
			PublishedPrice: published,
			Decimals:       DefaultDecimals,
			Observer:       observer,
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		// Only when EVERY entry was non-positive or unrepresentable (unregistered feeds are raw rows). A
		// batch with no row IS undecoded, so honest-blind completeness must count it.
		return nil, ErrEmptyUpdates
	}
	return out, nil
}

// orientedPrice returns the positive price in "<Base> in <Quote>" orientation, reciprocating an
// Invert feed at DefaultDecimals. ok is false when the reciprocal rounds to zero (a price-0 row would
// shadow the last real one). published is the on-chain integer for Invert feeds (lossy, ADR-0003).
func orientedPrice(entry feedEntry, raw canonical.Amount) (price canonical.Amount, published *canonical.Amount, ok bool) {
	if !entry.Invert {
		return raw, nil, true
	}
	inv := reciprocalAtScale(raw, DefaultDecimals)
	onchain := canonical.NewAmount(raw.BigInt())
	return inv, &onchain, inv.Sign() > 0
}

// resolveFeedEntry returns feedID's registry entry, else its raw:<feed_id> entry. Capture-totality:
// skipping unregistered feeds lost ~5,600 events after the ledger-63624934 relayer expansion until a
// replay; a raw row lands now and is promoted in place by a later registry entry. Misses are counted.
func resolveFeedEntry(feedID string) (feedEntry, error) {
	if entry, ok := lookupFeed(feedID); ok {
		return entry, nil
	}
	entry, err := rawFeedEntry(feedID)
	if err != nil {
		return feedEntry{}, err
	}
	obs.SourceUnknownSymbolsTotal.WithLabelValues("redstone").Inc()
	return entry, nil
}

// noteUnrepresentableFeed records a feed_id that is off-registry AND refused by the raw validator.
// The slot is a HOLE, so it has its own counter (SourceUnknownSymbolsTotal means "recorded as raw");
// the fix is a feeds.go entry plus a replay. feed_id is untrusted bytes, logged as a slog ATTRIBUTE so
// both obs.NewLogger handlers escape it (no log injection).
func noteUnrepresentableFeed(e *events.Event, slot int, feedID string, err error) {
	obs.SourceUnrepresentableSymbolsTotal.WithLabelValues(SourceName).Inc()
	slog.Warn("redstone: dropping slot with unrepresentable feed_id",
		"source", SourceName,
		"contract_id", e.ContractID,
		"ledger", e.Ledger,
		"tx_hash", e.TxHash,
		"slot", slot,
		"feed_id", feedID,
		"err", err,
	)
}

// rawFeedEntry builds the record-layer entry for an unregistered feed_id: the verbatim id as an
// AssetOracleRaw base, quoted in its `/<QUOTE>` suffix's fiat if ADR-0010-allowed, else USD (a hint only).
// Invert is never set; a raw row is never compared. Errors only on an unrepresentable id (ScString).
func rawFeedEntry(feedID string) (feedEntry, error) {
	base, err := canonical.NewOracleRawAsset(feedID)
	if err != nil {
		return feedEntry{}, err
	}
	quote := quoteUSD
	if i := strings.LastIndexByte(feedID, '/'); i >= 0 {
		if suffix := feedID[i+1:]; canonical.IsKnownFiat(suffix) {
			q, qerr := canonical.NewFiatAsset(suffix)
			if qerr != nil {
				return feedEntry{}, qerr
			}
			quote = q
		}
	}
	return feedEntry{Base: base, Quote: quote}, nil
}

// ─── SCVal decoders ─────────────────────────────────────────────
// sdkDecodeBody / sdkDecodeFeedIDsFromArg / sdkDecodeAddress are
// called directly below.

// priceDataDecoded mirrors the adapter's PriceData (common/src/lib.rs:12-18). Timestamps are u64 ms;
// OracleUpdate is stamped with package_timestamp (signed off-chain), not write_timestamp.
type priceDataDecoded struct {
	Price            canonical.Amount
	PackageTimestamp uint64
	WriteTimestamp   uint64
}

// sdkDecodeBody decodes the WritePrices event body:
//
//	Map { "updater": Address, "updated_feeds": Vec<PriceData> }
//
// The adapter emits it as ScVal::Bytes wrapping the XDR (`to_xdr().to_val()`), unwrapped once; a bare
// Map also decodes. updater is returned for the body↔args cross-check, "" when absent.
func sdkDecodeBody(valueB64 string) ([]priceDataDecoded, string, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return nil, "", fmt.Errorf("parse body: %w", err)
	}
	// Unwrap the Bytes-wrapped XDR Map if present.
	if raw, bytesErr := scval.AsBytes(body); bytesErr == nil {
		inner, parseErr := scval.ParseBytes(raw)
		if parseErr != nil {
			return nil, "", fmt.Errorf("unwrap Bytes body: %w", parseErr)
		}
		body = inner
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return nil, "", fmt.Errorf("body not a Map: %w", err)
	}
	updsSv, err := scval.MustMapField(entries, "updated_feeds")
	if err != nil {
		return nil, "", fmt.Errorf("body map missing updated_feeds: %w", err)
	}
	items, err := scval.AsVec(updsSv)
	if err != nil {
		return nil, "", fmt.Errorf("updated_feeds not a Vec: %w", err)
	}
	out := make([]priceDataDecoded, 0, len(items))
	for i, item := range items {
		pd, err := decodePriceData(item)
		if err != nil {
			return nil, "", fmt.Errorf("updated_feeds[%d]: %w", i, err)
		}
		out = append(out, pd)
	}
	updater := ""
	if updSv, uerr := scval.MustMapField(entries, "updater"); uerr == nil {
		if s, aerr := sdkDecodeAddress(updSv); aerr == nil {
			updater = s
		}
	}
	return out, updater, nil
}

// decodePriceData decodes one PriceData map entry:
//
//	Map { "price": U256, "package_timestamp": u64, "write_timestamp": u64 }
func decodePriceData(sv scval.ScVal) (priceDataDecoded, error) {
	entries, err := scval.AsMap(sv)
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("PriceData not a Map: %w", err)
	}
	priceSv, err := scval.MustMapField(entries, "price")
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("missing price: %w", err)
	}
	price, err := scval.AsAmountFromU256(priceSv)
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("price: %w", err)
	}
	pkgSv, err := scval.MustMapField(entries, "package_timestamp")
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("missing package_timestamp: %w", err)
	}
	pkg, err := scval.AsU64(pkgSv)
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("package_timestamp: %w", err)
	}
	wrSv, err := scval.MustMapField(entries, "write_timestamp")
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("missing write_timestamp: %w", err)
	}
	wr, err := scval.AsU64(wrSv)
	if err != nil {
		return priceDataDecoded{}, fmt.Errorf("write_timestamp: %w", err)
	}
	return priceDataDecoded{
		Price:            price,
		PackageTimestamp: pkg,
		WriteTimestamp:   wr,
	}, nil
}

// feedIDsFromOpArgs parses write_prices(updater: Address, feed_ids: Vec<String>, payload: Bytes)
// args (adapter/lib.rs:78), requiring arity >= 3. The function name is not plumbed; the [WriteFnName]
// four-layer binding substitutes: the OpArgs provenance gate, this signature check, the updater
// cross-check and state-write corroboration.
func feedIDsFromOpArgs(opArgs []string) (feedIDs []string, updater string, err error) {
	// Args are plumbed only for a direct top-level call into this event's own contract (provenance
	// gate: internal/dispatcher/dispatcher.go and internal/storage/clickhouse/extract.go). A WASM emitting
	// REDSTONE elsewhere is caught by the per-WASM-hash audit gate.
	if len(opArgs) < 3 {
		return nil, "", fmt.Errorf("op args arity %d, want ≥3 (updater, feed_ids, payload)", len(opArgs))
	}
	addrSv, err := scval.Parse(opArgs[0])
	if err != nil {
		return nil, "", fmt.Errorf("args[0] updater: %w", err)
	}
	updater, err = sdkDecodeAddress(addrSv)
	if err != nil {
		return nil, "", fmt.Errorf("args[0] updater: %w", err)
	}
	feedsSv, err := scval.Parse(opArgs[1])
	if err != nil {
		return nil, "", fmt.Errorf("args[1] feed_ids: %w", err)
	}
	feedIDs, err = sdkDecodeFeedIDsFromArg(feedsSv)
	if err != nil {
		return nil, "", fmt.Errorf("args[1] feed_ids: %w", err)
	}
	// args[2] (the signed payload) is read only on the subset path; see payloadFromOpArgs.
	return feedIDs, updater, nil
}

// payloadFromOpArgs extracts the raw RedStone signed payload from
// write_prices' third argument (`payload: Bytes`). Only called on the
// subset-filtered path, where it is the sole source of per-feed identity.
func payloadFromOpArgs(opArgs []string) ([]byte, error) {
	if len(opArgs) < 3 {
		return nil, fmt.Errorf("op args arity %d, want ≥3 (updater, feed_ids, payload)", len(opArgs))
	}
	sv, err := scval.Parse(opArgs[2])
	if err != nil {
		return nil, fmt.Errorf("args[2] payload: %w", err)
	}
	raw, err := scval.AsBytes(sv)
	if err != nil {
		return nil, fmt.Errorf("args[2] payload: %w", err)
	}
	return raw, nil
}

// sdkDecodeFeedIDsFromArg decodes a Vec<String> where each element
// is an ScString holding a feed_id like "BTC".
func sdkDecodeFeedIDsFromArg(sv scval.ScVal) ([]string, error) {
	items, err := scval.AsVec(sv)
	if err != nil {
		return nil, fmt.Errorf("not a Vec: %w", err)
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, err := scval.AsString(it)
		if err != nil {
			return nil, fmt.Errorf("feed_ids[%d]: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// sdkDecodeAddress decodes an Address SCVal to its strkey via scval.AsAddressStrkey.
func sdkDecodeAddress(sv scval.ScVal) (string, error) {
	return scval.AsAddressStrkey(sv)
}

// resolveFeedAttribution maps updated_feeds entries to feed ids. Equal arity zips positionally;
// a LONGER updated_feeds is malformed. A SHORTER (freshness-filtered) one resolves in order:
//
//  1. EXACT — the op's value-changing contract-data writes (events.Event.StateWriteKeys): rejected
//     feeds are rewritten byte-identical, so changed keys ∩ feed_ids, in feed_ids order, IS the
//     accepted subset. This resolves value collisions the median rule cannot (ledger 62056824).
//  2. FALLBACK — payload-median alignment (payload.go) when keys are absent or their arity disagrees;
//     anything non-unique, or naming a feed the writes show unchanged, refuses the event.
//
// Equal arity is CORROBORATED when keys are plumbed: every accepted feed's entry changes, so the
// written set must equal feed_ids. A mismatch falls back to payload alignment and, failing that, is
// ErrStateWriteFeedMismatch, never a guess. Without keys the zip stands: absence is "unknown".
func resolveFeedAttribution(prices []priceDataDecoded, feedIDs []string, e *events.Event) ([]string, error) {
	if len(feedIDs) == len(prices) {
		if len(e.StateWriteKeys) > 0 {
			written := writtenFeedSet(e.StateWriteKeys, e.ContractID)
			if !feedSetEqual(feedIDs, written) {
				if payload, perr := payloadFromOpArgs(e.OpArgs); perr == nil {
					if attributed, aerr := attributeSubset(prices, feedIDs, payload); aerr == nil {
						return attributed, nil
					}
				}
				return nil, fmt.Errorf("%w: %d feed_ids vs %d changed feed keys",
					ErrStateWriteFeedMismatch, len(feedIDs), len(written))
			}
		}
		return feedIDs, nil
	}
	if len(prices) > len(feedIDs) {
		return nil, fmt.Errorf("%w: %d feed_ids, %d updated_feeds",
			ErrFeedIDCountMismatch, len(feedIDs), len(prices))
	}
	if sub := subsetFromStateWrites(feedIDs, e.StateWriteKeys, e.ContractID); len(sub) == len(prices) {
		return sub, nil
	}
	payload, err := payloadFromOpArgs(e.OpArgs)
	if err != nil {
		return nil, fmt.Errorf("%w: %d feed_ids, %d updated_feeds; %w",
			ErrFeedIDCountMismatch, len(feedIDs), len(prices), err)
	}
	attributed, err := attributeSubset(prices, feedIDs, payload)
	if err == nil {
		err = corroborateFallback(attributed, e)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %d feed_ids, %d updated_feeds; %w",
			ErrFeedIDCountMismatch, len(feedIDs), len(prices), err)
	}
	return attributed, nil
}

// corroborateFallback refuses a payload-median alignment naming a feed the op did NOT change (a
// dropped feed is rewritten byte-identical), closing payload.go's F1 compound when the writes name feeds.
// It only turns attributions into refusals; an empty written set proves nothing.
func corroborateFallback(attributed []string, e *events.Event) error {
	written := writtenFeedSet(e.StateWriteKeys, e.ContractID)
	if len(written) == 0 {
		return nil
	}
	for _, f := range attributed {
		if !written[f] {
			return fmt.Errorf("%w: payload alignment names %q, whose stored PriceData did not change",
				ErrStateWriteFeedMismatch, f)
		}
	}
	return nil
}

// subsetFromStateWrites projects feedIDs, in order, onto the feed ids written by contractID (ScString
// keys, re-checked as defence in depth). Unparseable or non-string keys are skipped: the caller falls back
// to payload alignment on any arity disagreement, never a trusted wrong subset. Nil without keys.
func subsetFromStateWrites(feedIDs, stateWriteKeys []string, contractID string) []string {
	if len(stateWriteKeys) == 0 {
		return nil
	}
	written := writtenFeedSet(stateWriteKeys, contractID)
	var sub []string
	for _, f := range feedIDs {
		if written[f] {
			sub = append(sub, f)
			// Count each written feed once so a repeated candidate cannot inflate the arity into a match.
			delete(written, f)
		}
	}
	return sub
}

// writtenFeedSet returns the feed ids contractID wrote (ScString keys); other or unparseable keys
// are skipped, since callers refuse or fall back on any disagreement.
func writtenFeedSet(stateWriteKeys []string, contractID string) map[string]bool {
	written := make(map[string]bool, len(stateWriteKeys))
	for _, kb64 := range stateWriteKeys {
		owner, key, err := scval.ParseContractDataKey(kb64)
		if err != nil || owner != contractID {
			continue
		}
		feed, err := scval.AsString(key)
		if err != nil {
			continue
		}
		written[feed] = true
	}
	return written
}

// feedSetEqual reports whether feedIDs (duplicate-free) and the written
// set contain exactly the same feeds.
func feedSetEqual(feedIDs []string, written map[string]bool) bool {
	if len(written) != len(feedIDs) {
		return false
	}
	for _, f := range feedIDs {
		if !written[f] {
			return false
		}
	}
	return true
}

// firstDuplicate returns the first repeated entry of feedIDs, if any.
func firstDuplicate(feedIDs []string) (string, bool) {
	seen := make(map[string]bool, len(feedIDs))
	for _, f := range feedIDs {
		if seen[f] {
			return f, true
		}
		seen[f] = true
	}
	return "", false
}

// checkFanoutBounds validates the three inputs to the synthetic OpIndex packing so it cannot wrap
// uint32 into another event's block on the oracle_updates PK.
func checkFanoutBounds(e *events.Event, priceCount int) error {
	if priceCount > opIndexFanoutStride {
		return fmt.Errorf("redstone: feed count %d exceeds fanout stride %d",
			priceCount, opIndexFanoutStride)
	}
	if e.EventIndex < 0 || e.EventIndex >= eventFanoutStride {
		// See ErrEventIndexOverflow for rationale.
		return fmt.Errorf("%w: got %d", ErrEventIndexOverflow, e.EventIndex)
	}
	// See ErrOperationIndexOverflow.
	if e.OperationIndex < 0 || e.OperationIndex >= opIndexFanoutMax {
		return fmt.Errorf("%w: got %d", ErrOperationIndexOverflow, e.OperationIndex)
	}
	return nil
}
