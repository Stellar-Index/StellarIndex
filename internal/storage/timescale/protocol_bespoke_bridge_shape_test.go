// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestBridgeSeriesGrain guards the hourly-vs-daily bucketing rule for the
// bridge flow series: the 24h window (windowDays == 1) MUST bucket by hour —
// a daily bucket collapses it to a single useless point — and every longer
// window stays daily.
func TestBridgeSeriesGrain(t *testing.T) {
	cases := []struct {
		windowDays int
		wantTrunc  string
		wantFormat string
	}{
		{1, "hour", `YYYY-MM-DD"T"HH24:00`},
		{7, "day", "YYYY-MM-DD"},
		{30, "day", "YYYY-MM-DD"},
		{90, "day", "YYYY-MM-DD"},
	}
	for _, c := range cases {
		trunc, format := bridgeSeriesGrain(c.windowDays)
		if trunc != c.wantTrunc || format != c.wantFormat {
			t.Errorf("bridgeSeriesGrain(%d) = (%q, %q), want (%q, %q)",
				c.windowDays, trunc, format, c.wantTrunc, c.wantFormat)
		}
	}
}

// assertCCTPNumericSafe guards the ADR-0003 division rule shared by every
// CCTP flow query: exact NUMERIC at the canonical 6-decimal USDC scale,
// never a float literal.
func assertCCTPNumericSafe(t *testing.T, q string) {
	t.Helper()
	if !strings.Contains(q, "/ 1000000::numeric") {
		t.Error("cctp division must be exact NUMERIC at the 6-decimal USDC scale")
	}
	if strings.Contains(q, "1e6") || strings.Contains(q, "1e+06") {
		t.Error("cctp division must never use a float literal (ADR-0003)")
	}
}

// assertCCTPRawValueReads guards the post-deletion honesty rule (file-doc
// rule 1): the legacy event_index-0 twins were DELETED on 2026-07-31, so
// value-carrying flow queries read RAW per-event rows. The old per-(tx, op)
// collapse with max(amount) was a workaround for those twins, and keeping
// it would silently HALVE a future genuine batched double-transfer in one
// op (the 0112 class — admin events already prove same-op same-type groups
// occur on the wire: attester_enabled ×2, remote_token_messenger_added ×23
// in single ops on the 2026-07-31 lake census). max(amount) is the
// fingerprint of that collapse — cctpRecvCTE's legitimate one-body-per-op
// group uses min(body), so its presence in a full query is unambiguous.
func assertCCTPRawValueReads(t *testing.T, q string) {
	t.Helper()
	if strings.Contains(q, "max(amount)") {
		t.Error("cctp value rows must be read RAW — the per-(tx, op) max(amount) collapse was twin dedup whose reason (the deleted legacy event_index-0 twins) is gone, and it would halve a genuine same-op double-transfer")
	}
}

// TestCCTPValueCTEsReadRawRows pins file-doc rule 1 at the CTE level: the
// value-carrying subqueries (mints, burns) carry NO grouping at all, while
// the receive-side CTE keeps its per-(tx, op) group — that one is join
// semantics (one BurnMessage body per op so mint rows never fan out), not
// twin dedup, and must survive.
func TestCCTPValueCTEsReadRawRows(t *testing.T) {
	for name, cte := range map[string]string{
		"mints windowed": cctpMintsCTE(true),
		"mints all-time": cctpMintsCTE(false),
		"burns windowed": cctpBurnsCTE(true),
		"burns all-time": cctpBurnsCTE(false),
	} {
		if strings.Contains(cte, "GROUP BY") {
			t.Errorf("%s CTE must read raw per-event rows (no GROUP BY): %s", name, cte)
		}
		if strings.Contains(cte, "max(") || strings.Contains(cte, "min(") {
			t.Errorf("%s CTE must not aggregate value columns: %s", name, cte)
		}
	}
	recv := cctpRecvCTE()
	if !strings.Contains(recv, "GROUP BY tx_hash, op_index") {
		t.Error("recv CTE must keep one body row per (tx_hash, op_index) — the inbound join fans out mint rows otherwise")
	}
	if !strings.Contains(recv, "min(attributes->>'message_body')") {
		t.Error("recv CTE must collapse to a single deterministic body per op")
	}
}

// TestCCTPFlowSeriesQueryShape guards the load-bearing properties of the
// CCTP directional flow series SQL: hourly buckets at the 24h window, daily
// otherwise, honest direction sourcing (inbound = mint_and_withdraw ONLY —
// mint_and_forward restates the same funds at 10× local scale), dedup, and
// EXACT NUMERIC division (ADR-0003).
func TestCCTPFlowSeriesQueryShape(t *testing.T) {
	hourly := cctpFlowSeriesQuery(1, true)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") {
		t.Error("24h query must bucket by hour (a daily bucket gives one point)")
	}
	if !strings.Contains(hourly, `HH24:00`) {
		t.Error("24h query must format point timestamps with the hour")
	}

	daily := cctpFlowSeriesQuery(90, true)
	if !strings.Contains(daily, "date_trunc('day', ts)") {
		t.Error("90d query must bucket by day")
	}
	if strings.Contains(daily, "HH24") {
		t.Error("90d query must not carry an hour component in the timestamp format")
	}

	for _, q := range []string{hourly, daily} {
		assertCCTPNumericSafe(t, q)
		assertCCTPRawValueReads(t, q)
		if !strings.Contains(q, "'mint_and_withdraw'") {
			t.Error("inbound query must source mint_and_withdraw")
		}
		if strings.Contains(q, "mint_and_forward") {
			t.Error("inbound sums must EXCLUDE mint_and_forward — it restates the same transfer at the 7-decimal local scale (counting both inflates a transfer 11×)")
		}
		if !strings.Contains(q, "ts > now() - $1::interval") {
			t.Error("query must be window-bounded (never an unbounded walk)")
		}
	}

	out := cctpFlowSeriesQuery(90, false)
	assertCCTPNumericSafe(t, out)
	assertCCTPRawValueReads(t, out)
	if !strings.Contains(out, "'deposit_for_burn'") {
		t.Error("outbound query must source deposit_for_burn")
	}
}

// TestCCTPAggregateQueriesShape covers the phase-2 queries: KPIs, breakdowns,
// per-chain series and the largest-transfers table are window-bounded +
// deduped + numeric-safe; the cumulative net-inflow series is explicitly
// ALL-TIME (no window bound) and must net inbound against outbound.
func TestCCTPAggregateQueriesShape(t *testing.T) {
	windowed := map[string]string{
		"window KPIs":        cctpWindowKPIQuery(),
		"inbound breakdown":  cctpInboundBreakdownQuery(),
		"outbound breakdown": cctpOutboundBreakdownQuery(),
		"per-chain inbound":  cctpPerChainSeriesQuery(90, true),
		"per-chain outbound": cctpPerChainSeriesQuery(90, false),
		"largest transfers":  cctpLargestTransfersQuery(),
	}
	for name, q := range windowed {
		if !strings.Contains(q, "ts > now() - $1::interval") {
			t.Errorf("%s must be window-bounded", name)
		}
		assertCCTPNumericSafe(t, q)
		assertCCTPRawValueReads(t, q)
	}

	for name, q := range map[string]string{
		"per-chain inbound": cctpPerChainSeriesQuery(90, true),
		"inbound breakdown": cctpInboundBreakdownQuery(),
	} {
		if strings.Contains(q, "mint_and_forward") {
			t.Errorf("%s must exclude mint_and_forward from sums", name)
		}
		if !strings.Contains(q, "substr(COALESCE(r.body, ''), 33, 40)") {
			t.Errorf("%s must extract the burn-token tail from the message body", name)
		}
		if !strings.Contains(q, "LEFT JOIN r") {
			t.Errorf("%s must LEFT JOIN receives so unmatched transfers stay in the total (as Unknown source)", name)
		}
	}

	for _, dirQ := range []string{cctpPerChainSeriesQuery(90, true), cctpPerChainSeriesQuery(90, false)} {
		if !strings.Contains(dirQ, "LIMIT 5") {
			t.Error("per-chain series must cap at the top 5 chains by window volume")
		}
	}

	cum := cctpCumulativeNetInflowQuery()
	if strings.Contains(cum, "$1") || strings.Contains(cum, "interval") {
		t.Error("cumulative net-inflow series is an ALL-TIME chart and must not be window-bounded")
	}
	assertCCTPNumericSafe(t, cum)
	assertCCTPRawValueReads(t, cum)
	if !strings.Contains(cum, "-amount") {
		t.Error("cumulative series must subtract outbound from inbound")
	}
	if !strings.Contains(cum, "OVER (ORDER BY day)") {
		t.Error("cumulative series must be a running sum over days")
	}

	lifetime := cctpAllTimeKPIQuery()
	if strings.Contains(lifetime, "$1") {
		t.Error("all-time KPIs must not be window-bounded")
	}
	assertCCTPNumericSafe(t, lifetime)
	if !strings.Contains(lifetime, "DISTINCT depositor") {
		t.Error("all-time KPIs must count distinct depositors")
	}
	if !strings.Contains(lifetime, "forward_recipient") || !strings.Contains(lifetime, "mint_recipient") {
		t.Error("all-time KPIs must resolve recipients per transfer (forward_recipient over mint_recipient)")
	}
}

// TestCCTPChainLabels guards the honesty rule for SDF-facing chain
// attribution: verified tails/domains resolve to their chain names, and
// anything unrecognised gets an explicit Unverified / Domain N / Unknown
// label — NEVER a guessed chain.
func TestCCTPChainLabels(t *testing.T) {
	// Verified burn-token tails (Circle-published USDC addresses).
	for tail, want := range map[string]string{
		"833589fcd6edb6e08f4c7c32d4f71b54bda02913": "Base",
		"a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48": "Ethereum",
		"abc97431b1bbe4c2d2f6e0e47ca60203452f5d61": "Solana",
		"a0ed00aac12edcdda169e591cd41c94180b46f3b": "Aptos",
		"2b814e6c34a5224bc66947c47dab9dfee93b35fb": "Starknet",
	} {
		if got := cctpChainForBurnToken(tail); got != want {
			t.Errorf("cctpChainForBurnToken(%s) = %q, want %q", tail, got, want)
		}
	}
	// Unknown tail → honest Unverified label carrying the hex, no chain name.
	if got := cctpChainForBurnToken("3600000000000000000000000000000000000000"); got != "Unverified (0x3600…0000)" {
		t.Errorf("unknown tail label = %q, want honest Unverified form", got)
	}
	if got := cctpChainForBurnToken(""); got != "Unknown source" {
		t.Errorf("empty tail label = %q, want Unknown source", got)
	}

	// Domain registry (Circle public docs).
	for domain, want := range map[uint32]string{
		0: "Ethereum", 4: "Noble", 5: "Solana", 6: "Base", 9: "Aptos", 13: "Sonic", 27: "Stellar",
	} {
		if got := cctpChainForDomain(domain); got != want {
			t.Errorf("cctpChainForDomain(%d) = %q, want %q", domain, got, want)
		}
	}
	if got := cctpChainForDomain(24); got != "Domain 24" {
		t.Errorf("unassigned domain label = %q, want Domain 24", got)
	}

	// The largest-transfers / breakdown label seam routes by direction.
	if got := cctpChainLabel("Inbound", "833589fcd6edb6e08f4c7c32d4f71b54bda02913"); got != "Base" {
		t.Errorf("cctpChainLabel inbound = %q, want Base", got)
	}
	if got := cctpChainLabel("Outbound", "5"); got != "Solana" {
		t.Errorf("cctpChainLabel outbound = %q, want Solana", got)
	}
	if got := cctpChainLabel("Outbound", ""); got != "Unknown destination" {
		t.Errorf("cctpChainLabel outbound empty = %q, want Unknown destination", got)
	}
}

// TestRozoSettledSeriesQueryShape — same guards for the Rozo settled-volume
// series: hourly at 24h, daily otherwise, payment-only rows, and exact
// NUMERIC division at the 7-decimal SAC-stroop scale (ADR-0003).
func TestRozoSettledSeriesQueryShape(t *testing.T) {
	hourly := rozoSettledSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") || !strings.Contains(hourly, "HH24:00") {
		t.Error("24h query must bucket by hour with the hour in the timestamp format")
	}

	daily := rozoSettledSeriesQuery(30)
	if !strings.Contains(daily, "date_trunc('day', ts)") || strings.Contains(daily, "HH24") {
		t.Error("30d query must bucket by day with a date-only timestamp format")
	}

	for _, q := range []string{hourly, daily} {
		if !strings.Contains(q, "/ 10000000::numeric") {
			t.Error("rozo division must be exact NUMERIC at the 7-decimal SAC scale")
		}
		if strings.Contains(q, "1e7") || strings.Contains(q, "1e+07") {
			t.Error("rozo division must never use a float literal (ADR-0003)")
		}
		if !strings.Contains(q, "event_type = 'payment'") {
			t.Error("rozo series must count payment events only (flush sweeps double-count)")
		}
		if !strings.Contains(q, "ts > now() - $1::interval") {
			t.Error("query must be window-bounded")
		}
	}
}
