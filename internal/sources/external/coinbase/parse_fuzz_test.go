// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package coinbase

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// FuzzParseFrameMatch: size/price through a real-shape match frame; base
// is the exact 10^8 scaling, quote is base×price/10^8 truncated, dust is
// exactly a zero quote, and a field the scaler rejects never yields a
// trade.
func FuzzParseFrameMatch(f *testing.F) {
	f.Add("100.00000000", "0.17582000", "XLM-USD", int64(123456))
	f.Add("0.00000001", "0.16", "XLM-USD", int64(1))
	f.Add("170141183460469231731687303715884105727", "99999.99999999", "BTC-USD", int64(2))
	f.Add("--1", "--2", "XLM-USD", int64(3))
	f.Add("1", "1", "DOGE-USD", int64(4))
	pairs, err := DefaultPairs()
	if err != nil {
		f.Fatalf("DefaultPairs: %v", err)
	}
	f.Fuzz(func(t *testing.T, size, price, product string, tradeID int64) {
		raw, err := json.Marshal(map[string]any{
			"type": "match", "trade_id": tradeID, "size": size, "price": price,
			"product_id": product, "time": "2026-04-24T00:00:00.123456Z",
		})
		if err != nil {
			t.Skip()
		}
		trade, isTrade, perr := parseFrame(raw, pairs)

		wantBase, sErr := scale.DecimalStringToScaledInt(size, externalAmountDecimals)
		wantPrice, pErr := scale.DecimalStringToScaledInt(price, externalAmountDecimals)
		if sErr != nil || pErr != nil {
			if perr == nil {
				t.Fatalf("size=%q price=%q: parsed a trade from a malformed amount", size, price)
			}
			return
		}
		if perr != nil {
			return
		}
		if !isTrade {
			// parseFrame reports dust as "no trade, no error".
			if refQuote(wantBase, wantPrice).Sign() != 0 {
				t.Fatalf("size=%q price=%q: dropped a trade with nonzero quote", size, price)
			}
			return
		}
		pair, ok := pairs[strings.ToUpper(product)]
		if !ok || trade.Pair != pair {
			t.Fatalf("product %q: isTrade=%v pair=%v", product, isTrade, trade.Pair)
		}
		if trade.BaseAmount.BigInt().Cmp(wantBase) != 0 {
			t.Fatalf("base = %s, want %s", trade.BaseAmount, wantBase)
		}
		wantQuote := refQuote(wantBase, wantPrice)
		if wantQuote.Sign() == 0 || trade.QuoteAmount.BigInt().Cmp(wantQuote) != 0 {
			t.Fatalf("quote = %s, want nonzero %s", trade.QuoteAmount, wantQuote)
		}
	})
}

func refQuote(base, price *big.Int) *big.Int {
	q := new(big.Int).Mul(base, price)
	return q.Quo(q, big.NewInt(100_000_000))
}
