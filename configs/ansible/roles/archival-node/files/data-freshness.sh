#!/usr/bin/env bash
# data-freshness watchdog: node_exporter textfile gauges for per-domain ingest freshness
# and the per-source ADR-0033 completeness verdict, so a dead feed, a timer that never
# fires, or a real served<>lake gap pages instead of rotting silently.
# Complements the gap detector (source_coverage_snapshots, on-chain trade/event gaps):
# this covers reference oracles, FX, supply, the issuer-metadata cron and the verdict.
# Run from a 15-min timer. DSN sourced as in compute-archive-to.sh (peer-auth fails under systemd's user-switch).
# as compute-archive-to.sh (peer-auth fails under systemd's user-switch).
set -euo pipefail
# Read a systemd EnvironmentFile VERBATIM, never source it: its values are unquoted, so
# the shell would expand `$`, split on `;&|` and eat quotes inside a secret, mangling the DSN.
# Same reader as run-heavy-job.sh. usage: load_env_file FILE [export]
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
# CH as ops_monitor once the role renders this file; /dev/null (CH `default`) until then,
# so no deploy order strands this script.
CH_NETRC="${CH_NETRC:-/etc/clickhouse-client/ops-monitor.netrc}"
if [[ ! -r "$CH_NETRC" ]]; then
  echo "data-freshness.sh: credential file $CH_NETRC unreadable; using ClickHouse default user" >&2
  CH_NETRC=/dev/null
fi

# Debian's pg_wrapper psql aborts ("Invalid data directory") for users who cannot read the
# cluster data dir, which User=stellarindex cannot; call the versioned binary directly.
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
  echo '# HELP stellarindex_recognition_unattributed_shapes UNKNOWN-PROTOCOL events: distinct (contract, topic) shapes in the lake on contracts NO source owns. Foreign protocols we do not index. Expected large and permanently growing as Stellar grows — 23866 as of 2026-09-01 against ~30/day organic drift. A coverage-ambition number for prioritising which decoders to build next, NOT an error condition. Deliberately NOT alerted on: see stellarindex_recognition_ok for the signal that means a real defect.'
  echo '# TYPE stellarindex_recognition_unattributed_shapes gauge'
  echo '# HELP stellarindex_recognition_ok UNRECOGNIZED EVENTS FROM A KNOWN PROTOCOL: 0 when a source WE INDEX emitted a (contract, topic) shape none of its decoders claimed, 1 when every shape on its owned contracts decodes. This is the bucket that means OUR bug — a protocol we promised to cover is silently dropping events — and it is the one worth paging on. Distinct from unattributed_shapes, which counts other people contracts and is inert.'
  echo '# TYPE stellarindex_recognition_ok gauge'
  echo '# HELP stellarindex_cagg_history_missing 1 when a price continuous aggregate (prices_1m..prices_1mo) holds less history than the trades hypertable it aggregates — a migration recreated it WITH NO DATA, or a replay rewrote its base rows, and the manual refresh_continuous_aggregate follow-up never ran. Its refresh policy re-fills only a trailing sliver, so every newest-bar and last-refresh signal reads green while the back-history serves empty.'
  echo '# TYPE stellarindex_cagg_history_missing gauge'
  echo '# HELP stellarindex_twap_history_missing 1 when a TWAP continuous aggregate is missing the history prices_1m holds — a migration recreated/emptied it (WITH NO DATA) and the manual refresh_continuous_aggregate follow-up never ran.'
  echo '# TYPE stellarindex_twap_history_missing gauge'
  # These families are composed inside the SQL blocks below; an undeclared family is scraped
  # untyped, so declare HELP/TYPE here.
  echo '# HELP stellarindex_supply_assets_stale Watched assets whose newest asset_supply_history row is older than 30h. An aggregate max(time) cannot see a PARTIAL freeze: on 2026-07-28 a handful of live assets kept it green while 37 of 48 had stopped (CS-102).'
  echo '# TYPE stellarindex_supply_assets_stale gauge'
  echo '# HELP stellarindex_supply_asset_max_age_seconds Worst per-asset supply age in seconds across the watched set, 0 when none is known.'
  echo '# TYPE stellarindex_supply_asset_max_age_seconds gauge'
  echo '# HELP stellarindex_completeness_watermark_lag_ledgers Ledgers between the live ingest tip and a source latest ADR-0033 verdict watermark. A verdict can read complete=true while it was only ever verified up to an old ledger (CS-090).'
  echo '# TYPE stellarindex_completeness_watermark_lag_ledgers gauge'
  # Publication safety, not a data signal. Emitted on EVERY run so the healthy 0 exists as a
  # series rather than an absence; see the validator above the atomic swap.
  echo '# HELP stellarindex_data_freshness_unparseable_lines Exposition lines this run rendered that are not valid Prometheus samples and were therefore WITHHELD from the published textfile. 0 every healthy run; above 0 means a query answered with something that is not a number (a NULL, an error string, a command tag) and that one series is absent this tick.'
  echo '# TYPE stellarindex_data_freshness_unparseable_lines gauge'
} > "$TMP"

# (domain, source, age_seconds, threshold_seconds) per domain. Thresholds are a generous
# multiple of each domain's natural cadence so only a real stall fires.
#
# No window on the source enumeration: a windowed GROUP BY would make the window that
# defines the universe also define health, so a source dead longer than it (the
# coingecko-quota shape) would leave the GROUP BY, its stale series would go ABSENT
# instead of 1, and the alert would RESOLVE as the outage worsens. So the universe is
# every source the table has EVER held, aged against its own newest row. Scan cost stays
# small: oracle_updates is a hypertable on `ts` (an `ingested_at` window would not prune),
# fx_quotes is daily-grain, asset_supply_history answers max(time) from its index.
#
# Trade-off: a source retired on purpose keeps reporting stale (fail-closed). Retiring one
# means deleting its rows or excluding it here in the same change.
#
# shellcheck disable=SC2129  # the appends below are separate on purpose: each is a distinct query with its own reasoning between them, and a single `{ … } >> $TMP` would bury that
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH f AS (
  -- Crypto oracles (reflector/redstone/band/chainlink/coingecko) update every
  -- few minutes → 3h threshold. ECB is the exception: a DAILY FX reference
  -- (publishes ~16:00 CET on TARGET business days, none on weekends/holidays),
  -- so it needs a 4-day threshold to tolerate a weekend + a holiday without
  -- false-firing — otherwise it reads stale ~21h of every day. Tiingo is the
  -- same shape: end-of-day fund NAV bars, and a re-poll of a stored bar hits
  -- the primary key without moving ingested_at, so the age only resets when a
  -- NEW bar lands (none on weekends/holidays).
  SELECT 'oracle'  AS domain, source AS src, extract(epoch FROM now()-max(ingested_at)) AS age,
         CASE WHEN source IN ('ecb', 'tiingo') THEN 345600 ELSE 10800 END AS thr
    FROM oracle_updates GROUP BY source
  UNION ALL
  -- FX is daily-grain: observed_at is the data-point time (lags ~a day even
  -- when healthy), so freshness is measured off `bucket` (today's bucket
  -- written = the worker is alive).
  --
  -- 76h, NOT 48h, and the number is not arbitrary: it mirrors
  -- `aggregate.composite_reference.fx_max_age_hours` (default 76,
  -- internal/config/config.go) — the budget the /v1/price
  -- fiat-cross-rate serving path already applies to its FX leg
  -- (`pricing_guard.fx_cross_max_age_hours`, T650). That is one of
  -- several fiat-FX consumers, not "the" serving path singular: the
  -- declared-peg market-cap fallback (assets.go's declaredPegFXMaxAge)
  -- runs its own, independent, wider 7-day tolerance. An alert
  -- stricter than the tightest tolerance any consumer actually uses
  -- reports a fault the system does not have.
  --
  -- 48h cannot survive a weekend. `massive` publishes a business-day
  -- snapshot and FX markets close, so Friday's bucket is the freshest
  -- thing that exists until Monday: Fri 00:00 → Mon 00:00 is 72h. The
  -- alert would fire EVERY weekend (observed at a Saturday 00:00Z
  -- with the worker healthy — `forex: fx_quotes persisted rows=818` every
  -- hour, and `stellarindex_external_fx_rate_rejected_total` zero for all
  -- reasons). 76h clears Monday's publish with slack.
  --
  -- Same reasoning as the `ecb` exception above, which was given 4 days
  -- for exactly this: a business-day reference needs a threshold that
  -- spans a market close, or it measures the calendar rather than our
  -- health. A feed genuinely dead on a Tuesday still trips this within
  -- the day.
  --
  -- `frankfurter-historical` is the provenance label of the one-off
  -- fx-history-backfill tool, not a feed: its newest row never advances.
  --
  -- One worker writes fx_quotes, stamping whichever feed answered (the
  -- primary, or the `ecb` standby during an outage), so only the feed(s)
  -- behind the newest bucket are expected to advance. Every other label
  -- is an idle standby or the primary it replaced; a dead worker still
  -- ages the newest bucket's series.
  SELECT 'fx', source, extract(epoch FROM now()-max(bucket)), 273600
    FROM fx_quotes
   WHERE source <> 'frankfurter-historical'
     AND bucket = (SELECT max(bucket) FROM fx_quotes WHERE source <> 'frankfurter-historical')
   GROUP BY source
  UNION ALL
  -- Sparse Soroban AMMs get 24h: phoenix's MEASURED 30-day gap
  -- distribution (3,278 trades) is max 8h28m / p99 3h12m,
  -- and a 12h+ genuine market lull false-fired the flat 4h threshold
  -- twice — with the lake confirming zero swap events on ANY known or
  -- unknown pool (quiet, not stale). 24h still
  -- catches a dead decoder within a day, and the ADR-0033 verdict
  -- (129600s below) remains the real correctness net.
  SELECT 'trades', source, extract(epoch FROM now()-max(bucket)),
         CASE WHEN source IN ('phoenix','comet') THEN 86400 ELSE 14400 END
    FROM source_volume_1h GROUP BY source
  UNION ALL
  SELECT 'supply', 'asset_supply_history', extract(epoch FROM now()-max(time)), 108000
    FROM asset_supply_history
  UNION ALL
  SELECT 'verdict', source, extract(epoch FROM now()-max(computed_at)), 129600
    FROM completeness_snapshots GROUP BY source
  UNION ALL
  -- Newest SUCCESSFUL fetch, not sep1_resolved_at: every failed attempt
  -- stamps that one, so it stays green through a refresh failing every
  -- domain. sep1_payload_fetched_at is stamped only on success.
  SELECT 'sep1', 'issuers', extract(epoch FROM now()-max(sep1_payload_fetched_at)), 172800
    FROM issuers WHERE sep1_payload_fetched_at IS NOT NULL
)
-- The age sample is emitted only when an age EXISTS; the verdict is emitted
-- always. A domain whose table holds no rows at all (the sep1 refresh that
-- never ran, a supply history that never started) has no age to report, and
-- the old form rendered `... ` with an empty value field — a line the
-- publication validator withholds, so the verdict went absent and every
-- `== 1` alert over it stayed quiet. Unknown is NOT healthy: no observation
-- ever is the most stale a domain can be, so it reads 1.
SELECT 'stellarindex_data_freshness_age_seconds{domain="'||domain||'",source="'||src||'"} '||round(age)::text
  FROM f WHERE age IS NOT NULL
UNION ALL
SELECT 'stellarindex_data_freshness_stale{domain="'||domain||'",source="'||src||'"} '||(COALESCE(age, thr + 1) > thr)::int::text
  FROM f;
SQL
then
  echo "data-freshness: per-domain oracle/fx/supply/cursor freshness query failed — its gauges skipped this tick, others unaffected" >&2
fi

# The `supply` domain above is max(time) over the whole table, so it only proves SOME asset
# publishes (it read green with 37 of 48 watched assets frozen). Emit the per-asset shape
# too: count of stale watched assets and the worst age. Two series, so cardinality does not
# grow with the watched set.
#
# The 30-day window bounds the WATCHED SET, not health, and is anchored to the newest supply
# row, not now(): anchored to now(), a freeze outlasting it empties the CTE and the gauge
# reports a healthy 0 exactly when every asset has stopped. Anchored to the newest row, a
# frozen set stays in view and keeps aging, while a truly retired asset still ages out.
# go on climbing, while an asset genuinely retired 30 days before the last
# publication still ages out of the watched set as intended.
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH newest AS (SELECT max(time) AS t FROM asset_supply_history),
     per_asset AS (
  SELECT asset_key, extract(epoch FROM now()-max(time)) AS age
    FROM asset_supply_history
   WHERE time > (SELECT t FROM newest) - interval '30 days'
   GROUP BY asset_key
)
SELECT 'stellarindex_supply_assets_stale '||count(*) FILTER (WHERE age > 108000)::text
  FROM per_asset
UNION ALL
SELECT 'stellarindex_supply_asset_max_age_seconds '||COALESCE(round(max(age)),0)::text
  FROM per_asset;
SQL
then
  echo "data-freshness: supply asset staleness query failed — its gauges skipped this tick, others unaffected" >&2
fi

# Per-source completeness verdict (latest snapshot per source): 1 = incomplete.
# The system 'recognition' row is EXCLUDED: it counts shapes on contracts NO source owns
# (the rest of Soroban, ~23k and growing), so complete=false there is the permanent expected
# state, not a served<>lake gap; including it would keep the ticket alert firing. Real
# silent drops show in each source's OWN recognition axis, which flips that source's row
# here. The system row is exported below as a COUNT so a registry regression shows as a step.
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_completeness_incomplete{source="'||source||'"} '||(NOT complete)::int::text
  FROM (SELECT DISTINCT ON (source) source, complete
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s
 WHERE source <> 'recognition';
SQL
then
  echo "data-freshness: completeness_incomplete verdict query failed — its gauges skipped this tick, others unaffected" >&2
fi

# Per-source recognition verdict, the OWNED bucket. attributeRecognitionGaps puts gaps on
# contracts a source OWNS on that source's recognition axis and unowned ones in the system
# `recognition` census. Only the owned kind is a defect: an indexed protocol emitted an
# event none of its decoders claimed, i.e. we silently drop data we promised to cover.
# The system row is excluded as above: it is the unowned census, and its recognition_ok is
# false by construction.
# and its recognition_ok is false permanently by construction (it can only be
# true if no un-indexed Soroban contract exists anywhere on the network).
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_recognition_ok{source="'||source||'"} '||recognition_ok::int::text
  FROM (SELECT DISTINCT ON (source) source, recognition_ok
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s
 WHERE source <> 'recognition';
SQL
then
  echo "data-freshness: recognition_ok verdict query failed — its gauges skipped this tick, others unaffected" >&2
fi

# System recognition census count, parsed from the snapshot detail text ("<N> unrecognized
# shape(s) on <M> unowned contract(s) ..."; clean state leads with "0"). The leading-digits
# contract is OWNED by completeness.FormatRecognitionDetail (internal/completeness/audit_axis.go)
# and pinned by TestFormatRecognitionDetail_leadsWithTheShapeCount: a reword dropping the
# numeric prefix would make this report 0 silently. COALESCE is a fallback for older rows.
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_recognition_unattributed_shapes '||
       COALESCE(substring(detail FROM '^[0-9]+'), '0')
  FROM (SELECT detail FROM completeness_snapshots
         WHERE source = 'recognition'
         ORDER BY computed_at DESC LIMIT 1) r;
SQL
then
  echo "data-freshness: recognition unattributed-shape census query failed — its gauges skipped this tick, others unaffected" >&2
fi

# Emptied continuous aggregates. A cagg recreated WITH NO DATA (migrations 0115/0147 drop
# and recreate all nine price/TWAP views and leave refresh_continuous_aggregate to an
# operator), or whose base rows a replay rewrote, is empty pending refresh and nothing else
# sees it: each refresh POLICY only re-fills a trailing window, so newest-bar and
# last-refresh checks read GREEN while back-history serves empty. The ADR-0033 verdict
# cannot see it either (derived views are not reconcile TARGETS).
#
# Judge every view against `trades` (raw, kept forever), not prices_1m's own min(bucket):
# 0147 empties prices_1m too, so that reference would be the policy's minutes-old sliver and
# the gauge would publish a healthy 0 with no OHLC history. A view is emptied when it is
# empty or its oldest bar trails the oldest trade by more than a day, except where an ARMED
# retention policy (0156 ships one disabled on prices_1m) moves the floor to now()-drop_after;
# reading timescaledb_information.jobs keeps that in step without a hardcoded window.
#
# Cheap: min(bucket)/min(ts) ride the time indexes. The 1-day slack absorbs time_bucket
# rounding down; the 2-day gate keeps a fresh deploy's initial materialization quiet.
# window from false-firing.
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH base AS (SELECT min(ts) AS tmin FROM trades),
     ret AS (
       SELECT hypertable_name AS view_name,
              (config->>'drop_after')::interval AS drop_after
         FROM timescaledb_information.jobs
        WHERE proc_name = 'policy_retention'
          AND scheduled
          AND config ? 'drop_after'
     ),
     v (view_name, vmin) AS (
       VALUES ('prices_1m'::text, (SELECT min(bucket) FROM prices_1m)),
              ('prices_15m',      (SELECT min(bucket) FROM prices_15m)),
              ('prices_1h',       (SELECT min(bucket) FROM prices_1h)),
              ('prices_4h',       (SELECT min(bucket) FROM prices_4h)),
              ('prices_1d',       (SELECT min(bucket) FROM prices_1d)),
              ('prices_1w',       (SELECT min(bucket) FROM prices_1w)),
              ('prices_1mo',      (SELECT min(bucket) FROM prices_1mo)),
              ('twap_1h',         (SELECT min(bucket) FROM twap_1h)),
              ('twap_1d',         (SELECT min(bucket) FROM twap_1d))
     ),
     j AS (
       SELECT v.view_name,
              (CASE WHEN b.tmin IS NOT NULL
                     AND b.tmin < now() - interval '2 days'
                     AND (v.vmin IS NULL
                          OR v.vmin > greatest(b.tmin,
                                               now() - COALESCE(r.drop_after, interval '100 years'))
                                      + interval '1 day')
                    THEN 1 ELSE 0 END) AS missing
         FROM v
         CROSS JOIN base b
         LEFT JOIN ret r ON r.view_name = v.view_name
     )
SELECT 'stellarindex_cagg_history_missing{view="'||view_name||'"} '||missing::text
  FROM j WHERE view_name LIKE 'prices%'
UNION ALL
SELECT 'stellarindex_twap_history_missing{view="'||view_name||'"} '||missing::text
  FROM j WHERE view_name LIKE 'twap%';
SQL
then
  echo "data-freshness: emptied-continuous-aggregate detector query failed — its gauges skipped this tick, others unaffected" >&2
fi

# A verdict can read complete=true while its watermark lags the network head (mid-walk stall,
# small manual -to). Emit per-source lag (ingest cursor tip - verdict watermark) so that is
# alertable rather than a green "N/N complete" badge.
if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH tip AS (SELECT max(last_ledger) AS t FROM ingestion_cursors)
SELECT 'stellarindex_completeness_watermark_lag_ledgers{source="'||s.source||'"} '
       ||greatest(0, (SELECT t FROM tip) - s.watermark_ledger)::text
  FROM (SELECT DISTINCT ON (source) source, watermark_ledger
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s;
SQL
then
  echo "data-freshness: completeness watermark lag query failed — its gauges skipped this tick, others unaffected" >&2
fi

# supply_flows (ClickHouse) backs /v1/assets SEP-41 supply (TokenSupply() sums it FINAL), so
# its freshness IS served SEP-41 supply freshness. Threshold is generous: supply events are bursty.
#
# BEST-EFFORT by design: ClickHouse is a separate daemon that can be down while every
# Postgres gauge is computable. A bare `SF_AGE=$(curl ... | tr ...)` would take curl's status
# under `set -euo pipefail`, abort the script, and the EXIT trap would remove $TMP so
# node_exporter re-serves the PREVIOUS file: every gauge FREEZES (stale sources keep reading
# 0) and the absent_over_time meta-alert cannot fire. So the probe is an `if` condition
# (exempt from `set -e`) and `-f` turns an HTTP 5xx into a failure instead of a metric value.
SF_AGE=""
if ! SF_AGE=$(curl -sS -f --max-time 15 --netrc-file "$CH_NETRC" http://localhost:8123/ --data-binary \
  "SELECT toUInt64(dateDiff('second', max(ingested_at), now())) FROM stellar.supply_flows" | tr -d '[:space:]'); then
  echo "data-freshness: ClickHouse supply_flows probe failed — sep41_supply gauges skipped this tick" >&2
  SF_AGE=""
fi
# Digits only: ONE unparseable sample makes node_exporter reject the WHOLE file.
case "$SF_AGE" in
  '' | *[!0-9]*) SF_AGE="" ;;
esac
if [ -n "$SF_AGE" ]; then
  printf 'stellarindex_data_freshness_age_seconds{domain="sep41_supply",source="supply_flows"} %s\n' "$SF_AGE" >> "$TMP"
  printf 'stellarindex_data_freshness_stale{domain="sep41_supply",source="supply_flows"} %s\n' \
    "$([ "$SF_AGE" -gt 3600 ] && echo 1 || echo 0)" >> "$TMP"
fi

# ── Validate the rendered bytes before publishing ──
#
# Everything above reached $TMP unexamined: the psql invocations write Postgres-chosen bytes
# straight in. node_exporter rejects the WHOLE file on one unparseable line (a NULL as an
# empty value, an error string, a command tag), taking every gauge and this watchdog's own
# meta-alert with it. Same grammar and sequence as timescale-jobs-probe.sh
# (roles/archival-node/tasks/10-observability.yml): parse the rendered bytes, then swap.
#
# The verdict on a bad line differs on purpose. That probe refuses to publish because an
# unpublished file AGES and its last-run gauge notices. Here there is no last-run gauge and
# the only meta-alert is absent_over_time(stellarindex_data_freshness_stale[45m])
# (deploy/monitoring/rules/data-freshness.yml); an unpublished file is re-served verbatim, so
# nothing goes absent and every gauge freezes. So publish and withhold only the offending
# lines: a withheld series is ABSENT, which every `== 1` predicate already tolerates.
#
# Withholding is not silent: each line is named on stderr, the count is its own gauge, and a
# non-zero count exits non-zero AFTER the swap, leaving the unit `failed` under
# stellarindex_systemd_unit_failed (ticket; unit deliberately not in
# scripts/ci/unit-failed-dedicated.baseline).
CHECKED="$(mktemp "${TMP}.XXXXXX")"
# Run as an `if` condition so a validator that cannot run is handled, not fatal under
# `set -e`. awk's stdout is the withheld-line tally.
if ! DROPPED="$(awk -v keep="$CHECKED" '
  # Comment lines (the HELP/TYPE headers) and blanks are exposition
  # too, and carry no value field: pass them through untouched.
  /^[ \t]*$/ || /^#/ { print > keep; next }
  {
    # Braces as [{] / [}], not backslash-escaped: in an ERE the brace
    # opens an interval expression and the escape is undefined by POSIX,
    # so the bracket form is the one that means the same thing under mawk
    # (the Debian default), gawk and BWK awk alike. NB no apostrophes
    # anywhere in this program: it is a single-quoted shell word and one
    # would end it. `print` with no argument writes $0 byte for byte —
    # no field splitting, no OFS rebuild — so a kept line is unmodified.
    rest = $0
    sub(/^[a-zA-Z_:][a-zA-Z0-9_:]*([{][^}]*[}])?[ \t]+/, "", rest)
    if (rest != $0 && rest ~ /^[+-]?([0-9]+\.?[0-9]*([eE][+-]?[0-9]+)?|\.[0-9]+([eE][+-]?[0-9]+)?|Inf|NaN)([ \t]+[0-9]+)?[ \t]*$/) {
      print > keep
      next
    }
    printf "data-freshness: withholding unparseable exposition line: %s\n", $0 > "/dev/stderr"
    bad = bad + 1
  }
  END { print bad + 0 }
' "$TMP")"; then
  rm -f "$CHECKED"
  echo "data-freshness: the exposition validator did not run — refusing to publish unvalidated bytes, since node_exporter drops an unparseable file whole and says so only in node_textfile_scrape_error. Previous data_freshness.prom left in place." >&2
  exit 1
fi
# The tally becomes a metric value, so digits-only as above; a non-count means nothing was
# validated, so refuse rather than coerce to 0.
case "$DROPPED" in
  '' | *[!0-9]*)
    rm -f "$CHECKED"
    echo "data-freshness: exposition validator returned a non-numeric tally ('$DROPPED') — refusing to publish. Previous data_freshness.prom left in place." >&2
    exit 1
    ;;
esac
mv "$CHECKED" "$TMP"
printf 'stellarindex_data_freshness_unparseable_lines %s\n' "$DROPPED" >> "$TMP"

# node_exporter runs unprivileged — mktemp defaults to 0600, so make the
# rendered file world-readable before the atomic swap or the collector skips it.
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
trap - EXIT

# Published; now be loud. A withheld line is a defect in the SQL above or in what Postgres
# answered. The good samples are already on disk, so exiting non-zero only fails the oneshot,
# which the stellarindex_systemd_unit_failed ticket already watches.
# stellarindex_systemd_unit_failed ticket already watches.
if [ "$DROPPED" -gt 0 ]; then
  echo "data-freshness: published data_freshness.prom with $DROPPED unparseable line(s) withheld — stellarindex_data_freshness_unparseable_lines carries the count" >&2
  exit 1
fi
