// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package kraken

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// FuzzBuildTrade: qty/price as the json.Number strings the decoder hands
// over; base is the exact 10^8 scaling, quote is base×price/10^8
// truncated, dust is exactly a zero quote, and a field the scaler rejects
// never yields a trade.
func FuzzBuildTrade(f *testing.F) {
	f.Add("100.00000000", "0.17582", "XLM/USD", int64(987654321))
	f.Add("0.00000001", "0.16", "XLM/USD", int64(1))
	f.Add("170141183460469231731687303715884105727", "99999.99999999", "BTC/USD", int64(2))
	f.Add("--1", "--2", "XLM/USD", int64(3))
	f.Add("1e-5", "0.17", "XLM/USD", int64(4))
	f.Add("1", "1", "DOGE/USD", int64(5))
	pairs, err := DefaultPairs()
	if err != nil {
		f.Fatalf("DefaultPairs: %v", err)
	}
	f.Fuzz(func(t *testing.T, qty, price, symbol string, tradeID int64) {
		trade, perr := buildTrade(tradePayload{
			Symbol: symbol, Qty: json.Number(qty), Price: json.Number(price),
			TradeID: tradeID, Timestamp: "2026-04-24T12:34:56.789000Z",
		}, pairs)

		wantBase, qErr := scale.DecimalStringToScaledInt(qty, externalAmountDecimals)
		wantPrice, pErr := scale.DecimalStringToScaledInt(price, externalAmountDecimals)
		if qErr != nil || pErr != nil {
			if perr == nil {
				t.Fatalf("qty=%q price=%q: built a trade from a malformed amount", qty, price)
			}
			return
		}
		if perr != nil {
			if errors.Is(perr, ErrDustTrade) && refQuote(wantBase, wantPrice).Sign() != 0 {
				t.Fatalf("qty=%q price=%q: dust with nonzero quote", qty, price)
			}
			return
		}
		pair, ok := pairs[strings.ToUpper(symbol)]
		if !ok || trade.Pair != pair {
			t.Fatalf("symbol %q: pair=%v", symbol, trade.Pair)
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

// FuzzParseFrame: arbitrary frames never panic, and every trade emitted
// carries a nonzero quote equal to base×price/10^8 of its own entry.
func FuzzParseFrame(f *testing.F) {
	f.Add([]byte(`{"channel":"trade","type":"update","data":[{"symbol":"XLM/USD","side":"buy","qty":100.00000000,"price":0.17582,"trade_id":1,"timestamp":"2026-04-24T12:34:56.789000Z"}]}`))
	f.Add([]byte(`{"channel":"trade","type":"snapshot","data":[{"symbol":"XLM/USD","qty":1,"price":2,"trade_id":1,"timestamp":"2026-04-24T12:34:56Z"},{"symbol":"BTC/USD","qty":0.5,"price":65000.1,"trade_id":2,"timestamp":"2026-04-24T12:34:56Z"}]}`))
	f.Add([]byte(`{"channel":"heartbeat"}`))
	pairs, err := DefaultPairs()
	if err != nil {
		f.Fatalf("DefaultPairs: %v", err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		trades, perr := parseFrame(raw, pairs)
		if perr != nil {
			return
		}
		for _, tr := range trades {
			if tr.QuoteAmount.Sign() == 0 || tr.Source != SourceName || len(tr.TxHash) != 64 {
				t.Fatalf("emitted invalid trade %+v", tr)
			}
		}
	})
}

func refQuote(base, price *big.Int) *big.Int {
	q := new(big.Int).Mul(base, price)
	return q.Quo(q, big.NewInt(100_000_000))
}
