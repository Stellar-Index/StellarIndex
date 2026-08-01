// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package dispatcher_test (EXTERNAL) hosts the C2-010 lock-step guard.
// It has to live out here: the guard drives the REAL SDEX decoder via
// sdex.AuditOp, and internal/sources/sdex imports internal/dispatcher —
// an in-package _test.go importing it would be an import cycle.
package dispatcher_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sdexclaim"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
)

// ─── fixtures ────────────────────────────────────────────────────────

func lsIssuer(t *testing.T, seed byte) xdr.AccountId {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = seed
	return xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
}

// lsAlphaNum4 builds a 4-byte-code classic asset from RAW code bytes, so
// a test can plant a byte stellar-core permits on chain but
// canonical.validateClassicAssetCode rejects.
func lsAlphaNum4(t *testing.T, code [4]byte, issuerSeed byte) xdr.Asset {
	t.Helper()
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: xdr.AssetCode4(code),
			Issuer:    lsIssuer(t, issuerSeed),
		},
	}
}

func lsOrderBookClaim(t *testing.T, soldAsset, boughtAsset xdr.Asset, sold, bought int64) xdr.ClaimAtom {
	t.Helper()
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
		OrderBook: &xdr.ClaimOfferAtom{
			SellerId:     lsIssuer(t, 9),
			OfferId:      xdr.Int64(1),
			AssetSold:    soldAsset,
			AmountSold:   xdr.Int64(sold),
			AssetBought:  boughtAsset,
			AmountBought: xdr.Int64(bought),
		},
	}
}

// lsLiquidityPoolClaim builds a classic-LP claim atom (counterparty is a
// classic liquidity pool, not the Soroban AMMs). Same sold/bought shape as
// OrderBook, different discriminant — the decoder handles all three.
func lsLiquidityPoolClaim(t *testing.T, soldAsset, boughtAsset xdr.Asset, sold, bought int64) xdr.ClaimAtom {
	t.Helper()
	var pool xdr.PoolId
	pool[0] = 0x42
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool,
		LiquidityPool: &xdr.ClaimLiquidityAtom{
			LiquidityPoolId: pool,
			AssetSold:       soldAsset,
			AmountSold:      xdr.Int64(sold),
			AssetBought:     boughtAsset,
			AmountBought:    xdr.Int64(bought),
		},
	}
}

// lsV0Claim builds the legacy pre-CAP-27 V0 claim atom (F-1233): carries the
// seller's raw ed25519 bytes rather than an AccountId discriminant.
func lsV0Claim(t *testing.T, soldAsset, boughtAsset xdr.Asset, sold, bought int64) xdr.ClaimAtom {
	t.Helper()
	var seller xdr.Uint256
	seller[0] = 7
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeV0,
		V0: &xdr.ClaimOfferAtomV0{
			SellerEd25519: seller,
			OfferId:       xdr.Int64(1),
			AssetSold:     soldAsset,
			AmountSold:    xdr.Int64(sold),
			AssetBought:   boughtAsset,
			AmountBought:  xdr.Int64(bought),
		},
	}
}

// lsManageSellOfferOp wraps claims in the op shape all three counters
// walk (dispatcher.claimAtomCount, clickhouse.claimAtomCount,
// sdex.extractClaimAtoms).
func lsManageSellOfferOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	return xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer}},
		xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type: xdr.OperationTypeManageSellOffer,
				ManageSellOfferResult: &xdr.ManageSellOfferResult{
					Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
					Success: &xdr.ManageOfferSuccessResult{OffersClaimed: claims},
				},
			},
		}
}

// ─── the guard ───────────────────────────────────────────────────────

// TestClaimAtomCount_LockStepWithDecoder is the C2-010 regression guard
// (audit-2026-07-23).
//
// The census (ADR-0033) exists to prove the decoders lost nothing:
// `census.ClassicTradeEffectCount MUST equal COUNT(trades WHERE
// source='sdex')`. The lake's `classic_trade_effect_count` carries the
// same promise. Both counted through sdexclaim.RealTradeCount, whose
// only rule was "at least one leg is non-zero" — while the decoder
// ALSO drops unknown atom types, unconvertible assets and self-crosses.
// Every dropped-by-decoder-but-counted-by-oracle atom is a permanent,
// unclosable gap in the reconcile: real trade loss becomes
// indistinguishable from the baseline discrepancy.
//
// The table drives the ACTUAL decoder (sdex.AuditOp → extractClaimAtoms
// → decodeClaimAtom) and the ACTUAL shared counter, and demands the two
// agree atom for atom.
func TestClaimAtomCount_LockStepWithDecoder(t *testing.T) {
	t.Parallel()

	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := lsAlphaNum4(t, [4]byte{'U', 'S', 'D', 'C'}, 1)
	// A code stellar-core accepts (it does not enforce the alphanumeric
	// rule) but canonical.AssetFromXDR rejects: interior control byte.
	badCode := lsAlphaNum4(t, [4]byte{0x01, 'B', 'C', 0x00}, 2)

	cases := []struct {
		name      string
		atom      xdr.ClaimAtom
		wantTrade bool
		why       string
	}{
		{
			name:      "real trade (control)",
			atom:      lsOrderBookClaim(t, native, usdc, 100, 25),
			wantTrade: true,
			why:       "native→USDC, both legs positive: the ordinary case must still count",
		},
		{
			name:      "self-cross",
			atom:      lsOrderBookClaim(t, native, native, 100, 100),
			wantTrade: false,
			why:       "canonical.NewPair rejects base==quote, so the decoder emits no trade — DIVERGENT pre-fix",
		},
		{
			name:      "non-alphanumeric asset code",
			atom:      lsOrderBookClaim(t, badCode, usdc, 100, 25),
			wantTrade: false,
			why:       "validateClassicAssetCode rejects the control byte, so the decoder emits no trade — DIVERGENT pre-fix",
		},
		{
			name:      "both legs zero",
			atom:      lsOrderBookClaim(t, native, usdc, 0, 0),
			wantTrade: false,
			why:       "both-zero no-op claim; already dropped by both sides pre-fix (control for rule 2)",
		},
		{
			name:      "one leg zero — sold only (OrderBook)",
			atom:      lsOrderBookClaim(t, native, usdc, 100, 0),
			wantTrade: true,
			why:       "rule 2 is BOTH-zero; a fill whose bought leg rounded to 0 is a real trade and must be KEPT",
		},
		{
			name:      "one leg zero — bought only (OrderBook)",
			atom:      lsOrderBookClaim(t, native, usdc, 0, 100),
			wantTrade: true,
			why:       "sold leg rounded to 0 but bought>0: still a real trade, KEPT (sdexclaim.go:82 sold<=0 && bought<=0)",
		},
		{
			name:      "one leg zero — LiquidityPool",
			atom:      lsLiquidityPoolClaim(t, native, usdc, 100, 0),
			wantTrade: true,
			why:       "the one-leg-zero KEEP rule is atom-type-agnostic; LP fills must count identically to OrderBook",
		},
		{
			name:      "one leg zero — V0",
			atom:      lsV0Claim(t, native, usdc, 0, 100),
			wantTrade: true,
			why:       "legacy V0 atom, bought-only leg: KEPT — decoder and census must agree here too",
		},
		{
			name:      "unknown atom type",
			atom:      xdr.ClaimAtom{Type: xdr.ClaimAtomType(0x7fff)},
			wantTrade: false,
			why:       "ErrUnknownClaimAtomType; already dropped by both sides pre-fix (control for rule 1)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// What the REAL decoder does with this atom.
			op, res := lsManageSellOfferOp([]xdr.ClaimAtom{tc.atom})
			claims, drops := sdex.AuditOp(op, res)
			if claims != 1 {
				t.Fatalf("AuditOp saw %d claim atoms, want 1 (fixture broken)", claims)
			}
			decoderEmits := 1 - len(drops)

			// What the SHARED counter — used by dispatcher.claimAtomCount
			// (census) AND clickhouse.claimAtomCount (lake) — says.
			counted := sdexclaim.RealTradeCount([]xdr.ClaimAtom{tc.atom})

			wantN := 0
			if tc.wantTrade {
				wantN = 1
			}
			if decoderEmits != wantN {
				t.Errorf("decoder emitted %d trade(s), want %d — %s (drops: %+v)",
					decoderEmits, wantN, tc.why, drops)
			}
			if counted != wantN {
				t.Errorf("sdexclaim.RealTradeCount = %d, want %d — %s",
					counted, wantN, tc.why)
			}
			if counted != decoderEmits {
				t.Errorf("LOCK-STEP BROKEN: counter says %d, decoder emits %d. "+
					"The census and lake oracles now over/under-count this atom class, so "+
					"`ClassicTradeEffectCount == COUNT(trades)` can never reconcile — %s",
					counted, decoderEmits, tc.why)
			}
			// IsRealTrade is the single-atom spelling the counter loops.
			if got := sdexclaim.IsRealTrade(tc.atom); got != tc.wantTrade {
				t.Errorf("IsRealTrade = %v, want %v — %s", got, tc.wantTrade, tc.why)
			}
		})
	}
}

// TestClaimAtomCount_BothCountersDelegate is the structural half of the
// lock-step. The behavioural table above proves sdexclaim agrees with
// the decoder; this proves the two claimAtomCount COPIES still route
// through sdexclaim rather than re-implementing the predicate — the
// drift mode the "mirrors X exactly" comments could only ask for, never
// enforce. (Same technique as internal/pipeline/lockstep_ast_test.go.)
//
// clickhouse.claimAtomCount is unexported, so this reads the source
// rather than calling it; that is the point — a future edit that
// inlines an amount check back into either copy fails here.
func TestClaimAtomCount_BothCountersDelegate(t *testing.T) {
	t.Parallel()

	for _, f := range []struct{ label, path string }{
		{"dispatcher census", filepath.Join("census.go")},
		{"clickhouse lake extract", filepath.Join("..", "storage", "clickhouse", "extract.go")},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.path, nil, 0)
		if err != nil {
			t.Fatalf("%s: parse %s: %v", f.label, f.path, err)
		}
		var fn *ast.FuncDecl
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "claimAtomCount" && fd.Recv == nil {
				fn = fd
				break
			}
		}
		if fn == nil {
			t.Fatalf("%s: no top-level claimAtomCount in %s", f.label, f.path)
		}

		calls := map[string]int{}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					calls[pkg.Name+"."+sel.Sel.Name]++
				}
			}
			return true
		})
		// Six trade-op arms (manage-sell, manage-buy, passive×2 result
		// arms, path-payment strict-receive, strict-send).
		if got := calls["sdexclaim.RealTradeCount"]; got != 6 {
			t.Errorf("%s: claimAtomCount calls sdexclaim.RealTradeCount %d time(s), want 6 "+
				"(one per trade-op result arm). A missing call means that op type's fills are "+
				"counted by a DIFFERENT rule than the decoder applies (C2-010)", f.label, got)
		}
		for _, forbidden := range []string{"sdexclaim.Amounts", "sdexclaim.IsRealTrade"} {
			if calls[forbidden] > 0 {
				t.Errorf("%s: claimAtomCount calls %s directly — both counters must go through "+
					"RealTradeCount so the census and the lake stay identical", f.label, forbidden)
			}
		}
	}
}
