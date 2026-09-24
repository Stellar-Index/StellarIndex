-- 0072 down — restore the pre-alignment router name and notes.
--
-- The up appends its rename sentence to `notes`; it is stripped here so
-- a down/up cycle does not append it again (GH #1172). The strip is
-- suffix-only, so a note edited after 0072 keeps any later text.
--
-- Note: any `trades.routed_via` values written while 0072 was live
-- carry 'soroswap-router' and are NOT rewritten here — the tag is
-- attribution metadata, and the up-migration can simply be re-applied.

BEGIN;

UPDATE routers
   SET name  = 'soroswap-router-v1',
       notes = CASE
                   WHEN right(notes, length(s.sentence)) = s.sentence
                   THEN NULLIF(left(notes, length(notes) - length(s.sentence)), '')
                   ELSE notes
               END
  FROM (SELECT ' Renamed from soroswap-router-v1 by migration 0072 '
               || '(routed_via alignment).' AS sentence) s
 WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'
   AND name = 'soroswap-router';

COMMIT;
