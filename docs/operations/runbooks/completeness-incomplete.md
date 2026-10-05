---
title: Runbook — completeness verdict incomplete
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_completeness_incomplete`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_completeness_incomplete` |
| Severity | **P3** (ticket) |
| Detected by | `stellarindex_completeness_incomplete{source} == 1` for > 1h |
| Emitted by | `data-freshness.sh` from the latest `completeness_snapshots` row per source |
| Typical MTTR | minutes to verify; a re-derive can be longer (chunked) |
| Impact | The served tier no longer reconciles to the certified ClickHouse lake for that source (ADR-0033 `complete=false`) — a real served<>lake gap, i.e. served data is incomplete vs the proven substrate. |

## Symptoms

`stellarindex_completeness_incomplete{source="X"} = 1`. The ADR-0033 verdict
(`compute-completeness`, daily) found served counts ≠ the lake re-derive for
source `X`. Since the verdict is trustworthy + self-maintaining as of rc.149
(it preseeds factory children), this is a **real gap**, not a checker artifact.

## Quick diagnosis (≤ 5 min)

```sh
# The exact Δ + window the verdict recorded:
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, complete, watermark_ledger, detail FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s WHERE NOT complete;"
# Re-run that source to confirm it persists (off the serving DB, -ch):
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -source <X> -from <recent>
```

## Mitigation (≤ 15 min to start)

Re-derive the flagged source from the certified lake, then re-verify:

- **Soroban projected sources** (`trades`/protocol tables):

  ```sh
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source <X> -from <F> -write
  ```

  `-config`, `-source` and `-from` are all REQUIRED, and `-write` is required
  to actually rewind (dry run is the default). The source name is the
  projector registry's name, not the hyphenated table name.
- **Non-projected** (`sdex`, soroban-events):

  ```sh
  stellarindex-ops backfill -write -config /etc/stellarindex.toml \
    -source <X> -from <F> -to <T>
  ```

  or the CH re-derive (`ch-rebuild`, also `-config`-required).
- Then re-run `compute-completeness -ch -source <X>`; the gauge clears when it
  returns `complete=true`. Run chunked + off-peak (the SDEX/heavy re-derives
  blow ClickHouse's per-query memory limit over large windows).

  Since CS-095 this manual re-verify is a way to clear the gauge SOONER, not
  the only way: the nightly `-pass` floors a source whose prior projection
  verdict is failing at its genesis, so it re-verifies the whole served range
  on its own and publishes `complete=true` once the repair earns it. Before
  CS-095 the pass resumed a red source's projection reconcile from the LAKE
  watermark (at tip whenever the lake is clean), so it reconciled `[tip, tip]`,
  never re-saw the failing range, and carried the red forward every night — a
  repaired source stayed red until someone ran the command above by hand.

  That re-verify can outlast the pass's deadline (`-timeout`, default `120m`):
  on r1, sdex's served range from 61249957 is ~3.44M ledgers at ~200
  ledgers/s ≈ 4.8 h. The pass therefore runs every from-genesis source LAST and
  publishes the `recognition` row before the per-source loop, so only the
  re-verifying tail is left unevaluated (named in the pass's error). To grant a
  one-off larger budget, set `PASS_TIMEOUT` (e.g. `PASS_TIMEOUT=300m`) in the
  `/etc/default/compute-completeness` AND raise `TimeoutStartSec` (34200 s: a 23400 s lock wait + 180 min) by as much in
  `compute-completeness.service`, or systemd kills the pass first — or clear
  the source by hand with the chunked `-source` re-run above.

## Stale: projection evidence older than 10 d 6 h, or unknown

`/v1/coverage` sets `flags.stale` when a source claiming `projection_ok` has
`projection_evidenced_at` older than `MaxProjectionCarryAge` (7 d) plus three
26 h audit periods, or `null`. `computed_at` cannot show this: the nightly
`-pass` restamps it while carrying the old claim. The carry detail names the
proof time ("the carried prefix was last reconciled in full at …", or "has no
full-range reconcile on record").

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, projection_evidenced_at, computed_at FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s \
  WHERE projection_ok ORDER BY projection_evidenced_at NULLS FIRST;"
```

- **Event sources** clear on their own. Each `-pass` re-proves from genesis
  the expired (> 7 d, or `null`) sources, oldest first, at most three per night
  (the pass logs `re-proving expired projection evidence from genesis this
  pass: …`). Right after migration 0201 every green source is `null`, so the
  flag holds for about `ceil(green sources / 3)` nights. If a source stays
  expired past that, check the pass's error for a deadline cut.
- **`sdex` (the census) is re-proved weekly by its own timer.** Its full
  re-proof takes ~4.8 h, longer than the pass's 120 min, so the pass never
  re-floors it. `compute-completeness-sdex.timer` (Sunday 18:47 UTC) runs the
  nightly driver as `-source sdex -timeout 360m` under the same
  `run-heavy-job.sh` job name, so it never overlaps the nightly pass. Right
  after migration 0201, sdex stays `null` until that first Sunday run. If the
  timer failed or missed a week (`systemctl status compute-completeness-sdex`,
  `journalctl -u compute-completeness-sdex`), re-run it off-peak and outside
  the 05:30 UTC pass window:

  ```sh
  sudo systemctl start --no-block compute-completeness-sdex.service
  ```

  `sep41_transfers` is handled the same way by
  `compute-completeness-sep41.timer` (Wednesday 18:47 UTC,
  `-source sep41_transfers -timeout 360m`): the watched KALE SAC puts its
  CAP-67 transfers in nearly every lake granule, so its from-genesis re-proof
  overruns the pass's 45 min `-source-timeout` and the pass never forces it.
  Re-run with `sudo systemctl start --no-block compute-completeness-sep41.service`.

  The unit is the fallback to prefer: it applies the driver's tip−100 margin,
  without which undrained ledgers read as sdex mismatches. Do not add
  `-from`: a run that starts above the served floor carries the range below it
  and stamps no evidence, so chunked `-from` runs cannot clear this.

## Pending: a deferred dirty window (sdex, sep41_transfers)

If the source's `detail` reads `dirty window [F,T] PENDING this source's
dedicated weekly compute-completeness timer`, nothing was found wrong. A
`backfill -write`, `ch-rebuild` or `projector-replay` rewrote served rows from
`F`, and re-checking from `F` to tip is more than one day of ledgers, which
does not fit the nightly `-pass`. The pass withholds `complete` and holds the
watermark below `F` instead of carrying the old claim over the rewrite.

Do not re-derive. Re-prove the source with the weekly budget, which clears the
window once it reconciles clean:

```sh
sudo systemctl start compute-completeness-sdex.service   # or compute-completeness-sep41.service
# equivalent: run-compute-completeness.sh -source <X> -timeout 360m
```

A window that fits the pass (re-check from `F` within a day of ledgers) is
re-checked and cleared by the next nightly run, with no action.

## Root cause analysis

A served<>lake divergence: dropped rows (a decoder bug fixed forward-only, e.g.
the SEP-41 CAP-67 loss), a missed projection window, or a retention/PK artifact.
The `detail` column names the per-target Δ and window.

## Known false-positive patterns

- Pre-rc.149: factory-gated sources (blend) false-fired because the verdict's
  childgate wasn't self-seeded — **fixed**; if a NEW factory-gated source
  false-fires, confirm its creation events are reachable in `soroban_events`.
- **The `recognition` row is excluded on purpose and is NOT a source.**
  `data-freshness.sh` filters `source <> 'recognition'` out of this gauge: that
  row counts event shapes on contracts no source owns (the rest of the Soroban
  ecosystem, ~23k shapes growing ~30/day), so `complete=false` there is the
  permanent expected state of a curated indexer, not a served↔lake gap. Folding
  it in kept this ticket alert firing continuously from 2026-08-17. The
  unattributed census that row carries is exported separately as
  `stellarindex_recognition_unattributed_shapes` — shapes on contracts NOBODY
  owns, i.e. foreign protocols. It is large and permanently growing, and is
  deliberately NOT alerted on. The signal that means a real defect is
  per-source: `stellarindex_recognition_ok == 0`, meaning a protocol we DO
  index emitted something its decoders could not claim
  ([source-recognition-failing](source-recognition-failing.md)). Until
  2026-09-01 the alert watched the census and inferred the defect from its
  growth rate; it now reads the per-source axis directly.
- **A green verdict with a lagging watermark.** `complete=true` only speaks for
  the range that was walked; `stellarindex_completeness_watermark_lag_ledgers`
  (CS-090) is the companion gauge for "verified, but only up to an old ledger".

## Related

- `stellarindex_data_source_stale` — a source not ingesting at all
  ([data-source-stale](data-source-stale.md)).
- [ADR-0033](../../adr/0033-completeness-verification-model.md) — the
  completeness-verification model (there is no
  `docs/architecture/completeness-verification.md`).
- `docs/operations/launch-todo.md` Phase C.

## Changelog

- 2026-10-05 — a dirty window too wide for the nightly `-pass` (sdex,
  sep41_transfers) is deferred to the weekly re-proof and reads as pending
  re-verification, not a gap; see "Pending: a deferred dirty window".
- 2026-10-04 — `sep41_transfers` left the pass's forced re-proof;
  `compute-completeness-sep41.timer` re-proves it weekly (Wednesday).

- 2026-10-02 — added the projection-evidence stale reason (migration 0201):
  the `-pass` re-proves up to three expired sources per night;
  `compute-completeness-sdex.timer` re-proves `sdex` weekly.
- 2026-09-30 — the nightly `-pass` orders from-genesis re-verifies last,
  writes the `recognition` row first, and takes `-timeout` / `PASS_TIMEOUT`.
- 2026-09-09 — CS-095: the nightly `-pass` now re-verifies a source whose prior
  projection verdict was failing, instead of resuming it from the lake
  watermark and carrying the red forward forever. Mitigation section updated:
  the manual `compute-completeness -ch -source <X>` is now a way to clear the
  gauge sooner, not the only way.
- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): the
  "SEP-41 is EXCLUDED from the verdict" bullet is obsolete — P1-7 is DONE and
  `sep41_transfers`/`sep41_supply_events` have been promoted into the
  reconciliation catalogue whenever `[supply] watched_sep41_contracts` is
  configured since 2026-07-11; `docs/architecture/completeness-verification.md`
  does not exist → ADR-0033; both catch-up commands omitted the REQUIRED
  `-config` (and `projector-replay` also needs `-write`, or it is a dry run);
  added the recognition-census exclusion and the CS-090 watermark-lag note.
- 2026-06-30: created with the data-freshness watchdog.
