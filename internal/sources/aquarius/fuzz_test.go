package aquarius

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const fuzzTxHash = "78d86d0651d6e432dab500c7c2cb340fd60b391d681124ae83ea49cf7251eac7"

// refI128 is the independent reference for an i128 (hi, lo) pair:
// hi*2^64 + lo, computed without canonical.
func refI128(hi int64, lo uint64) *big.Int {
	r := new(big.Int).Lsh(big.NewInt(hi), 64)
	return r.Add(r, new(big.Int).SetUint64(lo))
}

func i128FromParts(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

// partsFromBytes slices raw into up to max 16-byte (hi, lo) pairs.
func partsFromBytes(raw []byte, maxN int) (his []int64, los []uint64) {
	for len(raw) >= 16 && len(his) < maxN {
		his = append(his, int64(binary.BigEndian.Uint64(raw[:8]))) //nolint:gosec // reinterpreting fuzz bytes as a signed hi word is the point.
		los = append(los, binary.BigEndian.Uint64(raw[8:16]))
		raw = raw[16:]
	}
	return his, los
}

// fuzzKinds is every topic[0] the Decoder recognises, in a stable order.
func fuzzKinds() []string {
	syms := make([]string, 0, len(kindByTopicSymbol)+1)
	for sym := range kindByTopicSymbol {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	return append(syms, TopicSymbolAddPool)
}

// fixtureSeeds returns (topic[0], topic count, raw body) triples from the
// mainnet trade captures plus the real bodies pinned elsewhere in this
// package's tests.
type fuzzSeed struct {
	topic0  string
	nTopics int
	body    []byte
}

func fixtureSeeds(tb testing.TB) []fuzzSeed {
	tb.Helper()
	b64 := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			tb.Fatalf("seed base64: %v", err)
		}
		return b
	}
	seeds := []fuzzSeed{
		{TopicSymbolSetProtocolFee, 1, b64(realSetFeeBody)},
		{TopicSymbolClaimProtocolFee, 2, b64(realClaimFeeBody)},
		{TopicSymbolClaimReward, 3, b64("AAAAEAAAAAEAAAABAAAACgAAAAAAAAAAAAAADA0rT7g=")},
		{TopicSymbolSetRewardsConfig, 1, b64("AAAAEAAAAAEAAAACAAAABQAAAABoziklAAAACQAAAAAAAAAAAAAAAABxWBw=")},
		{TopicSymbolPositionUpdate, 2, b64("AAAAEAAAAAEAAAADAAAABAABUPQAAAAEAAFg5AAAAAoAAAAAAAAAAAAAAihiwSyd")},
		{TopicSymbolGaugeClaim, 2, b64("AAAACgAAAAAAAAAAAAAAAAAAAAA=")},
	}
	root := filepath.Join("..", "..", "..", "test", "fixtures", "aquarius", "v2-2026-04-23")
	files, _ := filepath.Glob(filepath.Join(root, "*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			tb.Fatalf("read fixture: %v", err)
		}
		var fx aquariusFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			tb.Fatalf("fixture json: %v", err)
		}
		seeds = append(seeds, fuzzSeed{fx.Topics[0], len(fx.Topics), b64(fx.Value)})
	}
	return seeds
}

// FuzzDecodeTrade drives the trade body's three i128 words and checks the
// decoded amounts against a big.Int reference: negatives are malformed,
// a zero side is the recognised no-op, and everything else round-trips
// exactly across the full 128 bits with sold→base, bought→quote.
func FuzzDecodeTrade(f *testing.F) {
	f.Add(int64(0), uint64(100), int64(0), uint64(90), int64(0), uint64(1))
	f.Add(int64(0), uint64(2), int64(0), uint64(0), int64(0), uint64(0))
	f.Add(int64(0), uint64(0), int64(0), uint64(7), int64(0), uint64(0))
	f.Add(int64(-1), uint64(0), int64(0), uint64(5), int64(0), uint64(0))
	f.Add(int64(1<<62), uint64(1<<63), int64(0x7fffffffffffffff), ^uint64(0), int64(0), uint64(0))
	f.Fuzz(func(t *testing.T, sHi int64, sLo uint64, bHi int64, bLo uint64, fHi int64, fLo uint64) {
		tokenIn, tokenOut := makeContractStrkey(t, 0x01), makeContractStrkey(t, 0x02)
		body := vecVal(i128FromParts(sHi, sLo), i128FromParts(bHi, bLo), i128FromParts(fHi, fLo))
		e := &events.Event{
			Ledger: 1, TxHash: fuzzTxHash,
			Topic: []string{
				TopicSymbolTrade,
				encodeContractAddrFromStrkey(t, tokenIn),
				encodeContractAddrFromStrkey(t, tokenOut),
				encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
			},
			Value: marshalB64(t, body),
		}
		sold, bought := refI128(sHi, sLo), refI128(bHi, bLo)
		tr, err := decodeTrade(e, closedAtTest)
		switch {
		case sold.Sign() < 0 || bought.Sign() < 0:
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("sold=%s bought=%s: err=%v, want ErrMalformedPayload", sold, bought, err)
			}
		case sold.Sign() == 0 || bought.Sign() == 0:
			if !errors.Is(err, ErrZeroAmountTrade) {
				t.Fatalf("sold=%s bought=%s: err=%v, want ErrZeroAmountTrade", sold, bought, err)
			}
		default:
			if err != nil {
				t.Fatalf("sold=%s bought=%s: %v", sold, bought, err)
			}
			if tr.BaseAmount.BigInt().Cmp(sold) != 0 || tr.QuoteAmount.BigInt().Cmp(bought) != 0 {
				t.Fatalf("amounts (%s,%s), want (%s,%s)", tr.BaseAmount, tr.QuoteAmount, sold, bought)
			}
			if tr.Pair.Base.ContractID != tokenIn || tr.Pair.Quote.ContractID != tokenOut {
				t.Fatalf("pair %s/%s, want %s/%s", tr.Pair.Base, tr.Pair.Quote, tokenIn, tokenOut)
			}
			if err := tr.Validate(); err != nil {
				t.Fatalf("decoded trade fails Validate: %v", err)
			}
		}
	})
}

// partsBytes is the inverse of partsFromBytes, for seeding.
func partsBytes(vals ...*big.Int) []byte {
	out := make([]byte, 0, 16*len(vals))
	for _, v := range vals {
		hi, lo := splitBigInt128(v)
		out = binary.BigEndian.AppendUint64(out, uint64(hi)) //nolint:gosec // two's-complement round trip of the hi word.
		out = binary.BigEndian.AppendUint64(out, lo)
	}
	return out
}

// FuzzDecodeAmountVecEvents feeds a variable-length Vec<i128> into both
// Vec<i128> consumers — update_reserves and deposit_liquidity — and
// checks acceptance and every decoded value against the reference.
func FuzzDecodeAmountVecEvents(f *testing.F) {
	two64 := new(big.Int).Lsh(big.NewInt(1), 64)
	f.Add(uint8(1), partsBytes(big.NewInt(10), two64, big.NewInt(3)))                         // 2 tokens + shares: valid
	f.Add(uint8(1), partsBytes(big.NewInt(10), big.NewInt(20), big.NewInt(3), big.NewInt(4))) // one element too many
	f.Add(uint8(1), partsBytes(big.NewInt(10), big.NewInt(20)))                               // one element short
	f.Add(uint8(1), partsBytes(big.NewInt(-10), big.NewInt(20), big.NewInt(3)))               // negative amount/reserve
	f.Add(uint8(0), partsBytes(big.NewInt(10), big.NewInt(-3)))                               // negative shares/reserve
	f.Add(uint8(1), []byte{})
	f.Fuzz(func(t *testing.T, nTok uint8, raw []byte) {
		nTokens := int(nTok%4) + 1
		his, los := partsFromBytes(raw, 8)
		want := make([]*big.Int, len(his))
		elts := make([]xdr.ScVal, len(his))
		anyNeg := false
		for i := range his {
			want[i] = refI128(his[i], los[i])
			elts[i] = i128FromParts(his[i], los[i])
			anyNeg = anyNeg || want[i].Sign() < 0
		}
		body := marshalB64(t, vecVal(elts...))

		rv, err := decodeReserves(&events.Event{Value: body}, closedAtTest, EventUpdateReserves)
		if wantOK := len(want) > 0 && !anyNeg; (err == nil) != wantOK {
			t.Fatalf("reserves %v: err=%v, want ok=%v", want, err, wantOK)
		}
		if err == nil {
			for i, r := range rv.Reserves {
				if r.BigInt().Cmp(want[i]) != 0 {
					t.Fatalf("reserve[%d]=%s, want %s", i, r, want[i])
				}
			}
		}

		topics := []string{TopicSymbolDepositLiquidity}
		tokens := make([]string, nTokens)
		for i := range tokens {
			tokens[i] = makeContractStrkey(t, byte(0x40+i))
			topics = append(topics, encodeContractAddrFromStrkey(t, tokens[i]))
		}
		lq, err := decodeLiquidity(&events.Event{Topic: topics, Value: body}, LiquidityDeposit, closedAtTest)
		if wantOK := len(want) == nTokens+1 && !anyNeg; (err == nil) != wantOK {
			t.Fatalf("liquidity n=%d vals=%v: err=%v, want ok=%v", nTokens, want, err, wantOK)
		}
		if err != nil {
			return
		}
		if !slices.Equal(lq.Tokens, tokens) {
			t.Fatalf("tokens %v, want %v", lq.Tokens, tokens)
		}
		for i, a := range lq.Amounts {
			if a.BigInt().Cmp(want[i]) != 0 {
				t.Fatalf("amount[%d]=%s, want %s", i, a, want[i])
			}
		}
		if lq.Shares.BigInt().Cmp(want[nTokens]) != 0 {
			t.Fatalf("shares=%s, want %s", lq.Shares, want[nTokens])
		}
	})
}

// FuzzDecoderDecode_invariants pushes arbitrary body bytes through every
// recognised kind via the production Decode entry point. Whatever is
// emitted must satisfy the served tier's money invariants — a decode that
// succeeds on a shape the sink must refuse is a defect.
func FuzzDecoderDecode_invariants(f *testing.F) {
	kinds := fuzzKinds()
	for _, s := range fixtureSeeds(f) {
		f.Add(uint8(slices.Index(kinds, s.topic0)), uint8(s.nTopics), s.body)
	}
	dec := NewDecoder()
	f.Fuzz(func(t *testing.T, kindIdx, nTopics uint8, body []byte) {
		topics := []string{kinds[int(kindIdx)%len(kinds)]}
		for i := 1; i < int(nTopics%6); i++ {
			if i%2 == 1 {
				topics = append(topics, encodeContractAddrFromStrkey(t, makeContractStrkey(t, byte(i))))
			} else {
				topics = append(topics, encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, byte(i))))
			}
		}
		ev := events.Event{
			ContractID: MainnetRouter, Ledger: 1, TxHash: fuzzTxHash,
			LedgerClosedAt: "2026-04-16T20:09:32Z",
			Topic:          topics,
			Value:          base64.StdEncoding.EncodeToString(body),
		}
		out, err := dec.Decode(ev)
		if err != nil {
			return
		}
		for _, o := range out {
			checkEmittedInvariants(t, o)
		}
	})
}

func checkEmittedInvariants(t *testing.T, o any) {
	t.Helper()
	switch e := o.(type) {
	case TradeEvent:
		if err := e.Trade.Validate(); err != nil {
			t.Fatalf("emitted trade fails Validate: %v", err)
		}
	case ReservesEvent:
		if len(e.Reserves) == 0 {
			t.Fatal("emitted empty reserve vector")
		}
		for i, r := range e.Reserves {
			if r.Sign() < 0 {
				t.Fatalf("reserve[%d] negative: %s", i, r)
			}
		}
	case LiquidityEvent:
		if len(e.Amounts) != len(e.Tokens) || len(e.Tokens) == 0 {
			t.Fatalf("tokens/amounts %d/%d", len(e.Tokens), len(e.Amounts))
		}
		for i, a := range e.Amounts {
			if a.Sign() < 0 {
				t.Fatalf("amount[%d] negative: %s", i, a)
			}
		}
		if e.Shares.Sign() < 0 {
			t.Fatalf("shares negative: %s", e.Shares)
		}
	case RewardsEvent:
		if e.Amount != nil && e.Amount.Sign() < 0 {
			t.Fatalf("%s: promoted Amount negative: %s", e.Kind, e.Amount)
		}
	case FeeEvent:
		switch e.Kind {
		case EventClaimProtocolFee:
			if e.Amount.Sign() < 0 || e.Recipient == "" || e.Token == "" {
				t.Fatalf("claim fee: amount=%s recipient=%q token=%q", e.Amount, e.Recipient, e.Token)
			}
		case EventSetProtocolFee:
			if !e.HasOldFee && (e.Fee0New != e.Fee1New || e.Fee0Old != 0 || e.Fee1Old != 0) {
				t.Fatalf("vec-form set fee invented values: %+v", e)
			}
		default:
			t.Fatalf("fee kind %q", e.Kind)
		}
	case KillEvent, AdminEvent:
	default:
		t.Fatalf("unexpected emitted type %T", o)
	}
}

// routerOnlyKinds and poolOrRouterKinds restate the identity-gate spec
// independently of Matches: every other recognised kind is pool-flow and
// matches only a registered pool.
var (
	routerOnlyKinds   = []string{EventConfigRewards, EventPoolGaugeSwitchToken}
	poolOrRouterKinds = []string{
		EventApplyUpgrade, EventCommitUpgrade, EventSetPrivilegedAddrs,
		EventApplyTransferOwnership, EventCommitTransferOwnership,
		EventEnableEmergencyMode, EventDisableEmergencyMode,
		EventSetProtocolFee, EventClaimProtocolFee,
		EventKillDeposit, EventUnkillDeposit, EventKillSwap, EventUnkillSwap,
		EventKillClaim, EventUnkillClaim, EventKillGaugesClaim, EventUnkillGaugesClaim,
	}
)

// FuzzMatches_identityGate: whatever the topics, an event matches only
// when its emitter is trusted for that kind — a registered pool for
// pool-flow kinds, the router for router-scoped kinds and add_pool,
// either for the pool-emittable governance kinds, never anyone else.
func FuzzMatches_identityGate(f *testing.F) {
	kinds := fuzzKinds()
	for k := range kinds {
		for who := uint8(0); who < 3; who++ {
			f.Add([]byte{byte(k)}, who, uint8(k), uint8(4))
		}
	}
	pool := MainnetGatedSet()[0]
	f.Fuzz(func(t *testing.T, seed []byte, who, kindIdx, nTopics uint8) {
		var cid string
		switch who % 3 {
		case 0:
			cid = pool
		case 1:
			cid = MainnetRouter
		default:
			var raw [32]byte
			copy(raw[:], seed)
			var err error
			if cid, err = strkey.Encode(strkey.VersionByteContract, raw[:]); err != nil {
				t.Fatal(err)
			}
			if cid == MainnetRouter || slices.Contains(MainnetGatedSet(), cid) {
				return
			}
		}
		topic0 := kinds[int(kindIdx)%len(kinds)]
		topics := []string{topic0}
		for i := 1; i < int(nTopics%6); i++ {
			topics = append(topics, encodeContractAddrFromStrkey(t, makeContractStrkey(t, byte(i))))
		}
		isPool, isRouter := cid == pool, cid == MainnetRouter
		var want bool
		switch kind := kindByTopicSymbol[topic0]; {
		case topic0 == TopicSymbolAddPool, slices.Contains(routerOnlyKinds, kind):
			want = isRouter
		case slices.Contains(poolOrRouterKinds, kind):
			want = isPool || isRouter
		default:
			want = isPool
		}
		if got := NewDecoder().Matches(events.Event{ContractID: cid, Topic: topics}); got != want {
			t.Fatalf("emitter %s topic %q: Matches=%v, want %v", cid, topic0, got, want)
		}
	})
}

// A fixed-arity body with a trailing element is a different schema, not
// a superset to read a prefix of.
func TestDecode_overlongBodiesRejected(t *testing.T) {
	token := makeContractStrkey(t, 0x61)
	user := makeAccountStrkey(t, 0x62)
	one := i128Val(big.NewInt(1))
	cases := map[string]events.Event{
		EventClaimProtocolFee: {
			Topic: []string{TopicSymbolClaimProtocolFee, encodeContractAddrFromStrkey(t, token)},
			Value: marshalB64(t, vecVal(contractAddrVal(t, token), one, one)),
		},
		EventClaimReward: {
			Topic: []string{TopicSymbolClaimReward, encodeContractAddrFromStrkey(t, token), encodeAccountAddrFromStrkey(t, user)},
			Value: marshalB64(t, vecVal(one, one)),
		},
	}
	for kind, e := range cases {
		if _, err := decodeClaimAmount(t, kind, e); !errors.Is(err, ErrMalformedPayload) {
			t.Errorf("%s with a trailing element: err=%v, want ErrMalformedPayload", kind, err)
		}
	}
}
