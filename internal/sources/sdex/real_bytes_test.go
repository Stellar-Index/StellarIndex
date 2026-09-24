package sdex

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Real mainnet op+result frames (ledgers 40,000,000-40,000,003, 2022-03-12),
// byte-identical to the ones internal/sources/classicmovements/real_bytes_test.go
// pulled from the r1 lake. Every other test in this package decodes hand-built
// XDR, so only these pin what stellar-core actually emits: the ClaimAtom
// discriminants, the atom order inside a multi-hop result, and which side of
// each atom is "sold".

type wantClaim struct {
	pair        string
	base, quote string
	maker       string
	opIndex     uint32
}

type realFrame struct {
	name      string
	bodyB64   string
	resultB64 string
	ledger    uint32
	txHash    string
	opIndex   int
	txSource  string
	closedAt  time.Time
	want      []wantClaim
}

var realFrames = []realFrame{
	{
		// PathPaymentStrictReceive native -> SHIB -> native: hop 0 crosses an
		// order-book offer, hop 1 a classic liquidity pool. The only real
		// mixed-atom fanout in the suite.
		name:      "pathPaymentStrictReceive_offerThenPool",
		bodyB64:   "AAAAAgAAAAAAAAAABPtnBgAAAAAYyCYed5ULPCCZ1wtggxYUtoK6Hu5uyhKp0+DNkFyZ1gAAAAAAAAAABPtuGAAAAAEAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5w==",
		resultB64: "AAAAAAAAAAIAAAAAAAAAAgAAAAEAAAAAbaHTfp0wRYC9BeJP51OYornphxPI+aozmlv3trm4KbAAAAAAOKJmcAAAAAFTSElCAAAAAFru6lDtgm23+N9O74XWaYlltSJKuN4n1w2WI+dgyeznAAAAjC6sEZoAAAAAAAAAAAT7J2kAAAACd3xyt7p6rXDgES6e0Vie5PU5pKZvgUqA2rAbTcL4mHEAAAAAAAAAAAT7bhgAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5wAAAIwurBGaAAAAABjIJh53lQs8IJnXC2CDFhS2groe7m7KEqnT4M2QXJnWAAAAAAAAAAAE+24Y",
		ledger:    40_000_003,
		txHash:    "49203432aa0b5da1a3f621e093cdde6a116064f969efe6f3b6162691d1afb84b",
		opIndex:   0,
		txSource:  "GAMMQJQ6O6KQWPBATHLQWYEDCYKLNAV2D3XG5SQSVHJ6BTMQLSM5MSLE",
		closedAt:  time.Date(2022, 3, 12, 19, 33, 16, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "SHIB-GBNO52SQ5WBG3N7Y35HO7BOWNGEWLNJCJK4N4J6XBWLCHZ3AZHWOPRKF/native",
				base: "602078450074", quote: "83568489",
				maker: "GBW2DU36TUYELAF5AXRE7Z2TTCRLT2MHCPEPTKRTTJN7PNVZXAU3BDDS", opIndex: 0,
			},
			{
				pair: "native/SHIB-GBNO52SQ5WBG3N7Y35HO7BOWNGEWLNJCJK4N4J6XBWLCHZ3AZHWOPRKF",
				base: "83586584", quote: "602078450074",
				maker: "777c72b7ba7aad70e0112e9ed1589ee4f539a4a66f814a80dab01b4dc2f89871", opIndex: 1,
			},
		},
	},
	{
		// PathPaymentStrictSend aiXDOGE -> native -> AQUA at op_index 2: two
		// order-book atoms, so the fanout stride is exercised off op 0.
		name:      "pathPaymentStrictSend_twoOffers",
		bodyB64:   "AAAADQAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAAbwSApPmwboWhG14u1quvJh4f0t3hW09pLOIa1MPeGVwAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAAbpIAAAABAAAAAA==",
		resultB64: "AAAAAAAAAA0AAAAAAAAAAgAAAAEAAAAAQJzSfng2B5F/ARo1w+R7fYQ1nuZE0ILRfdDZNwTNIh8AAAAAOK4wUAAAAAAAAAAAAAAETAAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAEAAAAAQ5f/457bn13BXKa5Mccm5n80F2Y9HiOh0k2x4c4FIV4AAAAAOK47NAAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAA+DkAAAAAAAAAAAAABEwAAAAAG8EgKT5sG6FoRteLtarryYeH9Ld4VtPaSziGtTD3hlcAAAABQVFVQQAAAABblC5TrDPI/QqAzHwbGoXX2DipxBl3qtGLOvBX+OM98AAAAAAAAPg5",
		ledger:    40_000_000,
		txHash:    "04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40",
		opIndex:   2,
		txSource:  "GAN4CIBJHZWBXILII3LYXNNK5PEYPB7UW54FNU62JM4INNJQ66DFPWWG",
		closedAt:  time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "native/aiXDOGE-GAPGM7PVMHELZVRHNHPIAHVBDC2IY7FPFHGQTURLUTBRVH657Z466RAI",
				base: "1100", quote: "200000000",
				maker: "GBAJZUT6PA3APEL7AENDLQ7EPN6YINM64ZCNBAWRPXINSNYEZURB6ACP", opIndex: 2 * opIndexFanoutStride,
			},
			{
				pair: "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA/native",
				base: "63545", quote: "1100",
				maker: "GBBZP77DT3NZ6XOBLSTLSMOHE3TH6NAXMY6R4I5B2JG3DYOOAUQV5DGV", opIndex: 2*opIndexFanoutStride + 1,
			},
		},
	},
	{
		name:      "pathPaymentStrictReceive_singleOffer",
		bodyB64:   "AAAAAgAAAAAAAAAAALjvwAAAAAAWcHHVU3Pe9F+qSovW7OF1H73pa76MnRgmpMUXGO0/XQAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAA",
		resultB64: "AAAAAAAAAAIAAAAAAAAAAQAAAAEAAAAAtBJMX3t5P0oPc7eVIwj3MXMLwgQZNa6roDUwTYg44x8AAAAAOJxqdAAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAAAAAAAAC3GwAAAAAAFnBx1VNz3vRfqkqL1uzhdR+96Wu+jJ0YJqTFFxjtP10AAAABU09OWQAAAAC7C3F2yDiErh/qTFZUN1P84joJFHH5EEB5ykfohQCz+AADMouUTEAA",
		ledger:    40_000_001,
		txHash:    "32696e52909644d21ff1e36afbb6379cb8a555cbf658e8c7ae66a0c9c5b417b0",
		opIndex:   0,
		txSource:  "GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G",
		closedAt:  time.Date(2022, 3, 12, 19, 33, 2, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "SONY-GC5QW4LWZA4IJLQ75JGFMVBXKP6OEOQJCRY7SECAPHFEP2EFACZ7QZW5/native",
				base: "900000000000000", quote: "12000000",
				maker: "GC2BETC7PN4T6SQPOO3ZKIYI64YXGC6CAQMTLLVLUA2TATMIHDRR7HMS", opIndex: 0,
			},
		},
	},
}

// TestRealBytes_claimFanout pins, per claim atom of a real result, the pair
// orientation (base = the asset the maker sold), both amounts, the maker
// (G-address for an offer, pool-id hex for a pool) and the fanout OpIndex.
func TestRealBytes_claimFanout(t *testing.T) {
	for _, f := range realFrames {
		t.Run(f.name, func(t *testing.T) {
			ctx := realFrameContext(t, f)
			if !NewDecoder().Matches(ctx.Op) {
				t.Fatalf("Matches() = false for op type %s", ctx.Op.Body.Type)
			}
			outs, failed := NewDecoder().DecodeCounted(ctx)
			if failed != 0 {
				t.Fatalf("DecodeCounted failed = %d, want 0", failed)
			}
			if len(outs) != len(f.want) {
				t.Fatalf("got %d trades, want %d", len(outs), len(f.want))
			}
			for i, w := range f.want {
				ev, ok := outs[i].(TradeEvent)
				if !ok {
					t.Fatalf("claim %d: output is %T, want TradeEvent", i, outs[i])
				}
				assertRealClaim(t, i, ev, f, w)
			}
		})
	}
}

func assertRealClaim(t *testing.T, i int, ev TradeEvent, f realFrame, w wantClaim) {
	t.Helper()
	tr := ev.Trade
	if got := tr.Pair.String(); got != w.pair {
		t.Errorf("claim %d: Pair = %s, want %s", i, got, w.pair)
	}
	if tr.BaseAmount.String() != w.base || tr.QuoteAmount.String() != w.quote {
		t.Errorf("claim %d: amounts = %s/%s, want %s/%s", i, tr.BaseAmount.String(), tr.QuoteAmount.String(), w.base, w.quote)
	}
	if tr.Maker != w.maker {
		t.Errorf("claim %d: Maker = %s, want %s", i, tr.Maker, w.maker)
	}
	if tr.OpIndex != w.opIndex {
		t.Errorf("claim %d: OpIndex = %d, want %d", i, tr.OpIndex, w.opIndex)
	}
	if tr.Taker != f.txSource || tr.Ledger != f.ledger || tr.TxHash != f.txHash || !tr.Timestamp.Equal(f.closedAt) {
		t.Errorf("claim %d: taker/ledger/tx/ts = %s/%d/%s/%s, want %s/%d/%s/%s", i,
			tr.Taker, tr.Ledger, tr.TxHash, tr.Timestamp, f.txSource, f.ledger, f.txHash, f.closedAt)
	}
	if err := tr.Validate(); err != nil {
		t.Errorf("claim %d: Validate() = %v, want nil", i, err)
	}
}

func realFrameContext(t *testing.T, f realFrame) dispatcher.OpContext {
	t.Helper()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(f.bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(f.resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return dispatcher.OpContext{
		Ledger:   f.ledger,
		ClosedAt: f.closedAt,
		TxHash:   f.txHash,
		TxSource: f.txSource,
		OpIndex:  f.opIndex,
		Op:       xdr.Operation{Body: body},
		OpResult: result,
	}
}
