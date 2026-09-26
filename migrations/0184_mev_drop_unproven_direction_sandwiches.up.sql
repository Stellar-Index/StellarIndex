-- 0184 up — delete sandwich and oracle_sandwich accusations whose stored
-- evidence does not prove opposite-direction brackets.
--
-- WHAT IS WRONG TODAY
--
-- /v1/mev names an account as a sandwich attacker. A sandwich front-runs and
-- back-runs in OPPOSITE directions; a same-direction bracket is structurally
-- impossible. The sandwich detector only began requiring opposition on
-- 2026-08-16 (#81, which measured ~196 of 200 published candidates as
-- same-direction), and the oracle_sandwich detector on 2026-09-25. Neither
-- change touched rows already written, and the worker only rescans a
-- 30-minute window, so those rows are never revisited. The worker's 90-day
-- pruner, which would eventually have removed them, is removed alongside
-- this migration (#1168), so without this they would be served forever.
--
-- WHAT THIS DELETES
--
-- Every sandwich / oracle_sandwich row whose detail.legs fails the rule the
-- detectors now apply (internal/aggregate/mev/sandwich.go,
-- oppositeDirection / oppositeOnAsset / takerBaseIsReceived):
--
--   * direction per leg is which asset the taker RECEIVED. sdex puts the
--     taker-received asset in base; aquarius, comet, phoenix, soroswap and
--     sushiswap_v3 put the taker-SOLD asset in base. Any other source has no
--     known convention and cannot prove a direction.
--   * sandwich: the two 'bracket' legs must disagree on whether the taker
--     received the pair's LOW asset (byte order, hence COLLATE "C"; the XLM
--     SAC normalised to 'native' as normAsset does).
--   * oracle_sandwich: the 'before' and 'after' legs must disagree on
--     whether the taker received detail.asset.
--
-- A row that is not exactly two such legs with both directions known and
-- opposite is deleted. The check is date-independent: a row the current
-- detectors wrote passes it by construction (an integration test feeds real
-- detector output through this statement), so it touches only rows written
-- before each detector's fix.
--
-- Runtime: one pass over the sandwich / oracle_sandwich rows of mev_events,
-- a plain uncompressed table. Seconds. Rule-9 safe: no schema change; the
-- previous binary reads the surviving rows exactly as before.

BEGIN;

WITH leg AS (
    SELECT e.event_id,
           e.kind,
           l ->> 'role' AS role,
           CASE l ->> 'source'
               WHEN 'sdex' THEN true
               WHEN 'aquarius' THEN false
               WHEN 'comet' THEN false
               WHEN 'phoenix' THEN false
               WHEN 'soroswap' THEN false
               WHEN 'sushiswap_v3' THEN false
           END AS base_received,
           (CASE WHEN l ->> 'base' = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'
                 THEN 'native' ELSE l ->> 'base' END) COLLATE "C" AS base,
           (CASE WHEN l ->> 'quote' = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'
                 THEN 'native' ELSE l ->> 'quote' END) COLLATE "C" AS quote,
           (CASE WHEN e.detail ->> 'asset' = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA'
                 THEN 'native' ELSE e.detail ->> 'asset' END) COLLATE "C" AS asset
      FROM mev_events e
     CROSS JOIN LATERAL jsonb_array_elements(
               CASE WHEN jsonb_typeof(e.detail -> 'legs') = 'array'
                    THEN e.detail -> 'legs' ELSE '[]'::jsonb END) AS l
     WHERE e.kind IN ('sandwich', 'oracle_sandwich')
),
direction AS (
    SELECT event_id,
           CASE
               WHEN kind = 'sandwich' THEN base_received = (base <= quote)
               WHEN asset = base THEN base_received
               WHEN asset = quote THEN NOT base_received
           END AS received
      FROM leg
     WHERE (kind = 'sandwich' AND role = 'bracket')
        OR (kind = 'oracle_sandwich' AND role IN ('before', 'after'))
),
proven AS (
    SELECT event_id
      FROM direction
     GROUP BY event_id
    HAVING count(*) = 2
       AND count(received) = 2
       AND bool_or(received)
       AND NOT bool_and(received)
)
DELETE FROM mev_events m
 WHERE m.kind IN ('sandwich', 'oracle_sandwich')
   AND NOT EXISTS (SELECT 1 FROM proven p WHERE p.event_id = m.event_id);

COMMIT;
