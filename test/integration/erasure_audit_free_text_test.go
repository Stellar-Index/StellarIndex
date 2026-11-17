//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// After an erasure no kept audit row may carry staff free text naming the customer (`reason`) or the erased person's
// own address as a staff actor (`actor_email`); the non-identifying keys stay.
func TestAccountErasureStripsReasonAndActorEmail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb, _ := startRedis(t, ctx)
	x := seedErasureAccount(t, ctx, db, rdb, "rita", "f.example", "5")

	const reason = "onboarding Fakeco, contact jane@f.example"
	var mintRow, lookupRow uuid.UUID
	mustScan(t, ctx, db, &mintRow, `INSERT INTO audit_log (account_id, actor_kind, action, metadata)
		VALUES ($1, 'staff', 'key.mint', jsonb_build_object('reason', $2::text, 'target_identifier', 'acct:' || $3::text,
		        'scopes', '["read"]'::jsonb)) RETURNING id`, x.accountID, reason, x.slug)
	mustScan(t, ctx, db, &lookupRow, `INSERT INTO audit_log (actor_kind, actor_user_id, action, metadata)
		VALUES ('staff', $1, 'staff.customer.lookup', jsonb_build_object('actor_email', 'rita@f.example',
		        'before', jsonb_build_object('reason', $2::text), 'result_count', 1)) RETURNING id`, x.ownerID, reason)

	eraser := &accounterasure.Eraser{Store: postgresstore.NewAccountStore(postgresstore.New(db)), Redis: rdb}
	if _, err := eraser.Erase(ctx, x.accountID, platform.ActorUser); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	for row, kept := range map[uuid.UUID]string{mintRow: "scopes", lookupRow: "result_count"} {
		var raw string
		mustScan(t, ctx, db, &raw, `SELECT metadata::text FROM audit_log WHERE id = $1`, row)
		var meta map[string]any
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatalf("metadata %q: %v", raw, err)
		}
		for _, k := range []string{"reason", "actor_email"} {
			if _, ok := meta[k]; ok {
				t.Errorf("row %s kept %q after erasure: %s", row, k, raw)
			}
		}
		if before, ok := meta["before"].(map[string]any); ok && before["reason"] != nil {
			t.Errorf("row %s kept before.reason after erasure: %s", row, raw)
		}
		if _, ok := meta[kept]; !ok {
			t.Errorf("row %s lost the non-identifying key %q: %s", row, kept, raw)
		}
	}
}
