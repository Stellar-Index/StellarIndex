package redstone

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// The F1 compound (payload.go's F1 CAVEAT) driven through
// decodeWritePrices on the payload-median FALLBACK path. feed_ids are
// [BTC, ETH, USDC, XLM]; the adapter accepted BTC and XLM and dropped
// ETH and USDC, so updated_feeds carries two prices:
//
//  1. signer-filter divergence — XLM's trusted signer stored 2_000_000,
//     but the payload also carries two non-trusted XLM packages at
//     9_000_000 that the adapter filtered out and this parser does not,
//     so our XLM median is 9_000_000;
//  2. cross-feed collision — the DROPPED feed ETH's payload median is
//     2_000_000, byte-equal to XLM's stored price;
//  3. order-preserving position — ETH sits between BTC and XLM.
//
// The unique order-preserving alignment is therefore [BTC, ETH]: XLM's
// price under ETH's feed_id.
const f1PackageTs = uint64(1_745_000_000_000)

func f1FallbackEvent(t *testing.T, stateWriteFeeds ...string) *events.Event {
	t.Helper()
	feedIDs := []string{"BTC", "ETH", "USDC", "XLM"}
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1_000_000), big.NewInt(2_000_000)}, f1PackageTs, f1PackageTs+60_000)
	payload := buildTestPayload(t, f1PackageTs, map[string][]int64{
		"BTC":  {1_000_000},
		"ETH":  {2_000_000},
		"USDC": {7_000_000},
		"XLM":  {2_000_000, 9_000_000, 9_000_000},
	})
	ev := &events.Event{
		Topic:      []string{TopicSymbolRedstone},
		Value:      body,
		ContractID: adapterC,
		Ledger:     52_000_000,
		TxHash:     "f1-fallback",
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArgBytes(t, payload),
		},
	}
	for _, f := range stateWriteFeeds {
		ev.StateWriteKeys = append(ev.StateWriteKeys, adapterFeedKey(t, f))
	}
	return ev
}

// The two ways the dispatcher's enrichment degrades to the fallback
// while still plumbing keys (internal/dispatcher/state_write_keys.go):
// a restored-then-rewritten-unchanged entry of a DROPPED feed counts as
// changed (written set too large), and a per-key parse/marshal failure
// excludes an ACCEPTED feed (written set too small). Either way the
// subset's arity disagrees and the payload alignment runs — and it must
// not publish a price under a feed the state writes show was not
// accepted.
func TestDecode_F1Fallback_StateWritesRefuseDroppedFeed(t *testing.T) {
	cases := map[string][]string{
		"restored dropped feed inflates written set": {"BTC", "USDC", "XLM"},
		"excluded accepted key shrinks written set":  {"BTC"},
	}
	for name, written := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := decodeWritePrices(f1FallbackEvent(t, written...), time.Unix(1_745_000_000, 0))
			if !errors.Is(err, ErrStateWriteFeedMismatch) || !errors.Is(err, ErrFeedIDCountMismatch) {
				assets := make([]string, len(out))
				for i, u := range out {
					assets[i] = u.Asset.String()
				}
				t.Fatalf("err = %v (updates %v), want ErrFeedIDCountMismatch wrapping ErrStateWriteFeedMismatch: "+
					"ETH's stored PriceData did not change, so the fallback's [BTC ETH] alignment is a misattribution", err, assets)
			}
		})
	}
}

// The corroboration only removes attributions: when the payload medians
// are honest, the same inflated written set still lets the fallback
// attribute the accepted pair.
func TestDecode_F1Fallback_CorroboratedAlignmentStillAttributes(t *testing.T) {
	ev := f1FallbackEvent(t, "BTC", "USDC", "XLM")
	ev.OpArgs[2] = encodePayloadArgBytes(t, buildTestPayload(t, f1PackageTs, map[string][]int64{
		"BTC":  {1_000_000},
		"ETH":  {5_000_000},
		"USDC": {7_000_000},
		"XLM":  {2_000_000},
	}))
	out, err := decodeWritePrices(ev, time.Unix(1_745_000_000, 0))
	if err != nil {
		t.Fatalf("decode: %v, want the fallback to attribute BTC and XLM", err)
	}
	if len(out) != 2 || out[0].Asset.String() != "crypto:BTC" || out[1].Asset.String() != "crypto:XLM" {
		t.Fatalf("got %d updates %v, want crypto:BTC then crypto:XLM", len(out), out)
	}
	if out[1].Price.String() != "2000000" {
		t.Fatalf("XLM price = %s, want 2000000", out[1].Price)
	}
}

// With NO state-write keys (events outside every registered decoder's
// contract set, stellar-rpc fixtures, pre-plumb stored events) nothing
// can contradict the alignment, and the F1 compound still misattributes.
// This pins the residual payload.go's F1 CAVEAT documents: a change that
// closes it must fail here and update that caveat with it.
func TestDecode_F1Fallback_NoStateWriteKeys_ResidualMisattribution(t *testing.T) {
	out, err := decodeWritePrices(f1FallbackEvent(t), time.Unix(1_745_000_000, 0))
	if err != nil {
		t.Fatalf("decode: %v — the F1 residual no longer reproduces; update payload.go's F1 CAVEAT", err)
	}
	if len(out) != 2 || out[1].Asset.String() != "crypto:ETH" || out[1].Price.String() != "2000000" {
		t.Fatalf("got %v, want the documented residual: XLM's 2000000 published as crypto:ETH", out)
	}
}
