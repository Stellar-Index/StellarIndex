// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package classicmovements

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// TestMuxedCounterpartiesResolveToBaseAccount pins Q118: the three
// MUXED-typed classic counterparties (PaymentOp.Destination,
// ClawbackOp.From, AccountMergeOp.Destination) must be recorded as the
// G-strkey of the account that actually holds the balance.
//
// A muxed destination's M-strkey is a custodian's routing label, not an
// on-chain account. Written verbatim it lands in
// stellar.account_movements' address/counterparty columns, which are
// read by G-strkey equality (/v1/accounts/{g}/movements) — so the
// movement is unreachable for every reader AND missing from the
// underlying account's own feed. The same rule already governs the
// shared participant derivation and the lake's op-source extraction.

// mkMuxedAccount returns the M-strkey, the underlying G-strkey, and the
// xdr.MuxedAccount for a muxed (SEP-23) account with a memo id.
func mkMuxedAccount(t *testing.T, seed byte, id uint64) (mAddr, gAddr string, muxed xdr.MuxedAccount) {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = seed
	muxed = xdr.MuxedAccount{
		Type:     xdr.CryptoKeyTypeKeyTypeMuxedEd25519,
		Med25519: &xdr.MuxedAccountMed25519{Id: xdr.Uint64(id), Ed25519: pub},
	}
	mAddr, err := muxed.GetAddress()
	if err != nil {
		t.Fatalf("muxed GetAddress: %v", err)
	}
	gAddr, err = strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	if mAddr[0] != 'M' || gAddr[0] != 'G' {
		t.Fatalf("fixture is not a muxed/base pair: %s / %s", mAddr, gAddr)
	}
	return mAddr, gAddr, muxed
}

func decodeOneMovement(t *testing.T, op xdr.Operation, result xdr.OperationResult, txSource string) Movement {
	t.Helper()
	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: txSource, TxHash: "txmuxed1", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	return outs[0].(MovementEvent).Movement
}

func TestMuxedCounterpartiesResolveToBaseAccount(t *testing.T) {
	source, _ := mkAccount(t, 0x90)
	usdc := mkAlphanum4Asset(t, "USDC", 0x91)

	t.Run("payment destination", func(t *testing.T) {
		mAddr, gAddr, muxed := mkMuxedAccount(t, 0x92, 1234567890)
		op := xdr.Operation{Body: xdr.OperationBody{
			Type: xdr.OperationTypePayment,
			PaymentOp: &xdr.PaymentOp{
				Destination: muxed,
				Asset:       usdc,
				Amount:      xdr.Int64(50_0000000),
			},
		}}
		m := decodeOneMovement(t, op, mkPaymentSuccessResult(), source)

		if m.ToAddress != gAddr {
			t.Fatalf("ToAddress = %q, want the base account %q (got the M-strkey %q? — a movement under an "+
				"M-address is queryable by nobody and missing from the base account's feed; Q118)",
				m.ToAddress, gAddr, mAddr)
		}
		if m.FromAddress != source {
			t.Errorf("FromAddress = %q, want %q", m.FromAddress, source)
		}
	})

	t.Run("clawback holder", func(t *testing.T) {
		mAddr, gAddr, muxed := mkMuxedAccount(t, 0x93, 42)
		op := xdr.Operation{Body: xdr.OperationBody{
			Type: xdr.OperationTypeClawback,
			ClawbackOp: &xdr.ClawbackOp{
				Asset:  usdc,
				From:   muxed,
				Amount: xdr.Int64(7_0000000),
			},
		}}
		m := decodeOneMovement(t, op, mkClawbackSuccessResult(), source)

		if m.FromAddress != gAddr {
			t.Fatalf("FromAddress = %q, want the base account %q (M-strkey was %q; Q118)", m.FromAddress, gAddr, mAddr)
		}
		if m.ToAddress != source {
			t.Errorf("ToAddress = %q, want the clawing issuer %q", m.ToAddress, source)
		}
	})

	t.Run("account merge destination", func(t *testing.T) {
		mAddr, gAddr, muxed := mkMuxedAccount(t, 0x94, 7)
		op := xdr.Operation{Body: xdr.OperationBody{
			Type:        xdr.OperationTypeAccountMerge,
			Destination: &muxed,
		}}
		bal := xdr.Int64(249_9999000)
		result := xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type: xdr.OperationTypeAccountMerge,
				AccountMergeResult: &xdr.AccountMergeResult{
					Code:                 xdr.AccountMergeResultCodeAccountMergeSuccess,
					SourceAccountBalance: &bal,
				},
			},
		}
		m := decodeOneMovement(t, op, result, source)

		if m.ToAddress != gAddr {
			t.Fatalf("ToAddress = %q, want the base account %q (M-strkey was %q; Q118)", m.ToAddress, gAddr, mAddr)
		}
		if m.Amount.String() != "2499999000" {
			t.Errorf("Amount = %q, want 2499999000 (the merged balance is unaffected by the address fix)", m.Amount.String())
		}
	})

	t.Run("plain ed25519 counterparties are unchanged", func(t *testing.T) {
		destAddr, dest := mkAccount(t, 0x95)
		op := xdr.Operation{Body: xdr.OperationBody{
			Type: xdr.OperationTypePayment,
			PaymentOp: &xdr.PaymentOp{
				Destination: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519},
				Asset:       usdc,
				Amount:      xdr.Int64(1),
			},
		}}
		m := decodeOneMovement(t, op, mkPaymentSuccessResult(), source)
		if m.ToAddress != destAddr {
			t.Fatalf("ToAddress = %q, want %q — a non-muxed destination must round-trip byte-identically", m.ToAddress, destAddr)
		}
	})
}
