package timescale

import (
	"context"
	"fmt"
	"strconv"
)

// CCTP bespoke analytics.
//
// deposit_for_burn is OUTBOUND and mint_and_withdraw is INBOUND; message and admin/config events
// carry no value and are excluded from flow sums. Division is exact NUMERIC, never a float
// (ADR-0003).
//
//  1. Value rows are read raw, one transfer per stored event row, with no per-(tx, op) collapse:
//     the cctp_events PK discriminates on event_index, and max(amount) would halve a genuine
//     batched double-transfer in one op.
//  2. mint_and_forward is not a separate transfer. It always accompanies mint_and_withdraw for the
//     same funds at the local 7-decimal SAC scale (exactly 10x) while mint_and_withdraw carries
//     the canonical 6-decimal amount, so inbound sums use mint_and_withdraw only (summing both
//     counts it 11x). deposit_for_burn is canonical too
//     (TestDepositForBurnAmount_IsCanonicalSixDecimals).
//
// Inbound source-chain attribution: the same-op message_received row's message_body carries the
// BurnMessage, whose burn-token tail is resolved via cctpBurnTokenChains (cctp_chains.go). That
// receive side is the one place a per-(tx, op) group survives, since the join needs exactly one
// body row per op or every mint row would fan out (see cctpRecvCTE). Outbound uses
// counterparty_domain.

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

// cctpMintsCTE returns the inbound subquery: one RAW row per mint_and_withdraw event, no per-(tx, op) collapse (file-doc rule 1);
// windowed appends the $1::interval bound.
func cctpMintsCTE(windowed bool) string {
	q := `SELECT tx_hash, op_index, ts, amount
	   FROM cctp_events
	  WHERE event_type = 'mint_and_withdraw' AND amount IS NOT NULL`
	if windowed {
		q += ` AND ts > now() - $1::interval`
	}
	return q
}

// cctpBurnsCTE returns the outbound subquery: one RAW row per deposit_for_burn event with destination domain and depositor (file-doc
// rule 1).
func cctpBurnsCTE(windowed bool) string {
	q := `SELECT tx_hash, op_index, ts, amount,
	         counterparty_domain AS domain,
	         attributes->>'depositor' AS depositor
	   FROM cctp_events
	  WHERE event_type = 'deposit_for_burn' AND amount IS NOT NULL`
	if windowed {
		q += ` AND ts > now() - $1::interval`
	}
	return q
}

// cctpRecvCTE returns one BurnMessage body per (tx_hash, op_index) for source-chain attribution. The per-(tx, op) GROUP BY survives here
// (file-doc rule 1) so mints do not fan out; with two messages in one op, min(body) keeps totals and only skews that op's chain split.
// Always window-bounded.
func cctpRecvCTE() string {
	return `SELECT tx_hash, op_index, min(attributes->>'message_body') AS body
	   FROM cctp_events
	  WHERE event_type = 'message_received' AND ts > now() - $1::interval
	  GROUP BY tx_hash, op_index`
}

// cctpFlowSeriesQuery builds the directional total flow series at the
// window's grain over the raw per-event transfers.
func cctpFlowSeriesQuery(windowDays int, inbound bool) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	cte := cctpBurnsCTE(true)
	if inbound {
		cte = cctpMintsCTE(true)
	}
	return `
		WITH t AS (` + cte + `)
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), (COALESCE(sum(amount),0) / 1000000::numeric)::numeric(24,6)::text
		FROM t WHERE true` + completeDaysOnly(windowDays, "ts") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// cctpWindowKPIQuery is the windowed inbound/outbound volume + transfer
// counts over the raw per-event transfers.
func cctpWindowKPIQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(true) + `), b AS (` + cctpBurnsCTE(true) + `)
		SELECT
		  COALESCE((SELECT sum(amount) FROM m) / 1000000::numeric, 0)::numeric(24,6)::text,
		  (SELECT count(*) FROM m)::text,
		  COALESCE((SELECT sum(amount) FROM b) / 1000000::numeric, 0)::numeric(24,6)::text,
		  (SELECT count(*) FROM b)::text`
}

// cctpAllTimeKPIQuery is all-time inbound/outbound/net volumes and unique participants. Recipient resolves per op to forward_recipient
// else mint_recipient (the forward-restatement pairing of file-doc rule 2); count(DISTINCT r), not count(*), counts addresses.
func cctpAllTimeKPIQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(false) + `), b AS (` + cctpBurnsCTE(false) + `)
		SELECT
		  COALESCE((SELECT sum(amount) FROM m) / 1000000::numeric, 0)::numeric(24,6)::text,
		  COALESCE((SELECT sum(amount) FROM b) / 1000000::numeric, 0)::numeric(24,6)::text,
		  ((COALESCE((SELECT sum(amount) FROM m), 0) - COALESCE((SELECT sum(amount) FROM b), 0)) / 1000000::numeric)::numeric(24,6)::text,
		  (SELECT count(DISTINCT depositor) FROM b WHERE depositor IS NOT NULL AND depositor <> '')::text,
		  (SELECT count(DISTINCT r) FROM (
		     SELECT COALESCE(max(attributes->>'forward_recipient'), max(attributes->>'mint_recipient')) AS r
		       FROM cctp_events
		      WHERE event_type IN ('mint_and_withdraw','mint_and_forward')
		      GROUP BY tx_hash, op_index) q WHERE r IS NOT NULL AND r <> '')::text`
}

// cctpInboundBreakdownQuery groups inbound by burn-token tail, USDC-weighted; the LEFT JOIN keeps unmatched receives as "Unknown source"
// so it sums to the KPI.
func cctpInboundBreakdownQuery() string {
	return `
		WITH r AS (` + cctpRecvCTE() + `), m AS (` + cctpMintsCTE(true) + `)
		SELECT substr(COALESCE(r.body, ''), 33, 40),
		       (sum(m.amount) / 1000000::numeric)::numeric(24,6)::text,
		       count(*)
		FROM m LEFT JOIN r USING (tx_hash, op_index)
		GROUP BY 1 ORDER BY sum(m.amount) DESC`
}

// cctpOutboundBreakdownQuery groups the window's outbound transfers by
// destination domain, USDC-weighted descending.
func cctpOutboundBreakdownQuery() string {
	return `
		WITH b AS (` + cctpBurnsCTE(true) + `)
		SELECT COALESCE(domain::text, ''),
		       (sum(amount) / 1000000::numeric)::numeric(24,6)::text,
		       count(*)
		FROM b GROUP BY 1 ORDER BY sum(amount) DESC`
}

// cctpPerChainSeriesQuery builds top-5-chain directional series ordered by chain volume desc, chain_key (tie-break), bucket, so each
// chain's rows stay contiguous for collectPerChainSeries.
func cctpPerChainSeriesQuery(windowDays int, inbound bool) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	var j string
	if inbound {
		j = `WITH r AS (` + cctpRecvCTE() + `), m AS (` + cctpMintsCTE(true) + `),
		 j AS (SELECT m.ts, m.amount, substr(COALESCE(r.body, ''), 33, 40) AS chain_key
		         FROM m LEFT JOIN r USING (tx_hash, op_index))`
	} else {
		j = `WITH b AS (` + cctpBurnsCTE(true) + `),
		 j AS (SELECT ts, amount, COALESCE(domain::text, '') AS chain_key FROM b)`
	}
	return j + `,
		 top AS (SELECT chain_key, sum(amount) AS vol FROM j GROUP BY 1 ORDER BY 2 DESC, chain_key ASC LIMIT 5)
		SELECT j.chain_key,
		       to_char(date_trunc('` + trunc + `', j.ts), '` + format + `'),
		       (sum(j.amount) / 1000000::numeric)::numeric(24,6)::text
		FROM j JOIN top USING (chain_key)
		WHERE true` + completeDaysOnly(windowDays, "j.ts") + `
		GROUP BY j.chain_key, 2, top.vol
		ORDER BY top.vol DESC, j.chain_key ASC, 2 ASC`
}

// cctpCumulativeNetInflowQuery is the all-time daily running sum of (inbound - outbound), deliberately not window-bounded (tiny table).
func cctpCumulativeNetInflowQuery() string {
	return `
		WITH m AS (` + cctpMintsCTE(false) + `), b AS (` + cctpBurnsCTE(false) + `),
		 d AS (
		   SELECT date_trunc('day', ts) AS day, sum(amt) AS net FROM (
		     SELECT ts, amount AS amt FROM m
		     UNION ALL
		     SELECT ts, -amount AS amt FROM b) x
		   -- Complete days only: today's partial increment would render
		   -- as a phantom flattening/cliff on the running sum (UXP-16).
		   WHERE x.ts < ` + completeDayCutoffSQL + `
		   GROUP BY 1)
		SELECT to_char(day, 'YYYY-MM-DD'),
		       (sum(net) OVER (ORDER BY day) / 1000000::numeric)::numeric(24,6)::text
		FROM d ORDER BY day ASC`
}

// cctpLargestTransfersQuery lists the window's top-10 transfers by USDC
// amount across both directions, with the chain key for Go-side labelling.
func cctpLargestTransfersQuery() string {
	return `
		WITH r AS (` + cctpRecvCTE() + `), m AS (` + cctpMintsCTE(true) + `), b AS (` + cctpBurnsCTE(true) + `)
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

// cctpChainLabel resolves a per-chain key (burn-token tail inbound, decimal domain id outbound) to its verified display name.
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

// bespokeBridgeCCTP assembles the CCTP block: windowed and all-time KPIs, flow and per-chain series, breakdowns and largest transfers.
func (s *Store) bespokeBridgeCCTP(ctx context.Context, since string, windowDays int) (*BespokeBlock, error) {
	blk := &BespokeBlock{
		Category: "bridge",
		Notes: []string{
			"Flows are USDC (canonical 6-decimal event amounts: inbound verified against the SAC leg on-chain, outbound against the cross-chain BurnMessage). Inbound = mint_and_withdraw transfer events; mint_and_forward restates the same funds at the 7-decimal local scale and is excluded from sums. Outbound = deposit_for_burn events.",
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
		BespokeKPI{Label: fmt.Sprintf("Inbound volume (%dd)", windowDays), Value: inVol, Unit: "USDC", Hint: "USDC minted on Stellar from remote burns (mint_and_withdraw transfer events)"},
		BespokeKPI{Label: fmt.Sprintf("Outbound volume (%dd)", windowDays), Value: outVol, Unit: "USDC", Hint: "Stellar USDC burned for remote mints (deposit_for_burn transfer events)"},
		BespokeKPI{Label: fmt.Sprintf("Transfers in / out (%dd)", windowDays), Value: inCount + " / " + outCount},
		BespokeKPI{Label: "All-time inbound", Value: allIn, Unit: "USDC", Hint: "Total USDC ever minted onto Stellar through CCTP"},
		BespokeKPI{Label: "All-time outbound", Value: allOut, Unit: "USDC", Hint: "Total USDC ever burned off Stellar through CCTP"},
		BespokeKPI{Label: "All-time net inflow", Value: allNet, Unit: "USDC", Hint: "All-time inbound minus outbound — USDC that arrived via CCTP and stayed on Stellar"},
		BespokeKPI{Label: "Unique depositors", Value: depositors, Hint: "Distinct depositor addresses across all outbound burns (all-time)"},
		BespokeKPI{Label: "Unique recipients", Value: recipients, Hint: "Distinct recipient addresses across all inbound mints (the forwarded beneficiary when the transfer forwarded; all-time)"},
	)
	return nil
}

// cctpTotalFlowSeries appends the direction-stable total "Inbound (USDC)"/"Outbound (USDC)" series; grain (hourly at 24h, else daily) is
// in the timestamps.
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

// cctpPerChainSeries appends one "Inbound · <chain>"/"Outbound · <chain>" series per top-5 chain, ordered by window volume descending.
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

// collectPerChainSeries folds ordered (chain_key, bucket, value) rows into per-chain BespokeSeries, keeping the query's chain order.
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

// cctpLargestTransfers fills the window's top-10 table, ordered on the raw 6-decimal integer and served as the exact SQL NUMERIC string
// (ADR-0003).
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
