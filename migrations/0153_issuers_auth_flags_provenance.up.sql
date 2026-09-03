-- 0153 up — record HOW an issuer's auth flags were obtained (#374).
--
-- WHY: `stellarindex-ops issuer-flags` resolves an issuer's AccountEntry auth
-- flags out of the ClickHouse lake and persists them here. Its reader only
-- ever looked at LIVE entries (`ledger_entries_current … change_type !=
-- 'removed'`), so every issuer that has MERGED ITS ACCOUNT AWAY stayed
-- unresolved for good: r1 2026-09-02, 10,238 of 59,240 issuers, and a 300-key
-- sample says 296 of them (98.7%) are merged accounts, not gaps.
--
-- Those flags ARE knowable — an account_merge leaves a `state` pre-image in
-- the very ledger that removed the account — but a value read out of a dead
-- account is NOT the issuer's current authorisation policy, and serving it as
-- though it were would be a new, quieter defect than the one it fixes. So the
-- value only becomes persistable once its provenance is persistable with it:
--
--   auth_flags_source        'live' | 'last_known_before_removal'
--   auth_flags_as_of_ledger  the ledger the reading is true as of
--
-- The API surfaces both on /v1/issuers/{g_strkey} (additive, omitted when
-- unknown) so a consumer can tell a current policy from a historical one.
--
-- BACKFILL. Every row that already carries auth_required was written by the
-- pre-0153 reader, whose query filtered `change_type != 'removed'` — i.e. it
-- could only ever have read a LIVE AccountEntry. Stamping those 'live' is
-- therefore a statement of fact about how they were obtained, not a guess.
-- auth_flags_as_of_ledger is deliberately left NULL for them: the old reader
-- did not select ledger_seq, so the as-of ledger genuinely is not known, and
-- inventing one would be worse than admitting it. A re-run of the drain fills
-- it in.
--
-- OLD-BINARY-SAFE (migrations rule 9). Two new nullable columns; the
-- previous released binary neither reads nor writes them. Both CHECKs are
-- vacuous for it: it never writes auth_flags_source at all, so it can neither
-- pick a value outside the pair nor create a 'last_known_before_removal' row
-- without an as-of ledger. Deliberately NOT added: a
-- `auth_required IS NOT NULL => auth_flags_source IS NOT NULL` constraint —
-- it reads as the natural invariant, but the old binary's
-- PersistIssuerAuthFlags sets auth_required WITHOUT touching the source
-- column, so it would fail every write of the currently-deployed drain.

BEGIN;

ALTER TABLE issuers ADD COLUMN auth_flags_source text;
ALTER TABLE issuers ADD COLUMN auth_flags_as_of_ledger integer;

COMMENT ON COLUMN issuers.auth_flags_source IS
    'How auth_required/revocable/immutable/clawback were obtained: '
    '''live'' = decoded from the account''s current AccountEntry in the lake; '
    '''last_known_before_removal'' = the account has been merged away and '
    'these are its flags as of auth_flags_as_of_ledger, NOT its current '
    'policy. NULL = flags not resolved, or resolved before 0153 and not yet '
    're-drained. Never present a last_known_before_removal reading as '
    'current (#374).';

COMMENT ON COLUMN issuers.auth_flags_as_of_ledger IS
    'Ledger the persisted auth flags are true as of: the AccountEntry''s '
    'last-modified ledger when auth_flags_source = ''live'', the ledger that '
    'merged the account away when it is ''last_known_before_removal''. NULL '
    'when unknown (pre-0153 rows, whose reader did not select it).';

ALTER TABLE issuers ADD CONSTRAINT issuers_auth_flags_source_check  -- migration-compat:ok new nullable column; the previous released binary never writes auth_flags_source, so no value it can produce is rejected
    CHECK (auth_flags_source IS NULL
           OR auth_flags_source IN ('live', 'last_known_before_removal'));

-- A historical reading without its as-of ledger is unlabelled in the way
-- that matters: "these flags are old" is only actionable with "as of when".
ALTER TABLE issuers ADD CONSTRAINT issuers_auth_flags_as_of_ledger_check  -- migration-compat:ok same: the old binary cannot write 'last_known_before_removal', so this arm is unreachable for it
    CHECK (auth_flags_source <> 'last_known_before_removal'
           OR auth_flags_as_of_ledger IS NOT NULL);

UPDATE issuers
   SET auth_flags_source = 'live'
 WHERE auth_required IS NOT NULL
   AND auth_flags_source IS NULL;

COMMIT;
