// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package aggregate_test

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
)

// docs/methodology/vwap-aggregation.md and docs/protocols/README.md
// are public methodology pages, not generated from code — a reader
// (or a protocol team asked to verify their contract set) trusts the
// prose directly. A truth-audit found that closure of a prior drift
// sweep against those two pages couldn't be confirmed by reading
// alone, because nothing in the repo re-checks the load-bearing
// numbers the pages state against the source they describe. This
// file is that check for the facts most likely to silently drift:
// the stablecoin fiat-proxy set and the two bridges' pinned-contract
// counts. It is a compile step for the overlap, same shape as
// internal/api/v1/methodology_explorer_drift_test.go.

const (
	vwapMethodologyDocPath = "../../docs/methodology/vwap-aggregation.md"
	protocolsReadmeDocPath = "../../docs/protocols/README.md"
)

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestVWAPDocStablecoinProxyMapMatchesSource pins the "Stablecoin ->
// fiat proxy" table's three backer lists against
// aggregate.FiatBackers, the exported enumeration the aggregator
// itself uses to build its fetch plan. Adding or removing a
// stablecoin in internal/aggregate/stablecoin.go without touching
// the doc's table — or vice versa — fails this test.
func TestVWAPDocStablecoinProxyMapMatchesSource(t *testing.T) {
	doc := readDoc(t, vwapMethodologyDocPath)

	cases := []struct {
		fiat string
		re   *regexp.Regexp
	}{
		{"USD", regexp.MustCompile(`\|\s*([A-Za-z, ]+?)\s*\|\s*USD\s*\|`)},
		{"EUR", regexp.MustCompile(`\|\s*([A-Za-z, ]+?)\s*\|\s*EUR\s*\|`)},
		{"MXN", regexp.MustCompile(`\|\s*([A-Za-z, ]+?)\s*\|\s*MXN\s*\|`)},
	}

	for _, c := range cases {
		m := c.re.FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("%s: could not find a %q proxy-target table cell — "+
				"table format changed, update this test's parser", vwapMethodologyDocPath, c.fiat)
		}
		var docBackers []string
		for _, tok := range strings.Split(m[1], ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				docBackers = append(docBackers, tok)
			}
		}

		codeBackers := aggregate.FiatBackers(c.fiat)

		got, want := sortedCopy(docBackers), sortedCopy(codeBackers)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s proxy backers: doc lists %v, aggregate.FiatBackers(%q) returns %v — "+
				"update whichever one is stale", c.fiat, got, c.fiat, want)
		}
	}
}

// TestProtocolsReadmeBridgeContractCountsMatchSource pins the CCTP
// and Rozo "N pinned contracts" claims in the bridges table against
// each source's exported mainnet contract set — the fact a protocol
// team is asked to confirm when sent this page.
func TestProtocolsReadmeBridgeContractCountsMatchSource(t *testing.T) {
	doc := readDoc(t, protocolsReadmeDocPath)

	cctpRe := regexp.MustCompile(`CCTP \(Circle\) \| ✅ Gated — (\d+) pinned contracts`)
	m := cctpRe.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s: could not find the CCTP pinned-contract-count cell — "+
			"table format changed, update this test's parser", protocolsReadmeDocPath)
	}
	docCount, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse CCTP contract count %q: %v", m[1], err)
	}
	if got := len(cctp.MainnetContracts()); got != docCount {
		t.Errorf("%s claims CCTP has %d pinned contracts, cctp.MainnetContracts() returns %d",
			protocolsReadmeDocPath, docCount, got)
	}

	rozoRe := regexp.MustCompile(`Rozo \| ✅ Gated — (\d+) v1 Payment contracts`)
	m = rozoRe.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s: could not find the Rozo v1-Payment-contract-count cell — "+
			"table format changed, update this test's parser", protocolsReadmeDocPath)
	}
	docCount, err = strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse Rozo contract count %q: %v", m[1], err)
	}
	if got := len(rozo.MainnetPaymentContracts); got != docCount {
		t.Errorf("%s claims Rozo has %d v1 Payment contracts, len(rozo.MainnetPaymentContracts) returns %d",
			protocolsReadmeDocPath, docCount, got)
	}
}
