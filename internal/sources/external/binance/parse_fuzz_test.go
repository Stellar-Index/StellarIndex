// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package binance

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// FuzzParseAggTradeFrame drives quantity/price through a real-shape
// aggTrade frame and checks the money arithmetic against big.Int:
// base is the exact 10^8 scaling, quote is base×price/10^8 truncated,
// dust is exactly a zero quote, and a field the scaler rejects never
// yields a trade.
func FuzzParseAggTradeFrame(f *testing.F) {
	f.Add("152.34", "0.17582", "XLMUSDT", int64(987654321))
	f.Add("0.00000001", "0.16", "XLMUSDT", int64(1))
	f.Add("170141183460469231731687303715884105727", "99999999999.99999999", "BTCUSDT", int64(2))
	f.Add("--1", "--2", "XLMUSDT", int64(3))
	f.Add("1", "1e-3", "XLMUSDT", int64(4))
	f.Add("1", "1", "ETHUSDT", int64(5))
	f.Fuzz(func(t *testing.T, qty, price, symbol string, aggID int64) {
		raw, err := json.Marshal(map[string]any{
			"stream": "xlmusdt@aggTrade",
			"data": map[string]any{
				"e": "aggTrade", "E": 1745000000000, "s": symbol, "a": aggID,
				"p": price, "q": qty, "T": 1745000000100, "m": true,
			},
		})
		if err != nil {
			t.Skip()
		}
		pairs := buildPairMap(t)
		trade, perr := parseAggTradeFrame(raw, pairs)

		wantBase, qErr := scale.DecimalStringToScaledInt(qty, externalAmountDecimals)
		wantPrice, pErr := scale.DecimalStringToScaledInt(price, externalAmountDecimals)
		if qErr != nil || pErr != nil {
			if perr == nil {
				t.Fatalf("q=%q p=%q: parsed a trade from a malformed amount", qty, price)
			}
			return
		}
		if perr != nil {
			if errors.Is(perr, ErrDustTrade) {
				if refQuote(wantBase, wantPrice).Sign() != 0 {
					t.Fatalf("q=%q p=%q: dust with nonzero quote %s", qty, price, refQuote(wantBase, wantPrice))
				}
			}
			return
		}
		if _, ok := pairs[strings.ToUpper(symbol)]; !ok {
			t.Fatalf("symbol %q not in pair map but parsed", symbol)
		}
		if trade.BaseAmount.BigInt().Cmp(wantBase) != 0 {
			t.Fatalf("base = %s, want %s", trade.BaseAmount, wantBase)
		}
		wantQuote := refQuote(wantBase, wantPrice)
		if wantQuote.Sign() == 0 || trade.QuoteAmount.BigInt().Cmp(wantQuote) != 0 {
			t.Fatalf("quote = %s, want nonzero %s", trade.QuoteAmount, wantQuote)
		}
		if trade.Pair != pairs[strings.ToUpper(symbol)] || len(trade.TxHash) != 64 {
			t.Fatalf("pair/txhash wrong: %v %q", trade.Pair, trade.TxHash)
		}
	})
}

func refQuote(base, price *big.Int) *big.Int {
	q := new(big.Int).Mul(base, price)
	return q.Quo(q, big.NewInt(100_000_000))
}
