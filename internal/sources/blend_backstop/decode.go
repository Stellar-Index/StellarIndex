// Package blend_backstop decodes Blend's Backstop contract events on Soroban,
// a SEPARATE event surface from the Blend pool decoder (internal/sources/blend).
// Do NOT fold it into that package; they share neither contract addresses nor
// event vocabulary.
//
// The backstop is the insurance module (BLND:USDC LP stakers earn emissions and
// absorb bad debt). 12 event types (topic[0] = Symbol); `rw_zone` is the V1
// spelling of rw_zone_add and rw_zone_remove is V2 only. Field layouts come from
// mainnet lake samples (golden frames in decode_test.go). Easy to get wrong:
//
//  1. V1 gulp_emissions has 1 topic (no pool) and a BARE i128 body; requiring
//     the V2 shape would error all 209 V1 rows. V2 has the pool topic and a
//     2-i128 body.
//  2. The V1 reward-zone topic is literally `rw_zone` with no Option wrapper;
//     knowing only `rw_zone_add` drops its 5 real events.
//  3. V2 rw_zone_add's body is Vec[to_add: Address, to_remove: Option<Address>];
//     the second element is not a u32 index.
//  4. gulp_emissions' topic[1] is the POOL address; it lands in the Pool column.
//  5. withdraw's body is (shares_burned, tokens_out), the OPPOSITE of deposit's
//     (tokens_in, shares_minted), so Amount cannot uniformly promote vec[0].
//
// Applying these to stored rows takes
// `projector-replay -source blend_backstop -from 51499923`. Per ADR-0013 SCVal is
// read only through internal/scval; README.md covers wiring and provenance.
package blend_backstop

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ErrUnknownEvent flags an event whose topic[0] symbol isn't one of
// the backstop's ten. Defensive — Classify already gates, and the
// dispatcher's Matches gates on contract id too.
var ErrUnknownEvent = errors.New("blend_backstop: unknown event topic")

// ErrMalformedTopic flags a topic slice shorter than the event type
// requires (a genuinely malformed event — counted + skipped by the
// dispatcher).
var ErrMalformedTopic = errors.New("blend_backstop: malformed event topics")

// ErrMalformedBody surfaces a body whose SCVal shape doesn't match the
// event type at all (e.g. neither i128 nor a Vec where the schema
// requires one). Distinct from a single promoted-field shape
// mismatch, which degrades gracefully into Attributes rather than
// erroring the whole row.
var ErrMalformedBody = errors.New("blend_backstop: malformed event body")

// Classify reports which backstop event the given Event is, or empty
// string if topic[0] doesn't match. Contract-ID filtering happens
// DOWNSTREAM (Matches) — these symbols overlap with Blend POOL events,
// so Classify alone never decides a backstop match.
func Classify(e *events.Event) string {
	if len(e.Topic) < 1 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolDeposit:
		return EventDeposit
	case TopicSymbolClaim:
		return EventClaim
	case TopicSymbolDonate:
		return EventDonate
	case TopicSymbolQueueWithdrawal:
		return EventQueueWithdrawal
	case TopicSymbolWithdraw:
		return EventWithdraw
	case TopicSymbolDistribute:
		return EventDistribute
	case TopicSymbolGulpEmissions:
		return EventGulpEmissions
	case TopicSymbolDequeueWithdrawal:
		return EventDequeueWithdrawal
	case TopicSymbolDraw:
		return EventDraw
	case TopicSymbolRwZoneAdd:
		return EventRwZoneAdd
	case TopicSymbolRwZone:
		return EventRwZone
	case TopicSymbolRwZoneRemove:
		return EventRwZoneRemove
	}
	return ""
}

// decoded is the intermediate shape Decode* helpers fill — the
// promoted columns plus the per-kind Attributes remainder. consumer.go
// stamps the universal identity fields on top.
type decoded struct {
	Pool        string
	UserAddress string
	Amount      string
	Amount2     string
	Attributes  map[string]any
}

// parseTopicAddr parses topic[i] as an Address strkey. A genuine
// malformed topic (wrong SCVal kind / bad checksum) is an error — a
// promoted address that the schema REQUIRES must be present.
func parseTopicAddr(e *events.Event, i int, field string) (string, error) {
	sv, err := scval.Parse(e.Topic[i])
	if err != nil {
		return "", fmt.Errorf("blend_backstop: %s topic[%d] parse: %w", field, i, err)
	}
	addr, err := scval.AsAddressStrkey(sv)
	if err != nil {
		return "", fmt.Errorf("blend_backstop: %s address: %w", field, err)
	}
	return addr, nil
}

// i128String decodes a parsed i128 SCVal (already through scval.Parse)
// from a base64 body string to its decimal string.
func i128String(b64 string) (string, error) {
	sv, err := scval.Parse(b64)
	if err != nil {
		return "", err
	}
	amt, err := scval.AsAmountFromI128(sv)
	if err != nil {
		return "", err
	}
	return amt.String(), nil
}

// twoI128 reads a Vec[i128, i128] body and returns both as decimal
// strings. The two-amount shape recurs (deposit / withdraw /
// gulp_emissions), so it is shared. A body that is not a 2-element Vec
// of i128s is a genuinely malformed event.
//
// Note: the Vec elements are kept in a `:=`-inferred local and fed
// straight back into scval.As* — this file never NAMES the xdr type,
// per ADR-0013 / lint-imports B/xdr-scoped-to-scval. Same convention
// as internal/sources/defindex/decode.go.
func twoI128(e *events.Event, kind string) (a, b string, err error) {
	body, perr := scval.Parse(e.Value)
	if perr != nil {
		return "", "", fmt.Errorf("blend_backstop: %s body parse: %w", kind, perr)
	}
	vec, verr := scval.AsTupleN(body, 2)
	if verr != nil {
		return "", "", fmt.Errorf("%w: %s body not a 2-Vec: %w", ErrMalformedBody, kind, verr)
	}
	av, aerr := scval.AsAmountFromI128(vec[0])
	if aerr != nil {
		return "", "", fmt.Errorf("%w: %s amount[0]: %w", ErrMalformedBody, kind, aerr)
	}
	bv, berr := scval.AsAmountFromI128(vec[1])
	if berr != nil {
		return "", "", fmt.Errorf("%w: %s amount[1]: %w", ErrMalformedBody, kind, berr)
	}
	return av.String(), bv.String(), nil
}

// ─── per-event decoders ──────────────────────────────────────────

// decodeDeposit: topics=[sym, pool, user]; data=Vec[i128 amount, i128 shares].
func decodeDeposit(e *events.Event) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: deposit needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "deposit")
	if err != nil {
		return decoded{}, err
	}
	user, err := parseTopicAddr(e, 2, "deposit")
	if err != nil {
		return decoded{}, err
	}
	amount, shares, err := twoI128(e, "deposit")
	if err != nil {
		return decoded{}, err
	}
	return decoded{Pool: pool, UserAddress: user, Amount: amount, Amount2: shares, Attributes: map[string]any{}}, nil
}

// decodeClaim: topics=[sym, user]; data=i128 amount. NO pool.
func decodeClaim(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		return decoded{}, fmt.Errorf("%w: claim needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	user, err := parseTopicAddr(e, 1, "claim")
	if err != nil {
		return decoded{}, err
	}
	amount, err := i128String(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: claim amount: %w", ErrMalformedBody, err)
	}
	return decoded{UserAddress: user, Amount: amount, Attributes: map[string]any{}}, nil
}

// decodeDonate: topics=[sym, pool, from(contract)]; data=i128 amount.
// pool + amount promoted; from stashed in attributes.
func decodeDonate(e *events.Event) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: donate needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "donate")
	if err != nil {
		return decoded{}, err
	}
	amount, err := i128String(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: donate amount: %w", ErrMalformedBody, err)
	}
	attrs := map[string]any{}
	// `from` is a promoted-into-attributes field; a shape mismatch
	// degrades (note it) rather than erroring the whole row.
	if from, ferr := parseTopicAddr(e, 2, "donate"); ferr == nil {
		attrs["from"] = from
	} else {
		attrs["from_error"] = ferr.Error()
	}
	return decoded{Pool: pool, Amount: amount, Attributes: attrs}, nil
}

// decodeQueueWithdrawal: topics=[sym, pool, user];
// data=Vec[i128 shares, u64 expiration].
func decodeQueueWithdrawal(e *events.Event) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: queue_withdrawal needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "queue_withdrawal")
	if err != nil {
		return decoded{}, err
	}
	user, err := parseTopicAddr(e, 2, "queue_withdrawal")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("blend_backstop: queue_withdrawal body parse: %w", err)
	}
	vec, err := scval.AsTupleN(body, 2)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: queue_withdrawal body not a 2-Vec: %w", ErrMalformedBody, err)
	}
	shares, err := scval.AsAmountFromI128(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: queue_withdrawal shares: %w", ErrMalformedBody, err)
	}
	attrs := map[string]any{}
	if exp, eerr := scval.AsU64(vec[1]); eerr == nil {
		attrs["expiration"] = exp
	} else {
		attrs["expiration_error"] = eerr.Error()
	}
	return decoded{Pool: pool, UserAddress: user, Amount: shares.String(), Attributes: attrs}, nil
}

// decodeWithdraw: topics=[sym, pool, user];
// data=Vec[i128 shares_burned, i128 tokens_out] (blend-contracts-v2
// backstop/src/events.rs: `pub fn withdraw(e, pool_address, from,
// amount, tokens_out)` publishes `(amount, tokens_out)` where `amount`
// is documented as "the amount of backstop shares being burned" — the
// OPPOSITE element order from deposit's (tokens_in, shares_minted).
//
// Amount is normalized here to the TOKEN quantity (tokens_out) so it
// carries the same meaning as deposit's Amount (tokens_in) — the two
// dominant event kinds by row count (~77% of all backstop rows) and
// the two protocol_bespoke.go's "Backstop volume (token-units)" KPI
// cares about most. Amount2 carries the backstop shares burned.
//
// Promoting vec[0] positionally would put shares_burned in Amount — the
// opposite of this convention.
func decodeWithdraw(e *events.Event) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: withdraw needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "withdraw")
	if err != nil {
		return decoded{}, err
	}
	user, err := parseTopicAddr(e, 2, "withdraw")
	if err != nil {
		return decoded{}, err
	}
	sharesBurned, tokensOut, err := twoI128(e, "withdraw")
	if err != nil {
		return decoded{}, err
	}
	return decoded{Pool: pool, UserAddress: user, Amount: tokensOut, Amount2: sharesBurned, Attributes: map[string]any{}}, nil
}

// decodeDistribute: topics=[sym]; data=i128 amount. amount only.
func decodeDistribute(e *events.Event) (decoded, error) {
	amount, err := i128String(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: distribute amount: %w", ErrMalformedBody, err)
	}
	return decoded{Amount: amount, Attributes: map[string]any{}}, nil
}

// decodeGulpEmissions handles BOTH backstop versions — they diverge in
// both topic arity and body shape (a ClickHouse lake census found all
// 209 V1 rows with topic_count=1 and a bare-i128 body, and all 637 V2
// rows with topic_count=2 and a 2-element-Vec body):
//
//   - V1: topics=[sym]; data=i128 (a single pull amount). No pool
//     topic — Pool is left "" rather than guessed.
//   - V2: topics=[sym, pool_address]; data=Vec[i128
//     new_backstop_emissions, i128 new_pool_emissions]
//     (blend-contracts-v2 backstop/src/events.rs `gulp_emissions(e,
//     pool_address, new_backstop_emissions, new_pool_emissions)`).
//     topic[1] is the POOL address — the same field every other event
//     promotes — not a "token", so it lands in the Pool column.
func decodeGulpEmissions(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		// V1 shape: no pool topic, bare-i128 body.
		amount, err := i128String(e.Value)
		if err != nil {
			return decoded{}, fmt.Errorf("%w: gulp_emissions (v1, no pool topic) amount: %w", ErrMalformedBody, err)
		}
		return decoded{Amount: amount, Attributes: map[string]any{}}, nil
	}

	// V2 shape: pool topic + 2-element i128 Vec body.
	pool, err := parseTopicAddr(e, 1, "gulp_emissions")
	if err != nil {
		return decoded{}, err
	}
	amount, amount2, err := twoI128(e, "gulp_emissions")
	if err != nil {
		return decoded{}, err
	}
	return decoded{Pool: pool, Amount: amount, Amount2: amount2, Attributes: map[string]any{}}, nil
}

// decodeDequeueWithdrawal: topics=[sym, pool, user]; data=i128 amount.
func decodeDequeueWithdrawal(e *events.Event) (decoded, error) {
	if len(e.Topic) < 3 {
		return decoded{}, fmt.Errorf("%w: dequeue_withdrawal needs 3 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "dequeue_withdrawal")
	if err != nil {
		return decoded{}, err
	}
	user, err := parseTopicAddr(e, 2, "dequeue_withdrawal")
	if err != nil {
		return decoded{}, err
	}
	amount, err := i128String(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: dequeue_withdrawal amount: %w", ErrMalformedBody, err)
	}
	return decoded{Pool: pool, UserAddress: user, Amount: amount, Attributes: map[string]any{}}, nil
}

// decodeDraw: topics=[sym, pool]; data=Vec[Address to, i128 amount].
// pool promoted; to stashed; amount=data[1].
func decodeDraw(e *events.Event) (decoded, error) {
	if len(e.Topic) < 2 {
		return decoded{}, fmt.Errorf("%w: draw needs 2 topics, got %d", ErrMalformedTopic, len(e.Topic))
	}
	pool, err := parseTopicAddr(e, 1, "draw")
	if err != nil {
		return decoded{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("blend_backstop: draw body parse: %w", err)
	}
	vec, err := scval.AsTupleN(body, 2)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: draw body not a 2-Vec: %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(vec[1])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: draw amount: %w", ErrMalformedBody, err)
	}
	attrs := map[string]any{}
	if to, terr := scval.AsAddressStrkey(vec[0]); terr == nil {
		attrs["to"] = to
	} else {
		attrs["to_error"] = terr.Error()
	}
	return decoded{Pool: pool, Amount: amount.String(), Attributes: attrs}, nil
}

// decodeRwZoneAdd: topics=[sym]; data=Vec[Address to_add,
// Option<Address> to_remove] (blend-contracts-v2
// backstop/src/events.rs `rw_zone_add(e, to_add, to_remove:
// Option<Address>)`, `publish(topics, (to_add, to_remove))`). to_add
// is promoted to Pool (required — matches every other event's pool
// field); to_remove is stashed in attributes ONLY when present — all 5
// real lake rows carry `void` there, so the common case emits no key
// rather than an empty-string placeholder. vec[1] is never a u32
// reward-zone index; no such field exists on the wire.
func decodeRwZoneAdd(e *events.Event) (decoded, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("blend_backstop: rw_zone_add body parse: %w", err)
	}
	vec, err := scval.AsTupleN(body, 2)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: rw_zone_add body not a 2-Vec: %w", ErrMalformedBody, err)
	}
	pool, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: rw_zone_add to_add: %w", ErrMalformedBody, err)
	}
	attrs := map[string]any{}
	if toRemove, rerr := scval.AsAddressOrVoid(vec[1]); rerr == nil {
		if toRemove != "" {
			attrs["to_remove"] = toRemove
		}
	} else {
		attrs["to_remove_error"] = rerr.Error()
	}
	return decoded{Pool: pool, Attributes: attrs}, nil
}

// decodeRwZone: the V1 spelling of the reward-zone-update event —
// topics=[sym]; data=Vec[Address to_add, Address to_remove]. Unlike
// V2's rw_zone_add, V1's second element is NOT Option-wrapped: all 5
// real lake rows (ledgers 51.50M-55.18M) carry two concrete addresses,
// never a void. to_add is promoted to Pool (same convention as
// rw_zone_add); to_remove is stashed in attributes, degrading
// gracefully on a shape mismatch rather than erroring the row (it is
// not a promoted column).
func decodeRwZone(e *events.Event) (decoded, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("blend_backstop: rw_zone body parse: %w", err)
	}
	vec, err := scval.AsTupleN(body, 2)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: rw_zone body not a 2-Vec: %w", ErrMalformedBody, err)
	}
	pool, err := scval.AsAddressStrkey(vec[0])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: rw_zone to_add: %w", ErrMalformedBody, err)
	}
	attrs := map[string]any{}
	if toRemove, rerr := scval.AsAddressStrkey(vec[1]); rerr == nil {
		attrs["to_remove"] = toRemove
	} else {
		attrs["to_remove_error"] = rerr.Error()
	}
	return decoded{Pool: pool, Attributes: attrs}, nil
}

// decodeRwZoneRemove: topics=[sym]; data=Address (the pool removed
// from the reward zone, promoted to Pool).
//
// SOURCE NOTE: the Rust doc comment directly above this function in
// blend-contracts-v2 claims `topics - ["rw_zone_remove", pool_address:
// Address]`, but the actual `let topics = (...)` + `publish()` call one
// line below it is a ONE-element topic tuple with the pool passed as
// bare DATA, not a second topic — a doc-comment/code mismatch in
// Blend's own source. We trust the code (what actually serializes
// on-chain), not the comment:
//
//	pub fn rw_zone_remove(e: &Env, to_remove: Address) {
//	    let topics = (Symbol::new(e, "rw_zone_remove"),);
//	    e.events().publish(topics, to_remove);
//	}
//
// The lake held zero occurrences at the last census (this event had
// never fired on mainnet) — this decoder is SYNTHETIC-FROM-SOURCE,
// unverified against real bytes. See decode_test.go
// TestDecodeRwZoneRemove_SyntheticFromSource.
func decodeRwZoneRemove(e *events.Event) (decoded, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return decoded{}, fmt.Errorf("blend_backstop: rw_zone_remove body parse: %w", err)
	}
	pool, err := scval.AsAddressStrkey(body)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: rw_zone_remove pool: %w", ErrMalformedBody, err)
	}
	return decoded{Pool: pool, Attributes: map[string]any{}}, nil
}
