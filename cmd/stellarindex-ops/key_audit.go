package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// opsKeyReasonMaxLen bounds the -reason stored in one audit_log row.
const opsKeyReasonMaxLen = 512

// keyAuditSink is the audit_log writer mint-key and upgrade-key record
// through (postgresstore.AuditStore in production).
type keyAuditSink interface {
	Append(ctx context.Context, e platform.AuditEntry) error
}

// resolveOpsActor names who ran a privileged key command: -actor, else the
// OS login. An audit row with no actor is refused.
func resolveOpsActor(flagActor string) (string, error) {
	if a := strings.TrimSpace(flagActor); a != "" {
		return a, nil
	}
	u, err := user.Current()
	if err != nil || strings.TrimSpace(u.Username) == "" {
		return "", errors.New("-actor is required (the OS user could not be resolved)")
	}
	return u.Username, nil
}

// validateOpsKeyReason requires the -reason recorded with every CLI key
// action, the counterpart of the admin API's X-Reason header.
func validateOpsKeyReason(reason string) error {
	r := strings.TrimSpace(reason)
	if r == "" {
		return errors.New("-reason is required: it is recorded in audit_log with the actor")
	}
	if len(r) > opsKeyReasonMaxLen {
		return fmt.Errorf("-reason must be <= %d characters (got %d)", opsKeyReasonMaxLen, len(r))
	}
	return nil
}

// openKeyAudit opens the audit_log writer. The key commands refuse to run
// without it: their stderr is the only other record and does not outlive
// the terminal.
func openKeyAudit(ctx context.Context, dsn string) (keyAuditSink, func(), error) {
	if dsn == "" {
		return nil, nil, errors.New("storage.postgres_dsn is empty — key commands record to audit_log and refuse to run without it")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("audit_log postgres open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("audit_log postgres ping: %w", err)
	}
	return postgresstore.NewAuditStore(postgresstore.New(db)), func() { _ = db.Close() }, nil
}

// appendKeyAudit writes one staff-actor audit_log row for a CLI key action.
func appendKeyAudit(ctx context.Context, sink keyAuditSink, action, command, keyID, actor, reason string, detail map[string]any) error {
	meta := map[string]any{"actor": actor, "reason": strings.TrimSpace(reason), "via": "stellarindex-ops " + command}
	for k, v := range detail {
		meta[k] = v
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("audit metadata: %w", err)
	}
	return sink.Append(ctx, platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     action,
		TargetKind: "api_key",
		TargetID:   keyID,
		Metadata:   body,
		UserAgent:  "stellarindex-ops " + command,
		Timestamp:  time.Now().UTC(),
	})
}
