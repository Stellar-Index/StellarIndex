-- accounts_stats rollup — network-wide account analytics for the
-- /accounts hub (operator request 2026-08-08: totals, wealth
-- distribution, trustline distribution, most-held assets).
--
-- Computed by the SAME `stellarindex-ops ch-holders-rollup` cycle that
-- builds the holders boards (30-min timer, staging + atomic EXCHANGE) —
-- it already scans exactly these tables, so the analytics ride along
-- for one extra aggregation pass each. Readers are keyed/tiny:
-- sub-second by construction (page-speed goal 2026-08-08).
--
-- accounts_stats is metric-keyed (one Int64 per metric) rather than a
-- wide singleton row so adding a metric is an INSERT, not a schema
-- migration. Balance-typed metrics are stroops (Int64 — the total XLM
-- supply in stroops is ~1.05e18, inside Int64; ADR-0003 applies at the
-- API layer, which serves them as strings).

CREATE TABLE IF NOT EXISTS stellar.accounts_stats
(
    metric      String,
    value       Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY metric;

CREATE TABLE IF NOT EXISTS stellar.accounts_stats_staging
AS stellar.accounts_stats;

-- Wealth histogram: bucket = clamp(floor(log10(balance_xlm)), -1..10);
-- -1 is "< 1 XLM", 10 is ">= 10B XLM".
CREATE TABLE IF NOT EXISTS stellar.accounts_wealth_histogram
(
    bucket      Int8,
    accounts    UInt64,
    xlm_stroops Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY bucket;

CREATE TABLE IF NOT EXISTS stellar.accounts_wealth_histogram_staging
AS stellar.accounts_wealth_histogram;

-- Trustlines-per-account histogram (accounts with >= 1 trustline; the
-- zero bucket is derived API-side from total_accounts).
CREATE TABLE IF NOT EXISTS stellar.accounts_trustline_histogram
(
    bucket      String,
    accounts    UInt64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY bucket;

CREATE TABLE IF NOT EXISTS stellar.accounts_trustline_histogram_staging
AS stellar.accounts_trustline_histogram;
