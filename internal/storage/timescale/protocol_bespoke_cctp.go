package timescale

import (
	"context"
	"fmt"
	"strconv"
)

// ─── CCTP bespoke analytics (the SDF-showcase suite) ─────────────────────
//
// Direction map: deposit_for_burn = OUTBOUND (Stellar USDC burned for a
// remote mint); mint_and_withdraw = INBOUND (remote burn minted on Stellar).
// message_sent/received and the admin/config events carry no value and are
// excluded from flow sums.
//
// Two data-honesty rules every query below encodes (both ground-truthed on
// r1, 2026-07-30, over all 52,205 cctp_events rows):
//
//  1. DEDUPLICATION. Rows decoded before event_index was plumbed (migration
//     0112) coexist with their re-derived copies: 12,139 (tx, op, type)
//     groups hold exactly 2 rows — one legacy row at event_index 0 plus the
//     re-derived row at the true index — with IDENTICAL amounts (0 groups
//     with differing amounts, 0 groups of 3+). Every flow query therefore
//     collapses to one row per (tx_hash, op_index) transfer via GROUP BY
//     with max(amount) (identical across the pair, so max = the value).
//
//  2. mint_and_forward IS NOT A SEPARATE TRANSFER. Every mint_and_forward
//     op also emits mint_and_withdraw for the SAME funds (0 forward-only
//     ops), and the forward amount is exactly 10× the withdraw amount on
//     all 13,651 pairs — the forward event restates the value at the LOCAL
//     7-decimal SAC scale while mint_and_withdraw carries the CANONICAL
//     6-decimal amount the SAC leg was verified against. Inbound sums use
//     mint_and_withdraw ONLY; summing both would count the same transfer
//     11× over.
//
// Source-chain attribution (inbound): the same-op message_received row's
// message_body carries the CCTP BurnMessage; hex chars 33..72 are the low
// 20 bytes of the burn-side USDC token, resolved via cctpBurnTokenChains
// (see cctp_chains.go for the verification trail). Destination attribution
// (outbound): counterparty_domain via Circle's domain registry.
//
// All division is exact NUMERIC — never a float literal (ADR-0003). Every
// query is either window-bounded (ts > now() - $1::interval) or an
// explicitly all-time aggregate over the tiny cctp_events table
// (52,205 rows total on r1, 2026-07-30 — full scans are trivially cheap,
// and the whole block is built under the window-keyed protocol-detail
// cache).

// cctpSeriesNameInbound / …Outbound are the direction-stable total-series
// names the frontend pairs on; cctpPerChainPrefix* prefix the per-chain
// series; cctpSeriesNameCumulative is the all-time headline series.
const (
	cctpSeriesNameInbound    = "Inbound (USDC)"
	cctpSeriesNameOutbound   = "Outbound (USDC)"
	cctpPerChainPrefixIn     = "Inbound · "
	cctpPerChainPrefixOut    = "Outbound · "
	cctpSeriesNameCumulative = "Cumulative net inflow (all-time)"
)

// cctpMintsCTE returns the deduplicated-inbound-transfers subquery: one row
// per (tx_hash, op_index) mint_and_withdraw transfer (see the dedup rule in
// the file doc). windowed appends the $1::interval bound.
func cctpMintsCTE(windowed bool) string {
	q := `SELECT tx_hash, op_index, min(ts) AS ts, max(amount) AS amount
	   FROM cctp_events
	  WHERE event_type = 'mint_and_withdraw' AND amount IS NOT NULL`
	if windowed {
		q += ` AND ts > now() - $1::interval`
	}
	return q + ` GROUP BY tx_hash, op_index`
}

// cctpBurnsCTE returns the deduplicated-outbound-transfers subquery: one row
// per (tx_hash, op_index) deposit_for_burn, carrying the destination domain
// and depositor (identical across a legacy/re-derived pair; min is the value).
func cctpBurnsCTE(windowed bool) string {
	q := `SELECT tx_hash, op_index, min(ts) AS ts, max(amount) AS amount,
	         min(counterparty_domain) AS domain,
	         min(attributes->>'depositor') AS depositor
	   FROM cctp_events
	  WHERE event_type = 'deposit_for_burn' AND amount IS NOT NULL`
	if windowed {
		q += ` AND ts > now() - $1::interval`
	}
	return q + ` GROUP BY tx_hash, op_index`
}

// cctpRecvCTE returns the deduplicated message_received subquery: one
// BurnMessage body per (tx_hash, op_index) — the inbound join side for
// source-chain attribution.
func cctpRecvCTE(windowed bool) string {
	q := `SELECT tx_hash, op_index, min(attributes->>'message_body') AS body
	   FROM cctp_events
	  WHERE event_type = 'message_received'`
	if windowed {
		q += ` AND ts > now() - $1::interval`
	}
	return q + ` GROUP BY tx_hash, op_index`
}

// cctpFlowSeriesQuery builds the directional total flow series at the
// window's grain over the deduplicated transfers.
func cctpFlowSeriesQuery(windowDays int, inbound bool) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	cte := cctpBurnsCTE(true)
	if inbound {
		cte = cctpMintsCTE(true)
	}
	return `
		WITH t AS (` + cte + `)
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), (COALESCE(sum(amount),0) / 1000000::numeric)::numeric(24,6)::text
		FROM t GROUP BY 1 ORDER BY 1 ASC`
}

// cctpWindowKPIQuery is the windowed inbound/outbound volume + transfer
// counts over the deduplicated transfers.
func cctpWindowKPIQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(true) + `), b AS (` + cctpBurnsCTE(true) + `)
		SELECT
		  COALESCE((SELECT sum(amount) FROM m) / 1000000::numeric, 0)::numeric(24,6)::text,
		  (SELECT count(*) FROM m)::text,
		  COALESCE((SELECT sum(amount) FROM b) / 1000000::numeric, 0)::numeric(24,6)::text,
		  (SELECT count(*) FROM b)::text`
}

// cctpAllTimeKPIQuery is the all-time inbound / outbound / net volumes plus
// the unique-participant counts. Recipients resolve per transfer to the
// forward_recipient when the op forwarded (the final beneficiary) and the
// mint_recipient otherwise; DISTINCT collapses the legacy duplicate rows.
func cctpAllTimeKPIQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(false) + `), b AS (` + cctpBurnsCTE(false) + `)
		SELECT
		  COALESCE((SELECT sum(amount) FROM m) / 1000000::numeric, 0)::numeric(24,6)::text,
		  COALESCE((SELECT sum(amount) FROM b) / 1000000::numeric, 0)::numeric(24,6)::text,
		  ((COALESCE((SELECT sum(amount) FROM m), 0) - COALESCE((SELECT sum(amount) FROM b), 0)) / 1000000::numeric)::numeric(24,6)::text,
		  (SELECT count(DISTINCT depositor) FROM b WHERE depositor IS NOT NULL AND depositor <> '')::text,
		  (SELECT count(*) FROM (
		     SELECT COALESCE(max(attributes->>'forward_recipient'), max(attributes->>'mint_recipient')) AS r
		       FROM cctp_events
		      WHERE event_type IN ('mint_and_withdraw','mint_and_forward')
		      GROUP BY tx_hash, op_index) q WHERE r IS NOT NULL AND r <> '')::text`
}

// cctpInboundBreakdownQuery groups the window's deduplicated inbound
// transfers by burn-token tail (source chain), USDC-weighted descending.
// LEFT JOIN keeps a transfer whose receive row is missing or malformed in
// the total (as an empty tail → "Unknown source") so the breakdown always
// sums to the inbound KPI.
func cctpInboundBreakdownQuery() string {
	return `
		WITH r AS (` + cctpRecvCTE(true) + `), m AS (` + cctpMintsCTE(true) + `)
		SELECT substr(COALESCE(r.body, ''), 33, 40),
		       (sum(m.amount) / 1000000::numeric)::numeric(24,6)::text,
		       count(*)
		FROM m LEFT JOIN r USING (tx_hash, op_index)
		GROUP BY 1 ORDER BY sum(m.amount) DESC`
}

// cctpOutboundBreakdownQuery groups the window's deduplicated outbound
// transfers by destination domain, USDC-weighted descending.
func cctpOutboundBreakdownQuery() string {
	return `
		WITH b AS (` + cctpBurnsCTE(true) + `)
		SELECT COALESCE(domain::text, ''),
		       (sum(amount) / 1000000::numeric)::numeric(24,6)::text,
		       count(*)
		FROM b GROUP BY 1 ORDER BY sum(amount) DESC`
}

// cctpPerChainSeriesQuery builds the top-5-chains-by-window-volume
// directional series: (chain_key, bucket, usdc) rows ordered by each
// chain's total volume descending, then bucket.
func cctpPerChainSeriesQuery(windowDays int, inbound bool) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	var j string
	if inbound {
		j = `WITH r AS (` + cctpRecvCTE(true) + `), m AS (` + cctpMintsCTE(true) + `),
		 j AS (SELECT m.ts, m.amount, substr(COALESCE(r.body, ''), 33, 40) AS chain_key
		         FROM m LEFT JOIN r USING (tx_hash, op_index))`
	} else {
		j = `WITH b AS (` + cctpBurnsCTE(true) + `),
		 j AS (SELECT ts, amount, COALESCE(domain::text, '') AS chain_key FROM b)`
	}
	return j + `,
		 top AS (SELECT chain_key, sum(amount) AS vol FROM j GROUP BY 1 ORDER BY 2 DESC LIMIT 5)
		SELECT j.chain_key,
		       to_char(date_trunc('` + trunc + `', j.ts), '` + format + `'),
		       (sum(j.amount) / 1000000::numeric)::numeric(24,6)::text
		FROM j JOIN top USING (chain_key)
		GROUP BY j.chain_key, 2, top.vol
		ORDER BY top.vol DESC, 2 ASC`
}

// cctpCumulativeNetInflowQuery is the all-time daily running sum of
// (inbound − outbound) — the SDF headline series. Deliberately NOT
// window-bounded: it is an all-time chart (over a tiny table).
func cctpCumulativeNetInflowQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(false) + `), b AS (` + cctpBurnsCTE(false) + `),
		 d AS (
		   SELECT date_trunc('day', ts) AS day, sum(amt) AS net FROM (
		     SELECT ts, amount AS amt FROM m
		     UNION ALL
		     SELECT ts, -amount AS amt FROM b) x
		   GROUP BY 1)
		SELECT to_char(day, 'YYYY-MM-DD'),
		       (sum(net) OVER (ORDER BY day) / 1000000::numeric)::numeric(24,6)::text
		FROM d ORDER BY day ASC`
}

// cctpLargestTransfersQuery lists the window's top-10 transfers by USDC
// amount across both directions, with the chain key for Go-side labelling.
func cctpLargestTransfersQuery() string {
	return `
		WITH r AS (` + cctpRecvCTE(true) + `), m AS (` + cctpMintsCTE(true) + `), b AS (` + cctpBurnsCTE(true) + `)
		SELECT direction, chain_key, (amount / 1000000::numeric)::numeric(24,6)::text, tx_hash, day FROM (
		  SELECT 'Inbound' AS direction,
		         substr(COALESCE(r.body, ''), 33, 40) AS chain_key,
		         m.amount, m.tx_hash, to_char(m.ts, 'YYYY-MM-DD') AS day
		    FROM m LEFT JOIN r USING (tx_hash, op_index)
		  UNION ALL
		  SELECT 'Outbound', COALESCE(b.domain::text, ''), b.amount, b.tx_hash, to_char(b.ts, 'YYYY-MM-DD')
		    FROM b) t
		ORDER BY amount DESC NULLS LAST LIMIT 10`
}

// cctpChainLabel resolves a per-chain key from the queries above — a
// burn-token tail for inbound rows, a decimal domain id for outbound — to
// its verified display name.
func cctpChainLabel(direction string, key string) string {
	if direction == "Inbound" {
		return cctpChainForBurnToken(key)
	}
	if key == "" {
		return "Unknown destination"
	}
	d, err := strconv.ParseUint(key, 10, 32)
	if err != nil {
		return fmt.Sprintf("Domain %s", key)
	}
	return cctpChainForDomain(uint32(d))
}

// bespokeBridgeCCTP assembles the CCTP bridge block: windowed + all-time
// KPIs, the directional total series, per-chain top-5 series, the all-time
// cumulative net-inflow series, source/destination breakdowns, and the
// largest-transfers table.
func (s *Store) bespokeBridgeCCTP(ctx context.Context, since string, windowDays int) (*BespokeBlock, error) {
	blk := &BespokeBlock{
		Category: "bridge",
		Notes: []string{
			"Flows are USDC (canonical 6-decimal event amounts, verified against the SAC leg on-chain). Inbound = mint_and_withdraw transfers, deduplicated per (tx, op); mint_and_forward restates the same funds at the 7-decimal local scale and is excluded from sums. Outbound = deduplicated deposit_for_burn.",
			"Source chains are attributed from the burn-side USDC token inside each transfer's CCTP message body; destinations from Circle's public domain registry. Both maps were verified against Circle's published USDC contract addresses and domain list (2026-07-30). Anything unrecognised is labelled Unverified / Domain N — never guessed.",
		},
	}
	if err := s.cctpKPIs(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.cctpTotalFlowSeries(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.cctpPerChainSeries(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.cctpCumulativeSeries(ctx, blk); err != nil {
		return nil, err
	}
	if err := s.cctpBreakdowns(ctx, blk, since); err != nil {
		return nil, err
	}
	if err := s.cctpLargestTransfers(ctx, blk, since); err != nil {
		return nil, err
	}
	return blk, nil
}

// cctpKPIs fills the windowed volumes and the all-time volume/participant
// headline cards.
func (s *Store) cctpKPIs(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var inVol, inCount, outVol, outCount string
	err := s.db.QueryRowContext(ctx, cctpWindowKPIQuery(), since).
		Scan(&inVol, &inCount, &outVol, &outCount)
	if err != nil {
		return fmt.Errorf("timescale: bespokeBridge cctp window KPIs: %w", err)
	}
	var allIn, allOut, allNet, depositors, recipients string
	err = s.db.QueryRowContext(ctx, cctpAllTimeKPIQuery()).
		Scan(&allIn, &allOut, &allNet, &depositors, &recipients)
	if err != nil {
		return fmt.Errorf("timescale: bespokeBridge cctp all-time KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Inbound volume (%dd)", windowDays), Value: inVol, Unit: "USDC", Hint: "USDC minted on Stellar from remote burns (deduplicated mint_and_withdraw transfers)"},
		BespokeKPI{Label: fmt.Sprintf("Outbound volume (%dd)", windowDays), Value: outVol, Unit: "USDC", Hint: "Stellar USDC burned for remote mints (deduplicated deposit_for_burn transfers)"},
		BespokeKPI{Label: fmt.Sprintf("Transfers in / out (%dd)", windowDays), Value: inCount + " / " + outCount},
		BespokeKPI{Label: "All-time inbound", Value: allIn, Unit: "USDC", Hint: "Total USDC ever minted onto Stellar through CCTP"},
		BespokeKPI{Label: "All-time outbound", Value: allOut, Unit: "USDC", Hint: "Total USDC ever burned off Stellar through CCTP"},
		BespokeKPI{Label: "All-time net inflow", Value: allNet, Unit: "USDC", Hint: "All-time inbound minus outbound — USDC that arrived via CCTP and stayed on Stellar"},
		BespokeKPI{Label: "Unique depositors", Value: depositors, Hint: "Distinct depositor addresses across all outbound burns (all-time)"},
		BespokeKPI{Label: "Unique recipients", Value: recipients, Hint: "Distinct recipient addresses across all inbound mints (the forwarded beneficiary when the transfer forwarded; all-time)"},
	)
	return nil
}

// cctpTotalFlowSeries appends the direction-stable total inbound/outbound
// series ("Inbound (USDC)" / "Outbound (USDC)" across every window; the
// grain — hourly at 24h, daily otherwise — lives in the point timestamps).
func (s *Store) cctpTotalFlowSeries(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	for _, dir := range []struct {
		name    string
		inbound bool
	}{
		{cctpSeriesNameInbound, true},
		{cctpSeriesNameOutbound, false},
	} {
		series, err := s.scanDailySeries(ctx, cctpFlowSeriesQuery(windowDays, dir.inbound), since)
		if err != nil {
			return err
		}
		if len(series) > 0 {
			blk.Series = append(blk.Series, BespokeSeries{Name: dir.name, Unit: "USDC", Points: series})
		}
	}
	return nil
}

// cctpPerChainSeries appends one series per top-5 chain and direction,
// named "Inbound · <chain>" / "Outbound · <chain>", ordered by window
// volume descending within each direction.
func (s *Store) cctpPerChainSeries(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	for _, dir := range []struct {
		prefix    string
		direction string
		inbound   bool
	}{
		{cctpPerChainPrefixIn, "Inbound", true},
		{cctpPerChainPrefixOut, "Outbound", false},
	} {
		series, err := s.collectPerChainSeries(ctx, cctpPerChainSeriesQuery(windowDays, dir.inbound), since, dir.direction, dir.prefix)
		if err != nil {
			return err
		}
		blk.Series = append(blk.Series, series...)
	}
	return nil
}

// collectPerChainSeries runs a per-chain series query and folds its ordered
// (chain_key, bucket, value) rows into per-chain BespokeSeries, preserving
// the query's volume-descending chain order.
func (s *Store) collectPerChainSeries(ctx context.Context, query, since, direction, prefix string) ([]BespokeSeries, error) {
	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeBridge cctp per-chain series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out     []BespokeSeries
		lastKey string
	)
	for rows.Next() {
		var key string
		var pt BespokeSeriesPt
		if err := rows.Scan(&key, &pt.Date, &pt.Value); err != nil {
			return nil, fmt.Errorf("timescale: bespokeBridge cctp per-chain scan: %w", err)
		}
		if len(out) == 0 || key != lastKey {
			out = append(out, BespokeSeries{Name: prefix + cctpChainLabel(direction, key), Unit: "USDC"})
			lastKey = key
		}
		out[len(out)-1].Points = append(out[len(out)-1].Points, pt)
	}
	return out, rows.Err()
}

// cctpCumulativeSeries appends the all-time cumulative net-inflow series.
func (s *Store) cctpCumulativeSeries(ctx context.Context, blk *BespokeBlock) error {
	series, err := s.scanDailySeries(ctx, cctpCumulativeNetInflowQuery())
	if err != nil {
		return err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: cctpSeriesNameCumulative, Unit: "USDC", Points: series})
	}
	return nil
}

// cctpBreakdowns fills the two composition datasets behind the donuts.
func (s *Store) cctpBreakdowns(ctx context.Context, blk *BespokeBlock, since string) error {
	for _, b := range []struct {
		title     string
		direction string
		query     string
	}{
		{"Inflows by source chain", "Inbound", cctpInboundBreakdownQuery()},
		{"Outflows by destination chain", "Outbound", cctpOutboundBreakdownQuery()},
	} {
		bd, err := s.collectBreakdown(ctx, b.title, b.direction, b.query, since)
		if err != nil {
			return err
		}
		if len(bd.Rows) > 0 {
			blk.Breakdowns = append(blk.Breakdowns, bd)
		}
	}
	return nil
}

// collectBreakdown runs one composition query and labels its (chain_key,
// value, count) rows via the verified chain maps.
func (s *Store) collectBreakdown(ctx context.Context, title, direction, query, since string) (BespokeBreakdown, error) {
	bd := BespokeBreakdown{Title: title, Unit: "USDC"}
	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return bd, fmt.Errorf("timescale: bespokeBridge cctp breakdown %q: %w", title, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var row BespokeBreakdownRow
		if err := rows.Scan(&key, &row.Value, &row.Count); err != nil {
			return bd, fmt.Errorf("timescale: bespokeBridge cctp breakdown scan: %w", err)
		}
		row.Label = cctpChainLabel(direction, key)
		bd.Rows = append(bd.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return bd, fmt.Errorf("timescale: bespokeBridge cctp breakdown rows: %w", err)
	}
	return bd, nil
}

// cctpLargestTransfers fills the window's top-10 transfers table. Amounts
// are ordered on the raw 6-decimal integer and served as the SQL-side exact
// NUMERIC decimal string (ADR-0003).
func (s *Store) cctpLargestTransfers(ctx context.Context, blk *BespokeBlock, since string) error {
	rows, err := s.db.QueryContext(ctx, cctpLargestTransfersQuery(), since)
	if err != nil {
		return fmt.Errorf("timescale: bespokeBridge cctp largest transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tbl := BespokeTable{
		Title:   "Largest transfers",
		Columns: []string{"Direction", "Chain", "Amount (USDC)", "Tx", "Date"},
	}
	for rows.Next() {
		var direction, key, amount, txHash, day string
		if err := rows.Scan(&direction, &key, &amount, &txHash, &day); err != nil {
			return fmt.Errorf("timescale: bespokeBridge cctp largest transfers scan: %w", err)
		}
		tbl.Rows = append(tbl.Rows, []string{direction, cctpChainLabel(direction, key), amount, txHash, day})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("timescale: bespokeBridge cctp largest transfers rows: %w", err)
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return nil
}
