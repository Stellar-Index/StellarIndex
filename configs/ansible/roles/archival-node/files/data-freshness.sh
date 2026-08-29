#!/usr/bin/env bash
# data-freshness watchdog — the "never get behind" signal.
#
# Emits node_exporter textfile gauges for (a) per-domain ingest freshness across
# EVERY data domain and (b) the per-source ADR-0033 completeness verdict, so a
# feed dying (coingecko hit its quota → 11 days stale, unnoticed), a timer
# silently not firing (sep1-refresh never existed; the completeness verdict went
# 21 days stale), or a real served≠lake gap (a source going complete=false now
# that the watchdog is trustworthy) all PAGE instead of rotting silently.
#
# The gap detector (source_coverage_snapshots) already covers on-chain
# trade/event source gaps; this fills the rest: reference oracles, FX, supply,
# the issuer-metadata cron, and the verdict itself.
#
# Run from a 15-min timer. One cheap grouped query per domain; same DSN sourcing
# as compute-archive-to.sh (peer-auth fails under systemd's user-switch).
set -euo pipefail
# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
load_env_file /etc/default/stellarindex

# Debian's pg_wrapper `psql` stats the cluster data dir to pick a version and
# aborts with "Invalid data directory for cluster 15 main" for any user that
# cannot read it — which User=stellarindex (2026-07-03 non-root hardening)
# cannot. Call the versioned binary directly to bypass the wrapper.
PSQL="/usr/lib/postgresql/${PG_VERSION:-15}/bin/psql"

OUT="${TEXTFILE_OUTPUT:-/var/lib/node_exporter/textfile_collector/data_freshness.prom}"
TMP="$(mktemp "${OUT}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

{
  echo '# HELP stellarindex_data_freshness_age_seconds Seconds since the newest row for a data domain/source.'
  echo '# TYPE stellarindex_data_freshness_age_seconds gauge'
  echo '# HELP stellarindex_data_freshness_stale 1 when a domain/source is staler than its expected cadence.'
  echo '# TYPE stellarindex_data_freshness_stale gauge'
  echo '# HELP stellarindex_completeness_incomplete 1 when a source latest ADR-0033 verdict is complete=false (real served<>lake gap). Excludes the system recognition row — see stellarindex_recognition_unattributed_shapes.'
  echo '# TYPE stellarindex_completeness_incomplete gauge'
  echo '# HELP stellarindex_recognition_unattributed_shapes Distinct (contract, topic) event shapes in the lake no enabled source claims. Expected large and slowly growing (foreign protocols); the alertable signal is a JUMP (ownership-registry regression).'
  echo '# TYPE stellarindex_recognition_unattributed_shapes gauge'
  echo '# HELP stellarindex_twap_history_missing 1 when a TWAP continuous aggregate is missing the history prices_1m holds — a migration recreated/emptied it (WITH NO DATA) and the manual refresh_continuous_aggregate follow-up never ran.'
  echo '# TYPE stellarindex_twap_history_missing gauge'
} > "$TMP"

# (domain, source, age_seconds, threshold_seconds) per domain. Thresholds are a
# generous multiple of each domain's natural cadence so only a real stall fires.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH f AS (
  -- Crypto oracles (reflector/redstone/band/chainlink/coingecko) update every
  -- few minutes → 3h threshold. ECB is the exception: a DAILY FX reference
  -- (publishes ~16:00 CET on TARGET business days, none on weekends/holidays),
  -- so it needs a 4-day threshold to tolerate a weekend + a holiday without
  -- false-firing — otherwise it reads stale ~21h of every day.
  SELECT 'oracle'  AS domain, source AS src, extract(epoch FROM now()-max(ingested_at)) AS age,
         CASE WHEN source = 'ecb' THEN 345600 ELSE 10800 END AS thr
    FROM oracle_updates WHERE ingested_at > now()-interval '30 days' GROUP BY source
  UNION ALL
  -- FX is daily-grain: observed_at is the data-point time (lags ~a day even
  -- when healthy), so freshness is measured off `bucket` (today's bucket
  -- written = the worker is alive). 48h tolerates a late daily publish.
  SELECT 'fx', source, extract(epoch FROM now()-max(bucket)), 172800
    FROM fx_quotes WHERE bucket > now()-interval '30 days' GROUP BY source
  UNION ALL
  -- Sparse Soroban AMMs get 24h: phoenix's MEASURED 30-day gap
  -- distribution (2026-08-05, 3,278 trades) is max 8h28m / p99 3h12m,
  -- and a 12h+ genuine market lull false-fired the flat 4h threshold
  -- twice — with the lake confirming zero swap events on ANY known or
  -- unknown pool (quiet, not stale; the CS-102 class). 24h still
  -- catches a dead decoder within a day, and the ADR-0033 verdict
  -- (129600s below) remains the real correctness net.
  SELECT 'trades', source, extract(epoch FROM now()-max(bucket)),
         CASE WHEN source IN ('phoenix','comet') THEN 86400 ELSE 14400 END
    FROM source_volume_1h GROUP BY source
  UNION ALL
  SELECT 'supply', 'asset_supply_history', extract(epoch FROM now()-max(time)), 108000
    FROM asset_supply_history WHERE time > now()-interval '7 days'
  UNION ALL
  SELECT 'verdict', source, extract(epoch FROM now()-max(computed_at)), 129600
    FROM completeness_snapshots GROUP BY source
  UNION ALL
  SELECT 'sep1', 'issuers', extract(epoch FROM now()-max(sep1_resolved_at)), 172800
    FROM issuers WHERE sep1_resolved_at IS NOT NULL
)
SELECT 'stellarindex_data_freshness_age_seconds{domain="'||domain||'",source="'||src||'"} '||round(age)::text
  FROM f
UNION ALL
SELECT 'stellarindex_data_freshness_stale{domain="'||domain||'",source="'||src||'"} '||(age>thr)::int::text
  FROM f;
SQL

# CS-102: the `supply` domain above measures max(time) across the WHOLE table,
# so it only proves SOME asset is publishing. On 2026-07-28 that read green
# while 37 of 48 watched assets had frozen — a handful of live assets kept the
# global max current and hid the rest. An aggregate cannot see a partial
# freeze, so emit the per-asset shape too: how many watched assets are stale,
# and the worst age among them. Low cardinality (two series) on purpose —
# per-asset series would grow with the watched set.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH per_asset AS (
  SELECT asset_key, extract(epoch FROM now()-max(time)) AS age
    FROM asset_supply_history
   WHERE time > now()-interval '30 days'
   GROUP BY asset_key
)
SELECT 'stellarindex_supply_assets_stale '||count(*) FILTER (WHERE age > 108000)::text
  FROM per_asset
UNION ALL
SELECT 'stellarindex_supply_asset_max_age_seconds '||COALESCE(round(max(age)),0)::text
  FROM per_asset;
SQL

# Per-source completeness verdict (latest snapshot per source): 1 = incomplete.
#
# The system 'recognition' row is EXCLUDED: it counts event shapes on contracts
# NO source owns — i.e. the rest of the Soroban ecosystem (~23k shapes, growing
# ~30/day) — so complete=false there is the permanent, expected state of a
# curated indexer, not a served<>lake gap. Folding it into this gauge kept the
# ticket alert firing continuously (2026-08-17 onward, when W1-flowcompleteness-1
# restored the row's refresh). Real silent-drop detection lives in each source's
# OWN recognition axis (recognition_ok on owned contracts), which flips that
# source's row incomplete and still alerts here. The system row is exported
# below as a COUNT so a registry regression (a whole protocol's shapes suddenly
# unattributed — the rozo/BACKLOG-89 class) shows as a step change instead.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_completeness_incomplete{source="'||source||'"} '||(NOT complete)::int::text
  FROM (SELECT DISTINCT ON (source) source, complete
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s
 WHERE source <> 'recognition';
SQL

# System recognition census count, parsed from the snapshot's detail text
# ("<N> unrecognized shape(s) on unowned contracts (earliest ledger L) — run
# verify-recognition"; the clean state has no leading digits → 0).
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_recognition_unattributed_shapes '||
       COALESCE(substring(detail FROM '^[0-9]+'), '0')
  FROM (SELECT detail FROM completeness_snapshots
         WHERE source = 'recognition'
         ORDER BY computed_at DESC LIMIT 1) r;
SQL

# W1-migrations-1 / REC-01: the TWAP continuous aggregates (twap_1h/twap_1d)
# are hierarchical roll-ups over prices_1m that a migration recreates WITH NO
# DATA whenever their SELECT changes (0081 → 0115 → 0126). A recreate DELETES
# all materialized history; re-materialization is a MANUAL operator step
# (`CALL refresh_continuous_aggregate('twap_1h', NULL, now())`) that nothing
# enforces. The trap that hides a skipped follow-up: each view's refresh POLICY
# only auto-materializes a recent trailing window (twap_1h start_offset 4h,
# twap_1d 7d), so RECENT bars reappear on the next policy tick and every
# newest-bar freshness/age check reads GREEN while the entire back-history
# stays silently empty — the API serves no TWAP for older ranges. The
# ADR-0033 completeness verdict cannot see this: twap_* are derived price
# CAGGs, not reconcile TARGETS. So detect it directly — a view is emptied-
# pending-refresh when prices_1m holds real history but the view's OLDEST
# materialized bar is far newer than prices_1m's oldest (i.e. it only carries
# the policy's trailing sliver). Cheap: min(bucket) rides each view's
# time index; the 1-day slack absorbs twap_1d's daily bucket truncation.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH pm  AS (SELECT min(bucket) AS pmin FROM prices_1m),
     t1h AS (SELECT min(bucket) AS tmin, count(*) AS n FROM twap_1h),
     t1d AS (SELECT min(bucket) AS tmin, count(*) AS n FROM twap_1d)
-- Only judge once prices_1m has >2 days of history (a fresh/empty deploy or the
-- legitimate initial-materialization window must not false-fire). Missing iff
-- the view is empty OR its oldest bar trails prices_1m's oldest by >1 day.
SELECT 'stellarindex_twap_history_missing{view="twap_1h"} '||
       (CASE WHEN (SELECT pmin FROM pm) IS NOT NULL
                  AND (SELECT pmin FROM pm) < now() - interval '2 days'
                  AND ((SELECT n FROM t1h) = 0
                       OR (SELECT tmin FROM t1h) > (SELECT pmin FROM pm) + interval '1 day')
             THEN 1 ELSE 0 END)::text
UNION ALL
SELECT 'stellarindex_twap_history_missing{view="twap_1d"} '||
       (CASE WHEN (SELECT pmin FROM pm) IS NOT NULL
                  AND (SELECT pmin FROM pm) < now() - interval '2 days'
                  AND ((SELECT n FROM t1d) = 0
                       OR (SELECT tmin FROM t1d) > (SELECT pmin FROM pm) + interval '1 day')
             THEN 1 ELSE 0 END)::text;
SQL

# CS-090: a verdict can read complete=true while its watermark lags the live
# network head (a mid-walk stall or a manual small -to). complete/computed_at
# alone can't see that, so emit the per-source lag (live ingest cursor tip −
# verdict watermark) — a source verified only to an old ledger becomes
# observable/alertable instead of showing a green "N/N complete" badge.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH tip AS (SELECT max(last_ledger) AS t FROM ingestion_cursors)
SELECT 'stellarindex_completeness_watermark_lag_ledgers{source="'||s.source||'"} '
       ||greatest(0, (SELECT t FROM tip) - s.watermark_ledger)::text
  FROM (SELECT DISTINCT ON (source) source, watermark_ledger
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s;
SQL

# supply_flows (ClickHouse) is the per-token mint/burn/clawback set that backs
# /v1/assets SEP-41 supply — TokenSupply() sums it FINAL on-demand, so its
# freshness IS the served SEP-41 supply's freshness. Live-written by the
# indexer; if it stalls, served SEP-41 supply goes stale. (Threshold generous —
# supply events are bursty.)
SF_AGE=$(curl -sS --max-time 15 http://localhost:8123/ --data-binary \
  "SELECT toUInt64(dateDiff('second', max(ingested_at), now())) FROM stellar.supply_flows" 2>/dev/null | tr -d '[:space:]')
if [ -n "$SF_AGE" ]; then
  printf 'stellarindex_data_freshness_age_seconds{domain="sep41_supply",source="supply_flows"} %s\n' "$SF_AGE" >> "$TMP"
  printf 'stellarindex_data_freshness_stale{domain="sep41_supply",source="supply_flows"} %s\n' \
    "$([ "$SF_AGE" -gt 3600 ] && echo 1 || echo 0)" >> "$TMP"
fi

# node_exporter runs unprivileged — mktemp defaults to 0600, so make the
# rendered file world-readable before the atomic swap or the collector skips it.
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
trap - EXIT
