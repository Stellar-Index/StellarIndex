// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package bitstamp

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// FuzzParseFrameTrade: amount_str/price_str through a real-shape
// live_trades frame; base is the exact 10^8 scaling, quote is
// base×price/10^8 truncated, dust is exactly a zero quote, and a field
// the scaler rejects never yields a trade.
func FuzzParseFrameTrade(f *testing.F) {
	f.Add("100.5", "0.17582", "xlmusd", int64(123456789))
	f.Add("0.00000001", "0.16", "xlmusd", int64(1))
	f.Add("170141183460469231731687303715884105727", "99999.99999999", "btcusd", int64(2))
	f.Add("--1", "--2", "xlmusd", int64(3))
	f.Add("1", "1", "dogeusd", int64(4))
	pairs, err := DefaultPairs()
	if err != nil {
		f.Fatalf("DefaultPairs: %v", err)
	}
	f.Fuzz(func(t *testing.T, amount, price, symbol string, id int64) {
		raw, err := json.Marshal(map[string]any{
			"event": "trade", "channel": ChannelPrefix + symbol,
			"data": map[string]any{
				"id": id, "timestamp": "1745000000", "microtimestamp": "1745000000123456",
				"amount_str": amount, "price_str": price, "type": 0,
			},
		})
		if err != nil {
			t.Skip()
		}
		trade, isTrade, perr := parseFrame(raw, pairs)

		wantBase, aErr := scale.DecimalStringToScaledInt(amount, externalAmountDecimals)
		wantPrice, pErr := scale.DecimalStringToScaledInt(price, externalAmountDecimals)
		if aErr != nil || pErr != nil {
			if perr == nil && isTrade {
				t.Fatalf("amount=%q price=%q: parsed a trade from a malformed amount", amount, price)
			}
			return
		}
		if perr != nil {
			if errors.Is(perr, ErrDustTrade) && refQuote(wantBase, wantPrice).Sign() != 0 {
				t.Fatalf("amount=%q price=%q: dust with nonzero quote", amount, price)
			}
			return
		}
		pair, ok := pairs[strings.ToLower(symbol)]
		if !ok || !isTrade || trade.Pair != pair {
			t.Fatalf("symbol %q: isTrade=%v pair=%v", symbol, isTrade, trade.Pair)
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
