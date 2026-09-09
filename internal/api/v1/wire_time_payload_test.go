// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// wireTimeLocalZone is a deliberately non-UTC location, fixed rather
// than loaded from tzdata so the test behaves identically on a machine
// with no zoneinfo. +02:00 is the offset production actually leaked:
// r1 runs Europe/Berlin and pgx decodes `timestamptz` into the
// process's local zone, so every stored instant arrived at the
// handlers wearing an offset.
var wireTimeLocalZone = time.FixedZone("WIRETEST+02", 2*60*60)

// wireTimeFixture is the instant every fixture below is built from,
// expressed IN THAT ZONE. Rendered honestly it is
// 2026-06-01T10:00:00Z; rendered with the location attached it is
// 2026-06-01T12:00:00+02:00. Both are the same instant and both pass
// `format: date-time`, which is exactly why the leak survived every
// schema check the contract test runs.
var wireTimeFixture = time.Date(2026, 6, 1, 12, 0, 0, 0, wireTimeLocalZone)

// rfc3339Like matches any JSON string shaped like an RFC 3339
// timestamp, capturing the offset. Deliberately loose on the payload
// side: the point is to find timestamps the test author did not know
// about, including ones nested inside response types this file never
// names.
var rfc3339Like = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(Z|z|[+-]\d{2}:?\d{2})$`)

// scanTimestamps walks a decoded JSON document and returns every
// timestamp-shaped string it finds, as (json path, value) pairs.
func scanTimestamps(node any, path string, out *[][2]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			scanTimestamps(child, path+"."+k, out)
		}
	case []any:
		for i, child := range v {
			if i > 8 { // a page of identical rows proves nothing extra
				break
			}
			scanTimestamps(child, path+"[]", out)
		}
	case string:
		if rfc3339Like.MatchString(v) {
			*out = append(*out, [2]string{path, v})
		}
	}
}

// wireTimePayloadServer wires one server with every reader whose
// surface carries a timestamp, all fixtures stamped in the non-UTC
// zone above.
func wireTimePayloadServer(t *testing.T) *testServer {
	t.Helper()

	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse fiat:USD: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	trade := canonical.Trade{
		Source: "soroswap", Ledger: 52_430_001,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   wireTimeFixture,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1_0000000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(1_200000)),
	}
	points := []v1.HistoryPoint{
		{Bucket: wireTimeFixture.Add(-48 * time.Hour), VWAP: "0.12"},
		{Bucket: wireTimeFixture.Add(-24 * time.Hour), VWAP: "0.13"},
		{Bucket: wireTimeFixture, VWAP: "0.14"},
	}
	bars := []v1.OHLCSeriesBar{{
		T: v1.WireTime(wireTimeFixture), O: "0.12", H: "0.14", L: "0.11", C: "0.13",
		VBase: "1000000000", VQuote: "130000000", N: 7, Sources: []string{"sdex"},
	}}

	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID: "native", Quote: "fiat:USD", Price: "0.13",
				PriceType: "vwap", ObservedAt: v1.WireTime(wireTimeFixture),
				WindowSeconds: 60,
			},
		}},
		PriceAt: wireTimePriceAtStub{value: "0.13", bucketAt: wireTimeFixture},
		History: &stubHistoryReader{
			trades:       []canonical.Trade{trade},
			observations: []canonical.Trade{trade},
			points:       points,
			ohlcBars:     bars,
		},
		Oracle: &stubOracleReader{updates: []canonical.OracleUpdate{{
			Source:     "reflector-dex",
			ContractID: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
			Ledger:     52_430_001,
			TxHash:     "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
			OpIndex:    0,
			Timestamp:  wireTimeFixture,
			Asset:      canonical.NativeAsset(),
			Quote:      usd,
			Price:      canonical.NewAmount(big.NewInt(13_000_000_000_000)),
			Decimals:   14,
		}}},
		Lending: &stubLendingReader{pools: []timescale.BlendPoolSummary{{
			Pool:     "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			LastSeen: wireTimeFixture,
			// Flow proxies are decimal strings; content is irrelevant here.
			NetSupplied30d: "0", NetBorrowed30d: "0",
		}}},
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{{
			Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 62_999_000,
			CoveragePct: 99.99, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
			ComputedAt: wireTimeFixture,
		}}},
	})
	return httpTestServer(t, srv)
}

// wireTimePriceAtStub is the point-in-time reader, answering every
// pair with one bucket stamped in the non-UTC fixture zone.
type wireTimePriceAtStub struct {
	value    string
	bucketAt time.Time
}

func (s wireTimePriceAtStub) PriceAt(
	context.Context, canonical.Pair, time.Time, time.Duration,
) (string, time.Time, int, error) {
	return s.value, s.bucketAt, 60, nil
}

// TestWireTime_RenderedPayloadsCarryNoLocalOffset is the class test
// for the timezone leak. It feeds every timestamp-bearing reader a
// fixture stamped in a NON-UTC zone — the condition that produced the
// bug in production — then walks the RENDERED JSON of many endpoints
// and fails on any timestamp that is not `Z`.
//
// It scans payloads rather than naming fields on purpose. A handler
// that grows a new timestamp, or a response struct that gains a
// nested one, is covered the moment its endpoint appears in the table
// below — no per-field assertion to remember. The structural half of
// the guarantee (a NEW endpoint cannot opt out) is
// TestWireTime_NoRawTimeOnTheWire.
func TestWireTime_RenderedPayloadsCarryNoLocalOffset(t *testing.T) {
	ts := wireTimePayloadServer(t)

	at := wireTimeFixture.UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		path string
	}{
		{"price", "/v1/price?asset=native&quote=fiat:USD"},
		{"price/at", "/v1/price/at?asset=native&quote=fiat:USD&ts=" + at},
		{"history", "/v1/history?base=native&quote=fiat:USD&limit=5"},
		{"history/since-inception", "/v1/history/since-inception?asset=native&quote=fiat:USD"},
		{"chart", "/v1/chart?asset=native&quote=fiat:USD&window=7d"},
		{"ohlc", "/v1/ohlc?base=native&quote=fiat:USD&granularity=1d&limit=5"},
		{"observations", "/v1/observations?asset=native&quote=fiat:USD"},
		{"oracle/latest", "/v1/oracle/latest?asset=native&quote=fiat:USD"},
		{"lending/pools", "/v1/lending/pools"},
		{"coverage", "/v1/coverage"},
		{"status", "/v1/status"},
		{"version", "/v1/version"},
	}

	total := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, ts.URL+tc.path)
			if resp.StatusCode != http.StatusOK {
				// A non-200 would scan an error document and pass
				// vacuously. Fail loudly instead: the fixture is wrong.
				t.Fatalf("GET %s = %d, want 200", tc.path, resp.StatusCode)
			}
			var doc any
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatalf("decode %s: %v", tc.path, err)
			}
			var found [][2]string
			scanTimestamps(doc, "", &found)
			if len(found) == 0 {
				// Every endpoint here at minimum carries the envelope's
				// `as_of`. Zero means the fixture never rendered.
				t.Fatalf("GET %s rendered no timestamps — the case proves nothing", tc.path)
			}
			for _, ft := range found {
				if !strings.HasSuffix(ft[1], "Z") && !strings.HasSuffix(ft[1], "z") {
					t.Errorf("GET %s: %s = %q carries a local offset, want UTC `Z`",
						tc.path, ft[0], ft[1])
				}
			}
			total += len(found)
		})
	}

	// Guard against the whole table degrading into a vacuous pass — a
	// stub that silently stops producing rows would otherwise leave
	// every case checking nothing but `as_of`.
	if minTimestamps := 2 * len(cases); total < minTimestamps {
		t.Errorf("scanned %d timestamps across %d endpoints, want at least %d — "+
			"the fixtures are no longer exercising the payload bodies",
			total, len(cases), minTimestamps)
	}
}

// TestWireTime_ScannerCatchesALocalOffset proves the scanner above can
// actually fail. A test that only ever sees compliant input cannot
// distinguish "everything emits Z" from "the regexp matches nothing".
func TestWireTime_ScannerCatchesALocalOffset(t *testing.T) {
	doc := map[string]any{
		"data": map[string]any{
			"points": []any{
				map[string]any{"t": "2017-01-17T01:00:00+01:00", "p": "0.002"},
			},
		},
		"as_of": "2026-06-01T10:00:00Z",
		"note":  "not a timestamp",
	}
	var found [][2]string
	scanTimestamps(doc, "", &found)
	if len(found) != 2 {
		t.Fatalf("scanner found %d timestamps, want 2: %v", len(found), found)
	}
	var offenders int
	for _, ft := range found {
		if !strings.HasSuffix(ft[1], "Z") {
			offenders++
		}
	}
	if offenders != 1 {
		t.Errorf("scanner flagged %d local-offset timestamps, want 1", offenders)
	}
}
