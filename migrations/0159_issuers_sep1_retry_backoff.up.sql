-- 0159 up — give the SEP-1 refresh queue a retry ladder, so a home_domain
-- that serves nothing stops costing an attempt a day forever.
--
-- WHAT IS WRONG TODAY
--
-- `IssuersNeedingSep1Refresh` selects `ORDER BY sep1_resolved_at ASC NULLS
-- FIRST`, and EVERY terminating path of the refresh — success, fetch
-- failure, marshal failure, write failure — stamps `sep1_resolved_at =
-- NOW()`. That stamp was added deliberately (a NULL pins a row at the head
-- of `NULLS FIRST` forever, so one poisoned issuer starved the whole queue),
-- but it makes the column mean "when we last TRIED", not "when we last
-- SUCCEEDED", and nothing else records the difference. A domain that has
-- 404'd two hundred times is therefore re-queued on exactly the same
-- schedule as one that answers every time.
--
-- Measured on the production lake, 2026-09-12:
--
--     issuers with a home_domain    76,658
--     never attempted                    1
--     attempted, no payload         40,837   (53%)
--     holding a payload             35,820
--
-- and that day's run: 209 succeeded, 291 failed — 58% of a 500-row budget
-- spent on domains that have never returned a document. `coinonstellar.com`
-- is NXDOMAIN; large families of parked subdomains (`*.litemint.store`,
-- `*.8888skulls.com`, `quantumstellar.vercel.app`) are the rest.
--
-- WHAT THIS MIGRATION ADDS
--
--   sep1_consecutive_failures  how many attempts in a row have produced no
--                              payload. Reset to 0 by a success; NULL means
--                              "no attempt has been judged under the ladder
--                              yet".
--
--   sep1_next_attempt_after    the earliest time the refresh should try this
--                              domain again. NULL = no deferral, i.e. the
--                              plain `-older-than` cadence applies.
--
-- The ladder itself lives in the writer (internal/storage/timescale/
-- issuers.go), not in a default here, because the base and the cap are
-- operational tuning: 1d, 2d, 4d, 8d, 16d, then a 30d cap. The FIRST step is
-- deliberately one day — the same cadence a healthy domain already gets — so
-- a single bad night (our DNS, our egress) costs a domain exactly nothing,
-- and a recovering domain returns to the fast cadence on its first success
-- rather than serving out a ladder it no longer deserves.
--
-- The cap is what keeps this a BACKOFF and not an eviction: a domain that
-- has failed a thousand times is still retried monthly, so an issuer that
-- finally publishes a stellar.toml is found without an operator doing
-- anything.
--
-- SEEDING, AND WHY IT IS A STATEMENT OF FACT
--
-- A row with `sep1_resolved_at IS NOT NULL AND sep1_payload IS NULL` has
-- been attempted at least once and has never yielded a document — that is
-- what those two columns mean, by construction, since the only writer that
-- sets a payload is SetIssuerSep1Payload. Seeding those rows with
-- `sep1_consecutive_failures = 1` says exactly that and nothing more. We do
-- NOT know how many times they have failed (nothing counted), so we do not
-- guess a higher number to buy faster relief; the ladder climbs from 1 on
-- its own within a month. `sep1_next_attempt_after` is left NULL for them:
-- their existing `sep1_resolved_at` values are already spread across the
-- current ~153-day rotation, so the queue order spreads the first
-- post-migration attempt without an artificial jitter.
--
-- The one `never attempted` row (sep1_resolved_at IS NULL) is untouched and
-- stays candidate #1, which is correct — it has earned no deferral.
--
-- THE INDEX
--
-- The queue read is `ORDER BY sep1_resolved_at ASC NULLS FIRST, g_strkey
-- ASC LIMIT n` over the ~77k rows with a home_domain, and it has never had
-- an index that serves it — `issuers_home_domain_idx` is on home_domain
-- alone. That was survivable at LIMIT=500 once a day; it is not the shape to
-- leave in place while raising the budget 36x.
--
-- `sep1_next_attempt_after` rides along as an INCLUDE column on purpose. A
-- deferred dead domain keeps an OLD `sep1_resolved_at` while it waits, so it
-- sorts to the FRONT of this index and is scanned-and-skipped on every run
-- until its deferral expires — tens of thousands of entries per run. In the
-- INCLUDE payload that filter is answered from the index; in the heap it is
-- a random page fetch per skipped row.
--
-- OLD-BINARY SAFETY (migrations README rule 9)
--
-- Two new NULLABLE columns, no defaults, so no table rewrite. The previous
-- released binary (v0.75.0) reads neither: its queue SELECT lists
-- `g_strkey, home_domain`, its `MarkIssuerSep1Attempted` is `UPDATE issuers
-- SET sep1_resolved_at = NOW()`, and its `SetIssuerSep1Payload` sets
-- `sep1_payload` + `sep1_resolved_at`. Running against this schema it
-- behaves exactly as it does today — it simply never defers anything, and
-- the seeded counter sits unread. The CHECK is tolerant of NULL and of every
-- value that binary can produce (it produces none).
--
-- HISTORY, so the next reader does not think this is a revival. Migration
-- 0023 created a `sep1_resolved_status text NOT NULL DEFAULT 'pending' CHECK
-- (… IN ('pending','ok','fetch_failed','parse_failed','tls_failed'))` — but
-- on the `anchors` table, NOT on `issuers`, and 0152 dropped `anchors` whole
-- as one of six never-wired scaffold tables (no Go reader, no Go writer, 0
-- rows in every environment). Nothing on `issuers` has ever recorded a
-- failure. These two columns are not that column returning: they are a
-- RETRY SCHEDULE, written by the refresh job on every run and read by its
-- own queue, not a status enum for a UI badge. If a per-failure reason is
-- ever wanted for display, it belongs in the migration that adds the writer
-- and the reader together — that is precisely what 0152 was cleaning up.
--
-- Runtime: two catalog-only ADD COLUMNs, one UPDATE over ~41k rows of a
-- plain, uncompressed, non-hypertable table, and one index build over ~190k
-- rows. Seconds.

BEGIN;

ALTER TABLE issuers
    ADD COLUMN sep1_consecutive_failures integer,
    ADD COLUMN sep1_next_attempt_after   timestamptz;

ALTER TABLE issuers ADD CONSTRAINT issuers_sep1_consecutive_failures_check  -- migration-compat:ok new nullable column; the previous released binary never writes sep1_consecutive_failures, so no value it can produce is rejected
    CHECK (sep1_consecutive_failures IS NULL OR sep1_consecutive_failures >= 0);

COMMENT ON COLUMN issuers.sep1_consecutive_failures IS
    'Attempts in a row that produced no SEP-1 payload (dead domain, TLS '
    'error, unparseable TOML, SSRF-blocked, write failure). Reset to 0 by a '
    'success. NULL = no attempt has been judged under the retry ladder yet. '
    'Not a total: it counts the CURRENT failing streak, so a domain that '
    'recovers starts over.';

COMMENT ON COLUMN issuers.sep1_next_attempt_after IS
    'Earliest time the SEP-1 refresh should try this home_domain again. Set '
    'by the failure writer to now() + min(1 day * 2^(prior failures), 30 '
    'days); cleared by a success. NULL = no deferral, the plain -older-than '
    'cadence applies. The 30-day cap is deliberate — this defers a domain, '
    'it never evicts one, so an issuer that finally publishes a stellar.toml '
    'is picked up without operator action.';

COMMENT ON COLUMN issuers.sep1_resolved_at IS
    'When the SEP-1 refresh last ATTEMPTED this issuer — not when it last '
    'succeeded. Every terminating path stamps it, so a row can carry a '
    'recent timestamp and a NULL sep1_payload. Read sep1_consecutive_failures '
    'to tell the two apart.';

-- Every row here has been attempted and has never produced a document; that
-- is what (sep1_resolved_at IS NOT NULL, sep1_payload IS NULL) means, given
-- SetIssuerSep1Payload is the only writer of the payload. One is the exact,
-- provable claim. A larger seed would be a guess.
UPDATE issuers
   SET sep1_consecutive_failures = 1
 WHERE sep1_resolved_at IS NOT NULL
   AND sep1_payload IS NULL
   AND sep1_consecutive_failures IS NULL;

CREATE INDEX issuers_sep1_refresh_queue_idx
    ON issuers (sep1_resolved_at ASC NULLS FIRST, g_strkey ASC)
    INCLUDE (sep1_next_attempt_after)
 WHERE home_domain IS NOT NULL AND home_domain <> '';

COMMIT;
