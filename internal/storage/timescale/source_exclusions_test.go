package timescale

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestSourceExclusion_Covers(t *testing.T) {
	from := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	sac := canonical.Asset{Type: canonical.AssetSoroban, ContractID: canonical.NativeSACContractID()}.String()
	pair := SourceExclusion{Source: "binance", Base: "crypto:BTC", Quote: "native", From: from, Until: until}
	wide := SourceExclusion{Source: "binance", From: from}
	lifted := pair
	lifted.LiftedAt = from

	cases := []struct {
		name        string
		row         SourceExclusion
		source      string
		base, quote string
		ts          time.Time
		want        bool
	}{
		{"stored orientation", pair, "binance", "crypto:BTC", "native", from, true},
		{"reversed pair", pair, "binance", "native", "crypto:BTC", from, true},
		{"XLM as crypto:XLM", pair, "binance", "crypto:BTC", "crypto:XLM", from, true},
		{"XLM as its SAC", pair, "binance", "crypto:BTC", sac, from, true},
		{"XLM as its SAC, reversed", pair, "binance", sac, "crypto:BTC", from, true},
		{"other pair", pair, "binance", "crypto:ETH", "native", from, false},
		{"before from", pair, "binance", "crypto:BTC", "native", from.Add(-time.Nanosecond), false},
		{"last instant before until", pair, "binance", "crypto:BTC", "native", until.Add(-time.Nanosecond), true},
		{"at until (exclusive)", pair, "binance", "crypto:BTC", "native", until, false},
		{"lifted row", lifted, "binance", "crypto:BTC", "native", from, false},
		{"other source", pair, "kraken", "crypto:BTC", "native", from, false},
		{"unscoped row, any pair", wide, "binance", "crypto:ETH", "fiat:USD", from, true},
		{"unscoped row, open-ended", wide, "binance", "crypto:ETH", "fiat:USD", from.AddDate(10, 0, 0), true},
		{"unscoped row, other source", wide, "kraken", "crypto:ETH", "fiat:USD", from, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.Covers(tc.source, tc.base, tc.quote, tc.ts); got != tc.want {
				t.Fatalf("Covers(%s, %s/%s, %s) = %t, want %t", tc.source, tc.base, tc.quote, tc.ts, got, tc.want)
			}
		})
	}
}

func TestSourceExclusion_State(t *testing.T) {
	from := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	row := SourceExclusion{Source: "binance", From: from, Until: from.Add(time.Hour)}
	lifted := row
	lifted.LiftedAt = from
	cases := []struct {
		row  SourceExclusion
		now  time.Time
		want string
	}{
		{row, from.Add(-time.Second), SourceExclusionPending},
		{row, from, SourceExclusionInForce},
		{row, from.Add(time.Hour), SourceExclusionExpired},
		{SourceExclusion{From: from}, from.AddDate(5, 0, 0), SourceExclusionInForce},
		{lifted, from.Add(time.Minute), SourceExclusionLifted},
	}
	for _, tc := range cases {
		if got := tc.row.State(tc.now); got != tc.want {
			t.Errorf("State(%s) of %+v = %q, want %q", tc.now, tc.row, got, tc.want)
		}
	}
}
