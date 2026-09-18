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
  echo '# HELP stellarindex_recognition_unattributed_shapes UNKNOWN-PROTOCOL events: distinct (contract, topic) shapes in the lake on contracts NO source owns. Foreign protocols we do not index. Expected large and permanently growing as Stellar grows — 23866 as of 2026-09-01 against ~30/day organic drift. A coverage-ambition number for prioritising which decoders to build next, NOT an error condition. Deliberately NOT alerted on: see stellarindex_recognition_ok for the signal that means a real defect.'
  echo '# TYPE stellarindex_recognition_unattributed_shapes gauge'
  echo '# HELP stellarindex_recognition_ok UNRECOGNIZED EVENTS FROM A KNOWN PROTOCOL: 0 when a source WE INDEX emitted a (contract, topic) shape none of its decoders claimed, 1 when every shape on its owned contracts decodes. This is the bucket that means OUR bug — a protocol we promised to cover is silently dropping events — and it is the one worth paging on. Distinct from unattributed_shapes, which counts other people contracts and is inert.'
  echo '# TYPE stellarindex_recognition_ok gauge'
  echo '# HELP stellarindex_cagg_history_missing 1 when a price continuous aggregate (prices_1m..prices_1mo) holds less history than the trades hypertable it aggregates — a migration recreated it WITH NO DATA, or a replay rewrote its base rows, and the manual refresh_continuous_aggregate follow-up never ran. Its refresh policy re-fills only a trailing sliver, so every newest-bar and last-refresh signal reads green while the back-history serves empty.'
  echo '# TYPE stellarindex_cagg_history_missing gauge'
  echo '# HELP stellarindex_twap_history_missing 1 when a TWAP continuous aggregate is missing the history prices_1m holds — a migration recreated/emptied it (WITH NO DATA) and the manual refresh_continuous_aggregate follow-up never ran.'
  echo '# TYPE stellarindex_twap_history_missing gauge'
  # The three families below are composed inside the SQL blocks further
  # down rather than by a printf here, which is how they came to be the
  # only ones in this file with no header of their own. An undeclared
  # family is scraped untyped and its meaning lives nowhere the operator
  # reading a scrape can see it. Declared here with the rest.
  echo '# HELP stellarindex_supply_assets_stale Watched assets whose newest asset_supply_history row is older than 30h. An aggregate max(time) cannot see a PARTIAL freeze: on 2026-07-28 a handful of live assets kept it green while 37 of 48 had stopped (CS-102).'
  echo '# TYPE stellarindex_supply_assets_stale gauge'
  echo '# HELP stellarindex_supply_asset_max_age_seconds Worst per-asset supply age in seconds across the watched set, 0 when none is known.'
  echo '# TYPE stellarindex_supply_asset_max_age_seconds gauge'
  echo '# HELP stellarindex_completeness_watermark_lag_ledgers Ledgers between the live ingest tip and a source latest ADR-0033 verdict watermark. A verdict can read complete=true while it was only ever verified up to an old ledger (CS-090).'
  echo '# TYPE stellarindex_completeness_watermark_lag_ledgers gauge'
  # Publication safety, not a data signal. Emitted on EVERY run so the
  # healthy value (0) is a series that exists rather than an absence: see
  # the validator just above the atomic swap for why a line that is not a
  # metric is withheld instead of being allowed to take the file down.
  echo '# HELP stellarindex_data_freshness_unparseable_lines Exposition lines this run rendered that are not valid Prometheus samples and were therefore WITHHELD from the published textfile. 0 every healthy run; above 0 means a query answered with something that is not a number (a NULL, an error string, a command tag) and that one series is absent this tick.'
  echo '# TYPE stellarindex_data_freshness_unparseable_lines gauge'
} > "$TMP"

# (domain, source, age_seconds, threshold_seconds) per domain. Thresholds are a
# generous multiple of each domain's natural cadence so only a real stall fires.
#
# ── NO WINDOW ON THE SOURCE ENUMERATION (F149) ───────────────────────────
# Each per-source leg below used to read `WHERE <time col> > now() - interval
# '30 days' GROUP BY source`, which makes the window that defines the universe
# the same window that defines health. A source dead longer than it — the
# coingecko-quota shape, 11 days and climbing — leaves the GROUP BY entirely,
# its stale series goes ABSENT rather than to 1, Prometheus ages it out, and
# `stellarindex_data_source_stale == 1` RESOLVES. The watchdog got quieter the
# worse the outage got, and the same window silenced the supply leg at 7 days.
#
# So the universe is now every source the table has EVER held, and the age is
# measured against that source's own newest row. The scan cost is unchanged
# where it mattered: `oracle_updates` is a hypertable on `ts`, and the old
# predicate was on `ingested_at` — not a dimension, so it never pruned a chunk;
# `fx_quotes` is daily-grain (one row per ticker per bucket, upserted), so its
# whole history is small; `asset_supply_history` answers max(time) from its
# time index.
#
# The cost of the change is the other direction: a source RETIRED on purpose
# keeps its history and therefore keeps reporting stale. That is the
# fail-closed side of the trade and it is deliberate — a retired feed is a
# deliberate act with an operator behind it, a dead feed is not. Retiring one
# means deleting its rows (or excluding it here in the same change), and the
# ticket it raises until then is the reminder.
#
# shellcheck disable=SC2129  # the appends below are separate on purpose: each is a distinct query with its own reasoning between them, and a single `{ … } >> $TMP` would bury that
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
WITH f AS (
  -- Crypto oracles (reflector/redstone/band/chainlink/coingecko) update every
  -- few minutes → 3h threshold. ECB is the exception: a DAILY FX reference
  -- (publishes ~16:00 CET on TARGET business days, none on weekends/holidays),
  -- so it needs a 4-day threshold to tolerate a weekend + a holiday without
  -- false-firing — otherwise it reads stale ~21h of every day.
  SELECT 'oracle'  AS domain, source AS src, extract(epoch FROM now()-max(ingested_at)) AS age,
         CASE WHEN source = 'ecb' THEN 345600 ELSE 10800 END AS thr
    FROM oracle_updates GROUP BY source
  UNION ALL
  -- FX is daily-grain: observed_at is the data-point time (lags ~a day even
  -- when healthy), so freshness is measured off `bucket` (today's bucket
  -- written = the worker is alive).
  --
  -- 76h, NOT 48h, and the number is not arbitrary: it mirrors
  -- `aggregate.composite_reference.fx_max_age_hours` (default 76,
  -- internal/config/config.go) — the budget the SERVING path already
  -- applies to its FX leg. An alert stricter than the tolerance the code
  -- actually uses reports a fault the system does not have.
  --
  -- 48h could not survive a weekend. `massive` publishes a business-day
  -- snapshot and FX markets close, so Friday's bucket is the freshest
  -- thing that exists until Monday: Fri 00:00 → Mon 00:00 is 72h. The
  -- alert therefore fired EVERY weekend (#370, observed 2026-08-30 00:00Z
  -- with the worker healthy — `forex: fx_quotes persisted rows=818` every
  -- hour, and `stellarindex_external_fx_rate_rejected_total` zero for all
  -- reasons). 76h clears Monday's publish with slack.
  --
  -- Same reasoning as the `ecb` exception above, which was given 4 days
  -- for exactly this: a business-day reference needs a threshold that
  -- spans a market close, or it measures the calendar rather than our
  -- health. A feed genuinely dead on a Tuesday still trips this within
  -- the day.
  SELECT 'fx', source, extract(epoch FROM now()-max(bucket)), 273600
    FROM fx_quotes GROUP BY source
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
    FROM asset_supply_history
  UNION ALL
  SELECT 'verdict', source, extract(epoch FROM now()-max(computed_at)), 129600
    FROM completeness_snapshots GROUP BY source
  UNION ALL
  SELECT 'sep1', 'issuers', extract(epoch FROM now()-max(sep1_resolved_at)), 172800
    FROM issuers WHERE sep1_resolved_at IS NOT NULL
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

# CS-102: the `supply` domain above measures max(time) across the WHOLE table,
# so it only proves SOME asset is publishing. On 2026-07-28 that read green
# while 37 of 48 watched assets had frozen — a handful of live assets kept the
# global max current and hid the rest. An aggregate cannot see a partial
# freeze, so emit the per-asset shape too: how many watched assets are stale,
# and the worst age among them. Low cardinality (two series) on purpose —
# per-asset series would grow with the watched set.
#
# The 30-day window here bounds the WATCHED SET, not the health verdict, and it
# is anchored to the newest supply row in the table rather than to now() (F149).
# Anchored to now(), a freeze that outlasts it empties the CTE and the gauge
# reports a literal healthy 0 — `count(*) FILTER (age > 108000)` over no rows —
# at the exact moment every watched asset has stopped. Anchored to the domain's
# own newest observation, the set an all-stop froze stays in view and its ages
# go on climbing, while an asset genuinely retired 30 days before the last
# publication still ages out of the watched set as intended.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
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

# Per-source recognition verdict — the OWNED bucket.
#
# attributeRecognitionGaps splits every unrecognized shape two ways: gaps on
# contracts a source OWNS cap that source's recognition axis, and gaps on
# unowned contracts flow to the system `recognition` row as the unattributed
# census. Only the first kind is a defect — it means a protocol we index
# emitted an event none of its decoders claimed, so we are silently dropping
# data we promised to cover.
#
# That bucket was computed but never exported: recognition_ok has been a
# column on completeness_snapshots all along and appeared only in the API's
# /v1/coverage response. So the harmless number (unattributed_shapes) carried
# a metric AND an alert, while the harmful one had neither.
#
# The system `recognition` row is excluded for the same reason it is excluded
# from completeness_incomplete above: it is the unowned census, not a source,
# and its recognition_ok is false permanently by construction (it can only be
# true if no un-indexed Soroban contract exists anywhere on the network).
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_recognition_ok{source="'||source||'"} '||recognition_ok::int::text
  FROM (SELECT DISTINCT ON (source) source, recognition_ok
          FROM completeness_snapshots ORDER BY source, computed_at DESC) s
 WHERE source <> 'recognition';
SQL

# System recognition census count, parsed from the snapshot's detail text
# ("<N> unrecognized shape(s) on <M> unowned contract(s) (earliest ledger L) —
# …"; the clean state leads with a literal "0"). The leading-digits contract is
# OWNED by completeness.FormatRecognitionDetail (internal/completeness/audit_axis.go)
# and pinned by TestFormatRecognitionDetail_leadsWithTheShapeCount, because a
# reword that dropped the numeric prefix would make this substring() report 0
# shapes silently — the under-reporting direction. The COALESCE is kept as a
# belt-and-braces fallback for a row written before that format existed.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
SELECT 'stellarindex_recognition_unattributed_shapes '||
       COALESCE(substring(detail FROM '^[0-9]+'), '0')
  FROM (SELECT detail FROM completeness_snapshots
         WHERE source = 'recognition'
         ORDER BY computed_at DESC LIMIT 1) r;
SQL

# W1-migrations-1 / REC-01 / F047 / F116 / T425: a continuous aggregate that a
# migration recreated WITH NO DATA — or that a replay rewrote the base rows of —
# is EMPTIED PENDING REFRESH, and nothing else in this system can see that
# state. Migrations 0115 and 0147 both DROP and recreate all nine price/TWAP
# views and leave `CALL refresh_continuous_aggregate(...)` to an operator
# banner; a fresh database (a new region, a test net, a restore) applies them
# from zero and is left the same way. The trap that hides a skipped follow-up:
# each view's refresh POLICY only auto-materializes its trailing start_offset
# window (prices_1m 5 minutes, twap_1h 4 hours), so the NEWEST bars reappear on
# the next policy tick and every newest-bar freshness/age/last-refresh check —
# stellarindex_cagg_last_refresh_unix included — reads GREEN while the entire
# back-history serves empty. The ADR-0033 completeness verdict cannot see it
# either: these are derived price views, not reconcile TARGETS.
#
# So detect it directly, and judge EVERY view against the thing that is not
# emptied with them. The reference is `trades` — the raw hypertable all nine
# aggregate, kept forever (migration 0031 removed its retention). It used to be
# prices_1m's own min(bucket), which made the detector self-muting in exactly
# the scenario it names: 0147 empties prices_1m TOO, so after it runs the
# reference is the policy's minutes-old sliver, `tmin > pmin + 1 day` is false
# for every view, and the gauge publishes a healthy 0 for a database with no
# OHLC history at all.
#
# A view is emptied-pending-refresh when it is empty, or when its OLDEST
# materialized bar trails the oldest bar it OUGHT to hold by more than a day.
# That floor is the oldest trade — except where an ARMED retention policy makes
# a shorter history correct: migration 0156 attaches a 90-day retention to
# prices_1m and ships it disabled, and an operator who arms it deliberately
# moves that view's floor to now()-90d. Reading the armed policies out of
# timescaledb_information.jobs keeps the two facts in step automatically
# instead of hardcoding a window here that a later arming would falsify.
#
# Cheap: min(bucket) rides each view's time index and min(ts) rides the trades
# time index. The 1-day slack absorbs bucket truncation (time_bucket rounds
# DOWN, so a healthy view's oldest bar is at or before the oldest trade) and
# the 2-day gate keeps a fresh deploy's legitimate initial-materialization
# window from false-firing.
"$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA -F$'\t' >> "$TMP" <<'SQL'
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
#
# BEST-EFFORT, and it has to stay that way: this is the only non-Postgres
# probe in the script, and ClickHouse is a separate daemon that can be down
# while every Postgres-derived gauge above is perfectly computable. A bare
# `SF_AGE=$(curl … | tr …)` assignment took curl's status under
# `set -euo pipefail`, so a CH outage ABORTED the script here — the EXIT
# trap removed $TMP, the atomic `mv` below never ran, and node_exporter
# went on re-serving the PREVIOUS data_freshness.prom verbatim. Every gauge
# then FROZE at its last value rather than going absent: stale sources kept
# reading `stellarindex_data_freshness_stale 0`, and the watchdog's own
# meta-alert (absent_over_time(...[45m])) could not fire either, because
# the series were still present. One ClickHouse outage silenced the whole
# "never get behind" layer (Wave L, #319).
#
# Running the probe as an `if` condition exempts it from `set -e`; `-f`
# turns an HTTP 5xx into a failure instead of an error body that would be
# emitted as a metric value.
SF_AGE=""
if ! SF_AGE=$(curl -sS -f --max-time 15 http://localhost:8123/ --data-binary \
  "SELECT toUInt64(dateDiff('second', max(ingested_at), now())) FROM stellar.supply_flows" | tr -d '[:space:]'); then
  echo "data-freshness: ClickHouse supply_flows probe failed — sep41_supply gauges skipped this tick" >&2
  SF_AGE=""
fi
# Digits only. A partial or non-numeric body must not reach the textfile:
# ONE unparseable sample makes node_exporter reject the WHOLE file, taking
# every other gauge in it down with the probe.
case "$SF_AGE" in
  '' | *[!0-9]*) SF_AGE="" ;;
esac
if [ -n "$SF_AGE" ]; then
  printf 'stellarindex_data_freshness_age_seconds{domain="sep41_supply",source="supply_flows"} %s\n' "$SF_AGE" >> "$TMP"
  printf 'stellarindex_data_freshness_stale{domain="sep41_supply",source="supply_flows"} %s\n' \
    "$([ "$SF_AGE" -gt 3600 ] && echo 1 || echo 0)" >> "$TMP"
fi

# ── Validate the rendered bytes before publishing ─────────────────────
#
# Everything above this line reached $TMP UNEXAMINED. Seven psql
# invocations write their stdout straight into it and compose their
# exposition text inside SQL, so nothing in this shell script even looks
# like a metric line — which is why this producer was the one left
# unhardened when the class was gated (c8ceb7c55), and why it carries
# the largest exposure of the 21: 162 samples on r1 (measured
# 2026-09-10), every byte of every value chosen by Postgres.
#
# node_exporter does not skip an unparseable line, it rejects the WHOLE
# file. One NULL rendering as an empty value field, one error string,
# one stray command tag would take all 162 off the host together — the
# per-domain freshness gauges, the ADR-0033 completeness verdicts, the
# recognition axis — and take this watchdog's own meta-alert with them,
# because the series it reads is in this same file. That is exactly the
# shape that cost 127 unrelated stellarindex_timescale_* series ~20
# minutes of darkness.
#
# Same grammar, and the same place in the sequence, as
# timescale-jobs-probe.sh (roles/archival-node/tasks/10-observability.yml):
# parse the rendered bytes, then swap. What differs is the VERDICT on a
# bad line, and the difference is deliberate.
#
#   The probe REFUSES to publish and exits 1. Its stated reason is that
#   a file not published AGES, and `time() - stellarindex_timescale_
#   probe_last_run_unix` is precisely the arm of
#   stellarindex_timescale_probe_degraded that notices.
#
#   That premise is false here. This producer emits no last-run gauge,
#   and its only meta-alert is
#   `absent_over_time(stellarindex_data_freshness_stale[45m])`
#   (deploy/monitoring/rules/data-freshness.yml). A file left unpublished
#   is re-served verbatim by node_exporter on every scrape with a fresh
#   timestamp: NOTHING goes absent, so that alert cannot fire, and every
#   gauge FREEZES at its last value — genuinely stale sources keep
#   reading stellarindex_data_freshness_stale 0. That is not a
#   hypothetical; it is Wave L / #319, the incident the ClickHouse probe
#   above is shaped the way it is (an `if`, not a bare assignment) to
#   avoid. Refusing here would re-admit through the front door the
#   failure that fix removed through the back.
#
# So: publish, and withhold only the offending lines. 161 true samples
# beat 162 frozen ones, and a withheld line leaves its series ABSENT —
# where every alert predicate over this file is `== 1` and was already
# quiet on an absence — rather than leaving it asserting a falsehood.
#
# The withholding is NOT silent, which is the whole objection to dropping
# lines. Each one is named on stderr (journal), the count is published as
# a gauge of its own, and a non-zero count exits non-zero AFTER the swap,
# which leaves data-freshness.service in `failed` and therefore under
# stellarindex_systemd_unit_failed (infra.yml, severity ticket) — the
# unit is deliberately not in scripts/ci/unit-failed-dedicated.baseline,
# so that signal costs no new rule. Loud, and never at the cost of the
# other families in this file.
CHECKED="$(mktemp "${TMP}.XXXXXX")"
# Run as an `if` condition so a validator that cannot run is HANDLED
# rather than killing the script under `set -e` — same reasoning as the
# ClickHouse probe above. awk's stdout is the withheld-line tally.
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
# The tally is about to become a metric value, so it gets the same
# digits-only treatment as the ClickHouse body above. A validator that
# answered with something other than a count has validated nothing, so
# this is a refusal rather than a coercion to 0.
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

# Published — now be loud. A withheld line means this producer rendered
# something that is not a metric, which is a defect in the SQL above or in
# what Postgres answered, and the operator has to see it. The 161 good
# samples are already on disk by this point, so exiting non-zero costs
# nothing but a failed oneshot — which the catch-all
# stellarindex_systemd_unit_failed ticket already watches.
if [ "$DROPPED" -gt 0 ]; then
  echo "data-freshness: published data_freshness.prom with $DROPPED unparseable line(s) withheld — stellarindex_data_freshness_unparseable_lines carries the count" >&2
  exit 1
fi
