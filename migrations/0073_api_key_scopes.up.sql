-- 0073: capability scopes on api_keys.
--
-- Adds the coarse route-family scope list a key can be confined to
-- (read / account / dashboard / admin — vocabulary owned by
-- internal/platform.KnownKeyScopes). The default empty array keeps
-- every existing key on the legacy full-access posture: the
-- KeyPolicy middleware only enforces scopes when the list is
-- non-empty, so this migration is behaviour-neutral for all rows it
-- touches.

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS scopes text[] NOT NULL DEFAULT '{}';
