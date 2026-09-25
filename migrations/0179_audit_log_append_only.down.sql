-- 0179 down — drop audit_log's append-only triggers. The rows are
-- untouched; the table simply returns to comment-only append-only.

BEGIN;

DROP TRIGGER IF EXISTS audit_log_no_truncate ON audit_log;
DROP TRIGGER IF EXISTS audit_log_append_only ON audit_log;
DROP FUNCTION IF EXISTS audit_log_append_only();

COMMIT;
