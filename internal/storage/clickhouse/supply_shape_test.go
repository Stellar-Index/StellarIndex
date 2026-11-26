package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// A concentrated-liquidity pool's `mint` as stored in the mainnet lake: ONE topic, and a map whose `amount` is a u128 liquidity figure.
const (
	poolMintContract = "CBTOQ7X7TLDBRHJW4UYUDR7WR4NAYESZA7FYUZVLAN2JLQG5ZXWJJIG6"
	poolMintTopic0   = "AAAADwAAAARtaW50"
	poolMintBody     = "AAAAEQAAAAEAAAAHAAAADwAAAAZhbW91bnQAAAAAAAkAAAAAAAAAAAAAAAaxNSylAAAADwAAAAdhbW91bnQwAAAAAAkAAAAAAAAAAAAAAACVAvkAAAAADwAAAAdhbW91bnQxAAAAAAkAAAAAAAAAAAAAAACVAvkAAAAADwAAAAVvd25lcgAAAAAAABIAAAABIzovoLDzgMm/PSicyUIChIApn2d5n/jeVHAlcu+O3HoAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAAAAAAAPa6rGLzKsyuoU7E15VjCjVHRpqEcWgmvccDyP2KM39QAAAAPAAAACnRpY2tfbG93ZXIAAAAAAAT///jkAAAADwAAAAp0aWNrX3VwcGVyAAAAAAAEAAAHHA=="
)

func contractAddrScVal(b byte) xdr.ScVal {
	var cid xdr.ContractId
	cid[0] = b
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}}
}

func accountAddrScVal(b byte) xdr.ScVal {
	var key xdr.Uint256
	key[0] = b
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &key}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}}
}

func symScVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func strScVal(s string) xdr.ScVal {
	str := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
}

func b64All(t *testing.T, vals ...xdr.ScVal) []string {
	t.Helper()
	out := make([]string, len(vals))
	for i, v := range vals {
		s, err := xdr.MarshalBase64(v)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = s
	}
	return out
}

func muxedBody(t *testing.T, lo uint64) string {
	t.Helper()
	amt := xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(lo)}}
	id := xdr.Uint64(9)
	m := &xdr.ScMap{
		{Key: symScVal("amount"), Val: amt},
		{Key: symScVal("to_muxed_id"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &id}},
	}
	return b64All(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m})[0]
}

func TestClassifySupplyEventXDR_ProductionPoolMintIsNotTokenShaped(t *testing.T) {
	amt, skip := ClassifySupplyEventXDR(poolMintContract, []string{poolMintTopic0}, poolMintBody)
	if skip != SupplySkipNotTokenShaped || amt != nil {
		t.Fatalf("one-topic pool mint: skip=%q amt=%v, want %q", skip, amt, SupplySkipNotTokenShaped)
	}
	if SupplySkipAlerts(skip) {
		t.Error("the pool's mint must not alert")
	}
	ext := &LedgerExtract{}
	recordSupplyFlow(ext, ContractEventRow{ContractID: poolMintContract, Topic0Sym: "mint", TopicsXDR: []string{poolMintTopic0}, DataXDR: poolMintBody}, 1, time.Unix(0, 0), "tx")
	if len(ext.SupplyFlows) != 0 || len(ext.SupplyFlowSkipSamples) != 0 || ext.SupplyFlowSkips[SupplySkipNotTokenShaped] != 1 || len(ext.SupplyFlowSkips) != 1 {
		t.Errorf("flows=%d samples=%v skips=%v, want only {not_token_shaped: 1}", len(ext.SupplyFlows), ext.SupplyFlowSkipSamples, ext.SupplyFlowSkips)
	}
}

// Token-shaped topics with an amount that is not an i128 are a token losing supply: the alarm stays, u128 is not accepted.
func TestClassifySupplyEventXDR_TokenShapedNonI128StillAlerts(t *testing.T) {
	topics := b64All(t, symScVal("mint"), contractAddrScVal(1))
	bareU128 := b64All(t, xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Lo: 5}})[0]
	for name, body := range map[string]string{"pool's u128-amount map": poolMintBody, "bare u128": bareU128} {
		amt, skip := ClassifySupplyEventXDR(poolMintContract, topics, body)
		if skip != SupplySkipAmountNotI128 || amt != nil || !SupplySkipAlerts(skip) {
			t.Errorf("%s under [mint, to]: skip=%q amt=%v, want alerting %q", name, skip, amt, SupplySkipAmountNotI128)
		}
	}
}

func TestClassifySupplyEventXDR_TokenShapes(t *testing.T) {
	g, c, asset := accountAddrScVal(2), contractAddrScVal(3), strScVal("USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	for _, tc := range []struct {
		name   string
		topics []xdr.ScVal
	}{
		{"mint CAP-67", []xdr.ScVal{symScVal("mint"), c}},
		{"mint SAC", []xdr.ScVal{symScVal("mint"), g, asset}},
		{"mint legacy admin", []xdr.ScVal{symScVal("mint"), c, g}},
		{"mint legacy SAC", []xdr.ScVal{symScVal("mint"), c, g, asset}},
		{"burn", []xdr.ScVal{symScVal("burn"), g}},
		{"burn SAC", []xdr.ScVal{symScVal("burn"), g, asset}},
		{"clawback SAC", []xdr.ScVal{symScVal("clawback"), g, asset}},
		{"clawback legacy admin", []xdr.ScVal{symScVal("clawback"), c, g}},
		{"transfer", []xdr.ScVal{symScVal("transfer"), g, c}},
		{"transfer SAC", []xdr.ScVal{symScVal("transfer"), g, c, asset}},
	} {
		topics := b64All(t, tc.topics...)
		for body, want := range map[string]int64{i128Body(t, 777): 777, muxedBody(t, 888): 888} {
			amt, skip := ClassifySupplyEventXDR(poolMintContract, topics, body)
			if skip != "" || amt == nil || amt.Int64() != want {
				t.Errorf("%s: skip=%q amt=%v, want amount %d", tc.name, skip, amt, want)
			}
		}
	}
}

func TestClassifySupplyEventXDR_NotTokenShapedTopics(t *testing.T) {
	g := accountAddrScVal(2)
	for _, tc := range []struct {
		name   string
		topics []xdr.ScVal
	}{
		{"one-topic mint", []xdr.ScVal{symScVal("mint")}},
		{"one-topic burn", []xdr.ScVal{symScVal("burn")}},
		{"one-topic clawback", []xdr.ScVal{symScVal("clawback")}},
		{"one-topic transfer", []xdr.ScVal{symScVal("transfer")}},
		{"transfer without to", []xdr.ScVal{symScVal("transfer"), g}},
		{"mint with a symbol recipient", []xdr.ScVal{symScVal("mint"), symScVal("pool")}},
		{"transfer to a non-address", []xdr.ScVal{symScVal("transfer"), g, symScVal("x")}},
	} {
		// An i128 body is the case the topic rule decides alone: the body would otherwise decode.
		amt, skip := ClassifySupplyEventXDR(poolMintContract, b64All(t, tc.topics...), i128Body(t, 5))
		if skip != SupplySkipNotTokenShaped || amt != nil {
			t.Errorf("%s: skip=%q amt=%v, want %q", tc.name, skip, amt, SupplySkipNotTokenShaped)
		}
	}
}
