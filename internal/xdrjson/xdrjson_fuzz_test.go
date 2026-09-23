// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package xdrjson

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FuzzAssetID checks every asset renderer in this package agrees with
// canonical.AssetFromXDR — the identity the rest of the pipeline keys on — so
// the explorer never mints an id the canonical layer would refuse or render
// differently, and that SACContractID inverts the rendered id back to the
// asset's real SAC address (the Blend-reserve pricing join).
func FuzzAssetID(f *testing.F) {
	iss, err := strkey.Decode(strkey.VersionByteAccountID, issuerAccount)
	if err != nil {
		f.Fatal(err)
	}
	for _, c := range []struct {
		kind uint8
		code string
	}{
		{0, ""},
		{1, "USDC"},
		{1, "A"},
		{1, "AB\x00C"},
		{1, "A\x01B"},
		{2, "yUSDC"},
		{2, "ABCDEFGHIJKL"},
		{2, "AQUA"},
		{2, "ABCDE\x00\x00F"},
		{2, "é"},
		{1, "US-D"},
		{1, "\x00USD"},
		{2, "\x00\x00ABCDE"},
	} {
		f.Add(c.kind, []byte(c.code), iss)
	}
	f.Fuzz(func(t *testing.T, kind uint8, code, issuer []byte) {
		var k [32]byte
		copy(k[:], issuer)
		pk := xdr.Uint256(k)
		acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}

		var a xdr.Asset
		var tl xdr.TrustLineAsset
		var ct xdr.ChangeTrustAsset
		fits := true
		switch kind % 3 {
		case 0:
			a = xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
			tl = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative}
			ct = xdr.ChangeTrustAsset{Type: xdr.AssetTypeAssetTypeNative}
		case 1:
			var c4 xdr.AssetCode4
			copy(c4[:], code)
			an := &xdr.AlphaNum4{AssetCode: c4, Issuer: acct}
			a = xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: an}
			tl = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: an}
			ct = xdr.ChangeTrustAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: an}
		case 2:
			var c12 xdr.AssetCode12
			copy(c12[:], code)
			an := &xdr.AlphaNum12{AssetCode: c12, Issuer: acct}
			a = xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: an}
			tl = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: an}
			ct = xdr.ChangeTrustAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: an}
			// stellar-core requires alphanum12 codes of 5..12 characters;
			// only then is the (code, issuer) id unambiguous about its arm.
			fits = len(canonical.TrimTrailingNulls(c12[:])) >= 5
		}

		want := "unknown_asset"
		if ca, err := canonical.AssetFromXDR(a); err == nil {
			want = ca.String()
		}
		if got := AssetID(a); got != want {
			t.Fatalf("AssetID = %q, canonical = %q", got, want)
		}
		if got := TrustLineAssetID(tl); got != want {
			t.Fatalf("TrustLineAssetID = %q, canonical = %q", got, want)
		}
		if got := changeTrustAsset(ct); got != want {
			t.Fatalf("changeTrustAsset = %q, canonical = %q", got, want)
		}
		if p := assetPath([]xdr.Asset{a, {Type: xdr.AssetTypeAssetTypeNative}}); len(p) != 2 || p[0] != want || p[1] != "native" {
			t.Fatalf("assetPath = %v, want [%s native]", p, want)
		}

		if want == "unknown_asset" || !fits {
			return
		}
		gotSAC, ok := SACContractID(want, pubnet)
		if !ok {
			t.Fatalf("SACContractID(%q) ok=false", want)
		}
		id, err := a.ContractID(pubnet)
		if err != nil {
			t.Fatal(err)
		}
		wantSAC, err := strkey.Encode(strkey.VersionByteContract, id[:])
		if err != nil {
			t.Fatal(err)
		}
		if gotSAC != wantSAC {
			t.Fatalf("SACContractID(%q) = %s, want %s", want, gotSAC, wantSAC)
		}
		if other, _ := SACContractID(want, "Test SDF Network ; September 2015"); other == gotSAC {
			t.Fatalf("SACContractID(%q) is the same on two networks", want)
		}
	})
}

// opBodySeeds returns base64 OperationBody seeds covering every amount-bearing
// arm, plus an InvokeHostFunction.
func opBodySeeds(tb testing.TB) []string {
	tb.Helper()
	iss, _ := strkey.Decode(strkey.VersionByteAccountID, issuerAccount)
	var k [32]byte
	copy(k[:], iss)
	pk := xdr.Uint256(k)
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	mux := xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: &pk}
	muxM := xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeMuxedEd25519, Med25519: &xdr.MuxedAccountMed25519{Id: 7, Ed25519: pk}}
	usdc := xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: xdr.AssetCode4{'U', 'S', 'D', 'C'}, Issuer: acct}}
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	k2 := k
	k2[0] = 0 // GAAA…, which sorts after issuerAccount (GA5Z…)
	pk2 := xdr.Uint256(k2)
	acct2 := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk2}
	big := xdr.Int64(1<<62 + 12345)
	cid := xdr.ContractId(k)
	ops := []struct {
		typ xdr.OperationType
		v   any
	}{
		{xdr.OperationTypeCreateAccount, xdr.CreateAccountOp{Destination: acct, StartingBalance: big}},
		{xdr.OperationTypePayment, xdr.PaymentOp{Destination: muxM, Asset: usdc, Amount: big}},
		{xdr.OperationTypePathPaymentStrictReceive, xdr.PathPaymentStrictReceiveOp{SendAsset: usdc, SendMax: 5, Destination: mux, DestAsset: native, DestAmount: big, Path: []xdr.Asset{native, usdc}}},
		{xdr.OperationTypePathPaymentStrictSend, xdr.PathPaymentStrictSendOp{SendAsset: native, SendAmount: big, Destination: mux, DestAsset: usdc, DestMin: 3}},
		{xdr.OperationTypeManageSellOffer, xdr.ManageSellOfferOp{Selling: native, Buying: usdc, Amount: big, Price: xdr.Price{N: 7, D: 2}, OfferId: 99}},
		{xdr.OperationTypeManageBuyOffer, xdr.ManageBuyOfferOp{Selling: usdc, Buying: native, BuyAmount: big, Price: xdr.Price{N: 1, D: 3}, OfferId: -1}},
		{xdr.OperationTypeCreatePassiveSellOffer, xdr.CreatePassiveSellOfferOp{Selling: usdc, Buying: native, Amount: 1, Price: xdr.Price{N: 2, D: 9}}},
		{xdr.OperationTypeChangeTrust, xdr.ChangeTrustOp{Line: xdr.ChangeTrustAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: usdc.AlphaNum4}, Limit: 1<<63 - 1}},
		{xdr.OperationTypeClawback, xdr.ClawbackOp{Asset: usdc, From: muxM, Amount: big}},
		{xdr.OperationTypeBumpSequence, xdr.BumpSequenceOp{BumpTo: 1<<40 + 1}},
		{xdr.OperationTypeAccountMerge, muxM},
		// Claimants deliberately out of strkey order, plus a duplicate.
		{xdr.OperationTypeCreateClaimableBalance, xdr.CreateClaimableBalanceOp{Asset: native, Amount: 1, Claimants: []xdr.Claimant{
			{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{Destination: acct2, Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional}}},
			{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{Destination: acct, Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional}}},
			{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{Destination: acct2, Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional}}},
		}}},
		// An InvokeContract whose target is an account, not a contract.
		{xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct},
				FunctionName:    "transfer",
			},
		}}},
		{xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				FunctionName:    "swap",
			},
		}}},
	}
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		body, err := xdr.NewOperationBody(o.typ, o.v)
		if err != nil {
			tb.Fatalf("NewOperationBody(%v): %v", o.typ, err)
		}
		b64, err := xdr.MarshalBase64(body)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, b64)
	}
	return out
}

// TestOperationBody_rejectsTrailingBytesInFinalQuantum pins that both op-body
// entry points reject trailing bytes that share the final base64 quantum,
// which xdr.SafeUnmarshalBase64's consumed-length check misses.
func TestOperationBody_rejectsTrailingBytesInFinalQuantum(t *testing.T) {
	tested := 0
	for _, b64 := range opBodySeeds(t) {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		hidden := map[int]int{0: 0, 1: 2, 2: 1}[len(raw)%3] // trailing bytes inside the last quantum
		for extra := 1; extra <= hidden; extra++ {
			tested++
			padded := base64.StdEncoding.EncodeToString(append(append([]byte{}, raw...), make([]byte, extra)...))
			if d, err := DecodeOperationBody(padded); err == nil {
				t.Errorf("DecodeOperationBody(%s body + %d byte(s)) accepted: %v", d.Type, extra, d.Fields)
			}
			if ps, err := ParticipantAccounts(padded); err == nil {
				t.Errorf("ParticipantAccounts(body + %d byte(s)) accepted: %v", extra, ps)
			}
		}
	}
	if tested == 0 {
		t.Fatal("no seed body has a partial final base64 quantum; the test is vacuous")
	}
}

// FuzzDecodeOperationBody checks the operation renderer is total on any wire
// body and that every classic Int64 amount it emits is the exact decimal of
// the on-wire stroops (ADR-0003: a string, never a float, never truncated),
// with each asset/price field bound to the right operand.
func FuzzDecodeOperationBody(f *testing.F) {
	for _, s := range opBodySeeds(f) {
		f.Add(s)
	}
	f.Add("")
	f.Add("AAAA")
	f.Fuzz(func(t *testing.T, b64 string) {
		d, err := DecodeOperationBody(b64)
		var body xdr.OperationBody
		if uerr := strictDecodeB64(b64, &body); uerr != nil {
			if err == nil {
				t.Fatalf("DecodeOperationBody accepted a body the XDR decoder rejects: %v", uerr)
			}
			return
		}
		if err != nil {
			t.Fatalf("DecodeOperationBody: %v", err)
		}
		if d.Type != OpTypeName(body.Type) {
			t.Fatalf("Type = %q, want %q", d.Type, OpTypeName(body.Type))
		}
		if (len(d.Fields) == 0) != (d.RawXDR == b64) {
			t.Fatalf("RawXDR must be the body exactly when no fields decoded: fields=%d raw=%q", len(d.Fields), d.RawXDR)
		}
		if _, err := json.Marshal(d.Fields); err != nil {
			t.Fatalf("fields not JSON-serialisable: %v", err)
		}

		want := map[string]any{}
		dec := func(v int64) string { return strconv.FormatInt(v, 10) }
		pr := func(p xdr.Price) map[string]any { return map[string]any{"n": int32(p.N), "d": int32(p.D)} }
		switch body.Type {
		case xdr.OperationTypeCreateAccount:
			want["starting_balance"] = dec(int64(body.CreateAccountOp.StartingBalance))
		case xdr.OperationTypePayment:
			op := body.PaymentOp
			want["amount"], want["asset"] = dec(int64(op.Amount)), AssetID(op.Asset)
		case xdr.OperationTypePathPaymentStrictReceive:
			op := body.PathPaymentStrictReceiveOp
			want["send_max"], want["dest_amount"] = dec(int64(op.SendMax)), dec(int64(op.DestAmount))
			want["send_asset"], want["dest_asset"] = AssetID(op.SendAsset), AssetID(op.DestAsset)
		case xdr.OperationTypePathPaymentStrictSend:
			op := body.PathPaymentStrictSendOp
			want["send_amount"], want["dest_min"] = dec(int64(op.SendAmount)), dec(int64(op.DestMin))
			want["send_asset"], want["dest_asset"] = AssetID(op.SendAsset), AssetID(op.DestAsset)
		case xdr.OperationTypeManageSellOffer:
			op := body.ManageSellOfferOp
			want["amount"], want["offer_id"], want["price"] = dec(int64(op.Amount)), dec(int64(op.OfferId)), pr(op.Price)
			want["selling"], want["buying"] = AssetID(op.Selling), AssetID(op.Buying)
		case xdr.OperationTypeManageBuyOffer:
			op := body.ManageBuyOfferOp
			want["buy_amount"], want["offer_id"], want["price"] = dec(int64(op.BuyAmount)), dec(int64(op.OfferId)), pr(op.Price)
			want["selling"], want["buying"] = AssetID(op.Selling), AssetID(op.Buying)
		case xdr.OperationTypeCreatePassiveSellOffer:
			op := body.CreatePassiveSellOfferOp
			want["amount"], want["price"] = dec(int64(op.Amount)), pr(op.Price)
			want["selling"], want["buying"] = AssetID(op.Selling), AssetID(op.Buying)
		case xdr.OperationTypeChangeTrust:
			want["limit"] = dec(int64(body.ChangeTrustOp.Limit))
		case xdr.OperationTypeClawback:
			want["amount"], want["asset"] = dec(int64(body.ClawbackOp.Amount)), AssetID(body.ClawbackOp.Asset)
		case xdr.OperationTypeBumpSequence:
			want["bump_to"] = dec(int64(body.BumpSequenceOp.BumpTo))
		}
		for k, w := range want {
			g, ok := d.Fields[k]
			if !ok {
				t.Fatalf("%s: field %q missing", d.Type, k)
			}
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			if string(gj) != string(wj) {
				t.Fatalf("%s: field %q = %s, want %s", d.Type, k, gj, wj)
			}
		}

		// The participant extractor reads the same bodies: it must agree on
		// decodability and emit a sorted, de-duplicated set of G-accounts.
		ps, err := ParticipantAccounts(b64)
		if err != nil {
			t.Fatalf("ParticipantAccounts rejected a body DecodeOperationBody accepted: %v", err)
		}
		if !sort.StringsAreSorted(ps) {
			t.Fatalf("participants not sorted: %v", ps)
		}
		for i, p := range ps {
			if !canonical.IsAccountID(p) {
				t.Fatalf("participant %q is not a G-account", p)
			}
			if i > 0 && ps[i-1] == p {
				t.Fatalf("participant %q duplicated", p)
			}
		}
		switch body.Type {
		case xdr.OperationTypeCreateClaimableBalance:
			set := map[string]bool{}
			for _, c := range body.CreateClaimableBalanceOp.Claimants {
				if c.V0 != nil {
					set[c.V0.Destination.Address()] = true
				}
			}
			if len(ps) != len(set) {
				t.Fatalf("claimable-balance participants = %v, want the %d distinct claimants", ps, len(set))
			}
			for _, p := range ps {
				if !set[p] {
					t.Fatalf("participant %q is not a claimant", p)
				}
			}
		case xdr.OperationTypeInvokeHostFunction:
			hf := body.InvokeHostFunctionOp.HostFunction
			if hf.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
				break
			}
			addr := hf.InvokeContract.ContractAddress
			wantID, wantOK := "", false
			switch addr.Type {
			case xdr.ScAddressTypeScAddressTypeContract:
				wantID, wantOK = strkey.MustEncode(strkey.VersionByteContract, addr.ContractId[:]), true
			case xdr.ScAddressTypeScAddressTypeAccount:
				wantID, wantOK = addr.AccountId.Address(), true
			}
			gotID, gotOK := d.Fields["contract_id"]
			if gotOK != wantOK || (wantOK && gotID != wantID) {
				t.Fatalf("contract_id = %v (present=%v), want %q (present=%v)", gotID, gotOK, wantID, wantOK)
			}
		}
		if body.Type == xdr.OperationTypePayment {
			dest := body.PaymentOp.Destination.ToAccountId()
			g := dest.Address()
			if len(ps) != 1 || ps[0] != g {
				t.Fatalf("payment participants = %v, want [%s] (muxed destination resolved to its G-account)", ps, g)
			}
		}
	})
}

// FuzzResultName checks the result-code slugs are never blank and that an
// unmapped code renders its own number, never a neighbour's slug.
func FuzzResultName(f *testing.F) {
	for _, c := range []int32{0, 1, -1, -2, -17, -18, 2, 1 << 30, -1 << 31} {
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, code int32) {
		tx := TxResultName(code)
		if _, known := txResultNames[xdr.TransactionResultCode(code)]; known == (tx == "tx_unknown("+strconv.Itoa(int(code))+")") || tx == "" {
			t.Fatalf("TxResultName(%d) = %q (known=%v)", code, tx, known)
		}
		op := OpResultName(code)
		if _, known := opResultNames[xdr.OperationResultCode(code)]; known == (op == "op_unknown("+strconv.Itoa(int(code))+")") || op == "" {
			t.Fatalf("OpResultName(%d) = %q (known=%v)", code, op, known)
		}
	})
}

// strictDecodeB64 is the test oracle for a base64 XDR body: exactly one value,
// no bytes left over.
func strictDecodeB64(b64 string, dest any) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	return xdr.SafeUnmarshal(raw, dest)
}
