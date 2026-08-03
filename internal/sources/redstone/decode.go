package redstone

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// opIndexFanoutStride spaces synthetic op_index values derived from
// a single batch update, same concept as the Reflector decoder.
// Redstone emits at most 30 entries today (19 original + the
// 2026-07-24 expansion); 1024 holds the full feed set with headroom
// for growth and stays inside uint32.
const opIndexFanoutStride = 1024

// eventFanoutStride bounds how many contract events ONE operation can
// emit before their per-event op_index blocks would collide.
// DAT-06/trap-15 (audit-2026-07-23): the fanout base used to be
// OperationIndex ALONE, so two REDSTONE events within the SAME
// operation collided on their whole 1024-wide OpIndex block.
// EventIndex ALONE isn't a safe replacement either: per events.Event's
// own doc it is scoped PER-OPERATION (resets to 0 for each new op), so
// swapping it straight in would instead collide two DIFFERENT
// operations that each emit a single event. Combining both dimensions
// — OperationIndex and EventIndex — keeps every event's OpIndex block
// disjoint regardless of whether the collision risk is same-op or
// cross-op. See the identical rationale + bound arithmetic in
// internal/sources/reflector/decode.go.
const eventFanoutStride = 64

// opIndexFanoutMax bounds e.OperationIndex so the packed op_index
// (OperationIndex*eventFanoutStride+EventIndex)*opIndexFanoutStride+i
// stays within uint32: 2^32 / (eventFanoutStride * opIndexFanoutStride)
// = 2^32 / (64*1024) = 65536.
const opIndexFanoutMax = (1 << 32) / (eventFanoutStride * opIndexFanoutStride)

// classify reports whether this is a Redstone "REDSTONE" event.
// topic[0] is byte-compared against the pre-encoded constant.
func classify(e *events.Event) bool {
	if len(e.Topic) < 1 {
		return false
	}
	return e.Topic[0] == TopicSymbolRedstone
}

// decodeWritePrices converts one REDSTONE event into a slice of
// canonical.OracleUpdate — one per (feed_id, price) pair.
//
// Topic arity: 1 (REDSTONE only). Body: Map{updater, updated_feeds:
// Vec<PriceData>}. feed_ids are NOT in the body; they come from the
// tx envelope's write_prices(feed_ids, …) call and are passed in via
// events.Event.OpArgs, populated by internal/dispatcher.
//
// Each OracleUpdate shares (ledger, tx_hash, source) but gets a
// distinct OpIndex derived from the vector position so identity
// stays unique in oracle_updates.
func decodeWritePrices(e *events.Event, closedAt time.Time) ([]canonical.OracleUpdate, error) {
	if !classify(e) {
		return nil, ErrNotRedstoneEvent
	}

	// Decode the BODY before requiring op args. An empty on-wire batch
	// (`{updated_feeds: [], updater}`) carries nothing to attribute, so
	// it must classify as a no-op REGARDLESS of whether the producing
	// op's args were captured — and in practice those pushes often lack
	// usable args, which is exactly why the 2026-07-29 replay left
	// 1,624 ledgers "undecodable-but-matched" even after the first
	// empty-batch fix landed BELOW the OpArgs gate. Order matters:
	// body → empty short-circuit → args for the non-empty path only.
	prices, bodyUpdater, err := sdkDecodeBody(e.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	if len(prices) == 0 {
		// Empty ON-WIRE batch: a write_prices push whose freshness
		// verifier dropped every candidate feed still emits
		// `{updated_feeds: [], updater}`. The old assumption that "the
		// adapter only emits when at least one feed passes" is
		// disproven by the lake — ~1.5% of ALL REDSTONE events ever
		// (small ~156-byte bodies, every ledger band since source
		// genesis) are this shape, and treating them as errors is what
		// kept redstone `projection_ok=false` under the v0.21.2
		// honest-blind completeness accounting (866 "undecodable"
		// ledgers, root-caused 2026-07-29). A recognized no-op projects
		// zero rows and reconciles; it is not an error.
		return nil, nil
	}

	// Non-empty batch: feed attribution requires the producing op's
	// args (the event body carries no feed_ids) — from here on the
	// original requirements apply unchanged.
	if len(e.OpArgs) == 0 {
		return nil, ErrMissingOpArgs
	}
	feedIDs, updater, err := feedIDsFromOpArgs(e.OpArgs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedPayload, err)
	}
	// Bind the args to THIS event: write_prices publishes its own
	// updater argument in the event body, so body.updater must equal
	// args[0]. A body without the field (unknown historical WASM
	// shape) is tolerated — the check can't be attacker-suppressed,
	// because the current adapter always emits it.
	if bodyUpdater != "" && bodyUpdater != updater {
		return nil, fmt.Errorf("%w: body %s, args[0] %s", ErrUpdaterMismatch, bodyUpdater, updater)
	}
	// A genuine write_prices carries no duplicate feeds (the
	// redstone-core SDK refuses them before the adapter can emit —
	// see ErrDuplicateFeedIDs). Refusing here also removes the
	// duplicate-inflation lever over the subset arity math below.
	if dup, has := firstDuplicate(feedIDs); has {
		return nil, fmt.Errorf("%w: %q", ErrDuplicateFeedIDs, dup)
	}

	// Positional zip requires matching arity. When the adapter's
	// freshness verifier dropped ≥1 feed (updated_feeds SHORTER than
	// feed_ids — a real, ongoing class: 1,626 events blind on the
	// 2026-07-29 full verify, still occurring at tip), the accepted
	// subset is recovered EXACTLY from the op's value-changing
	// contract-data writes when available (only accepted feeds' stored
	// PriceData changes — events.Event.StateWriteKeys), falling back
	// to the signed payload in args[2]: the adapter
	// stores each accepted feed's signer-value MEDIAN, so a surviving
	// price must equal exactly one candidate feed's payload median at
	// its package_timestamp (verified byte-exact on the
	// ledger-59258375 event). Anything non-unique refuses the whole
	// event — better honest-blind than misattributed.
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
		entry, ok := lookupFeed(attributed[i])
		if !ok {
			// Partial-event skip: land every feed in the ADR-0028
			// registry, drop the rest. A miss means RedStone deployed
			// a feed beyond the registered set — count it so
			// operators get a signal (F-1234, codex audit-2026-05-12)
			// rather than blocking the whole batch. The 2026-07-24
			// relayer expansion (11 new feed_ids, ledger 63624934) hit
			// exactly this path — plus ErrEmptyUpdates for all-new
			// batches — for ~5,600 events until the registry caught up.
			obs.SourceUnknownSymbolsTotal.WithLabelValues("redstone").Inc()
			continue
		}
		if pd.Price.Sign() <= 0 {
			// Redstone publishes non-zero prices by construction —
			// defensive skip in case a contract upgrade relaxes that.
			// Also guarantees a positive divisor for the Invert path.
			continue
		}
		price := pd.Price
		if entry.Invert {
			// Feed published in market-FX orientation (units-per-USD);
			// reciprocate to our canonical "<Base> in USD" convention
			// so the row is comparable to every other feed. See
			// feedEntry.Invert + reciprocalAtScale.
			price = reciprocalAtScale(price, DefaultDecimals)
		}
		u := canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: e.ContractID,
			Ledger:     e.Ledger,
			TxHash:     e.TxHash,
			// OpIndex packs (OperationIndex, EventIndex, vector
			// position) so two Redstone events anywhere in the tx —
			// same operation or different — can't collide on the
			// oracle_updates PK. See eventFanoutStride's godoc.
			OpIndex: (uint32(e.OperationIndex)*eventFanoutStride+uint32(e.EventIndex))*opIndexFanoutStride + uint32(i),
			// canonical.SafeUnixMillis prefers the contract-supplied
			// PackageTimestamp but clamps 0 / sentinel / far-future
			// garbage (incl. the >MaxInt64 wrap class of the router
			// deadline_ts overflow) to the ledger close time.
			Timestamp: canonical.SafeUnixMillis(pd.PackageTimestamp, closedAt),
			Asset:     entry.Base,
			// Per-feed quote — USD for all but EUROC/EUR (ADR-0028).
			// Pre-#53 this was hardcoded USD, mislabelling EUROC.
			Quote:    entry.Quote,
			Price:    price,
			Decimals: DefaultDecimals,
			Observer: observer,
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		// Every entry was unknown — treat like Reflector's all-skipped
		// case. Surfaces to the dispatcher as a decode error counter
		// bump so an all-RWA batch is visible in metrics.
		return nil, ErrEmptyUpdates
	}
	return out, nil
}

// ─── SCVal decoders ─────────────────────────────────────────────
// sdkDecodeBody / sdkDecodeFeedIDsFromArg / sdkDecodeAddress are
// called directly below. (A package-var indirection seam used to sit
// here for test fixture-swapping; removed as unused — no test ever
// swapped them.)

// priceDataDecoded mirrors the adapter's PriceData struct
// (common/src/lib.rs:12-18) at the canonical-types boundary. The
// timestamps are `u64` ms on the wire — package_timestamp is when
// RedStone signed the payload off-chain, write_timestamp is when it
// landed on-chain. We stamp package_timestamp on OracleUpdate
// (the oracle's published time, not the block close time) — matches
// the OracleUpdate contract in canonical/oracle.go.
type priceDataDecoded struct {
	Price            canonical.Amount
	PackageTimestamp uint64
	WriteTimestamp   uint64
}

// sdkDecodeBody decodes the WritePrices event body:
//
//	Map { "updater": Address, "updated_feeds": Vec<PriceData> }
//
// On the wire the Rust adapter contract (redstone-adapter's
// event.rs) emits the body as `ScVal::Bytes` wrapping the XDR-
// encoded struct: `self.to_xdr(env).to_val()`. We unwrap that Bytes
// layer once, then re-parse as ScVal::Map below. If we ever see a
// body that's already a Map (e.g. a future contract upgrade that
// drops the custom to_xdr), the fallback path still works.
//
// The op args stay authoritative for OBSERVER attribution (they carry
// the full strkey regardless of muxed variants), but the body's own
// `updater` field is ALSO returned so decodeWritePrices can enforce
// the body↔args agreement the contract guarantees (write_prices
// publishes its own updater argument) — the cross-check that binds a
// set of attached args to the event they claim to describe. updater is
// "" when the body carries no decodable field (tolerated: an unknown
// historical WASM shape; the current adapter always emits it, so an
// attacker cannot suppress the check).
func sdkDecodeBody(valueB64 string) ([]priceDataDecoded, string, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return nil, "", fmt.Errorf("parse body: %w", err)
	}
	// Unwrap the Bytes-wrapped XDR payload if present. The adapter
	// uses `to_xdr().to_val()` which produces ScVal::Bytes holding
	// the XDR-encoded Map; re-parsing those bytes yields the Map
	// shape our existing downstream logic already handles.
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

// feedIDsFromOpArgs parses the dispatcher-supplied InvokeContract
// args and returns the feed_ids + updater strkey. Argument layout
// per adapter/lib.rs:78:
//
//	write_prices(updater: Address, feed_ids: Vec<String>, payload: Bytes)
//
// We enforce arity ≥ 3 (extra args from a contract upgrade would
// surface here). We do NOT verify the function name was write_prices
// — the dispatcher only plumbs the Args slice, not the function name.
// What substitutes for it is the four-layer binding documented at
// [WriteFnName]: the dispatcher/lake OpArgs PROVENANCE gate (args are
// attached ONLY when the op's invoked contract IS the event's own
// contract — a wrapper-invoked adapter event arrives with no args and
// refuses via ErrMissingOpArgs), this function's structural signature
// check, the body↔args updater cross-check, and the state-write feed
// corroboration.
func feedIDsFromOpArgs(opArgs []string) (feedIDs []string, updater string, err error) {
	// The InvokeContract wire layout stores the function name OUTSIDE
	// the Args slice (it lives alongside them in InvokeContractArgs).
	// The dispatcher plumbs only Args, but ONLY for a direct top-level
	// call into this event's own contract (the provenance gate,
	// internal/dispatcher/dispatcher.go + the lake twin in
	// internal/storage/clickhouse/extract.go). The adapter only emits
	// REDSTONE from write_prices; a future WASM that emits it from
	// another entry point is covered by
	// docs/architecture/contract-schema-evolution.md's per-WASM-hash
	// audit gate.
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
	// args[2] is the signed payload bytes — needed only on the
	// subset-filtered path; see payloadFromOpArgs.
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

// sdkDecodeAddress decodes an Address SCVal to its G-strkey form.
// Delegates to scval.AsAddressStrkey which owns the strkey
// formatting for all address types.
func sdkDecodeAddress(sv scval.ScVal) (string, error) {
	return scval.AsAddressStrkey(sv)
}

// resolveFeedAttribution maps updated_feeds entries to feed ids. Equal
// arity zips positionally (the common case). A SHORTER updated_feeds is
// the freshness-filtered subset class, resolved in preference order:
//
//  1. EXACT — the operation's VALUE-CHANGING contract-data write keys
//     (events.Event.StateWriteKeys): write_prices stores each feed's
//     PriceData under ScString(feed_id) and rewrites REJECTED feeds
//     byte-identical, so only ACCEPTED feeds' entries change (r1 ground
//     truth, ledger 62056824) — the changed keys ∩ feed_ids (in
//     feed_ids order — updated_feeds is built in a single pass over
//     feed_ids) IS the accepted subset. Zero heuristics; used whenever
//     the plumbed keys yield a subset of matching arity. This is what
//     resolves the value-collision residue the median rule provably
//     cannot (ledger 62056824: one surviving price 1.00000000 matching
//     both BENJI_ETHEREUM_FUNDAMENTAL twins).
//  2. FALLBACK — payload-median alignment (see payload.go), for events
//     whose reader did not plumb state-write keys (stellar-rpc fixture
//     captures, pre-plumb stored events) or where the changed-key
//     subset's arity disagrees with updated_feeds (e.g. a restored
//     entry with no visible pre-image, or a storage-shape change —
//     fall back rather than trust it). Anything non-unique refuses the
//     whole event.
//
// A LONGER updated_feeds cannot come from freshness filtering and is
// refused as genuinely malformed.
//
// EQUAL arity is additionally CORROBORATED against the state writes
// when they are plumbed (2026-07-31 hardening): equal arity means the
// adapter accepted every requested feed, and an accepted feed's stored
// PriceData always changes — so the op's value-changed feed-keyed
// writes must equal the feed_ids set exactly (order-insensitive;
// feed_ids are duplicate-free by the decodeWritePrices gate). A
// mismatch refuses the whole event (ErrStateWriteFeedMismatch): the
// claimed names are uncorroborated by what the contract actually
// stored, and a positional zip onto them would be exactly the
// misattribution this file exists to prevent. When no keys were
// plumbed (stellar-rpc fixtures, non-opted readers) the positional zip
// stands alone, as before — absence is "unknown", not "no writes".
func resolveFeedAttribution(prices []priceDataDecoded, feedIDs []string, e *events.Event) ([]string, error) {
	if len(feedIDs) == len(prices) {
		if len(e.StateWriteKeys) > 0 {
			written := writtenFeedSet(e.StateWriteKeys, e.ContractID)
			if !feedSetEqual(feedIDs, written) {
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
	if err != nil {
		return nil, fmt.Errorf("%w: %d feed_ids, %d updated_feeds; %w",
			ErrFeedIDCountMismatch, len(feedIDs), len(prices), err)
	}
	return attributed, nil
}

// subsetFromStateWrites derives the accepted-feed subset from the
// operation's value-changing contract-data write keys: parse each
// plumbed LedgerKey,
// keep ScString keys owned by the event's own contract (the plumbing
// already filters by contract; re-checking here is defence-in-depth
// against a future reader that forgets), collect the written feed-id
// set, and project feedIDs onto it PRESERVING feed_ids order — the
// same order updated_feeds is built in. Non-string keys (any other
// adapter storage) and unparseable keys are skipped, not fatal: the
// caller compares the subset's arity against updated_feeds and falls
// back to payload alignment on any disagreement, so a partial read can
// only cause a fallback, never a misattribution. Nil when no keys were
// plumbed.
func subsetFromStateWrites(feedIDs, stateWriteKeys []string, contractID string) []string {
	if len(stateWriteKeys) == 0 {
		return nil
	}
	written := writtenFeedSet(stateWriteKeys, contractID)
	var sub []string
	for _, f := range feedIDs {
		if written[f] {
			sub = append(sub, f)
			// Count each WRITTEN feed once (audit F2): feed_ids are
			// duplicate-free by the decodeWritePrices gate, but this
			// projection must never let a repeated candidate inflate
			// the subset's arity into a spurious match — belt to that
			// gate's braces.
			delete(written, f)
		}
	}
	return sub
}

// writtenFeedSet parses the plumbed value-changed contract-data keys
// and returns the SET of feed ids written by the event's own contract:
// ScString keys owned by contractID. Non-string keys (any other
// adapter storage) and unparseable keys are skipped, not fatal — the
// callers compare arity/set membership and refuse or fall back on any
// disagreement, so a partial read can only cause a refusal/fallback,
// never a misattribution.
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

// checkFanoutBounds validates the three inputs to the synthetic OpIndex
// packing (OperationIndex*eventFanoutStride+EventIndex)*opIndexFanoutStride+i
// so the packed value stays within uint32 — a bad input would wrap and
// overlap another event's op_index block on the oracle_updates PK.
// Extracted from decodeWritePrices to keep it under the gocognit ceiling.
func checkFanoutBounds(e *events.Event, priceCount int) error {
	if priceCount > opIndexFanoutStride {
		return fmt.Errorf("redstone: feed count %d exceeds fanout stride %d",
			priceCount, opIndexFanoutStride)
	}
	if e.EventIndex < 0 || e.EventIndex >= eventFanoutStride {
		// See ErrEventIndexOverflow for rationale.
		return fmt.Errorf("%w: got %d", ErrEventIndexOverflow, e.EventIndex)
	}
	// OperationIndex is the third input to the packing and was the one left
	// unguarded next to its bounded siblings (EventIndex, vector position).
	// See ErrOperationIndexOverflow.
	if e.OperationIndex < 0 || e.OperationIndex >= opIndexFanoutMax {
		return fmt.Errorf("%w: got %d", ErrOperationIndexOverflow, e.OperationIndex)
	}
	return nil
}
