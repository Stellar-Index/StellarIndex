package postgresstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Store is the shared *sql.DB handle every concrete store wraps.
// Constructed by Open and threaded through {Account,User,Token,
// APIKey,Audit,Billing,Webhook}Store.
//
// Safe for concurrent use; *sql.DB is internally pooled.
type Store struct {
	db *sql.DB
}

// New wraps an existing *sql.DB. Used by tests that already
// have a container handle and by production wiring that opens
// the pool once and shares it across stores.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the underlying handle for the rare case a caller
// needs to run a transaction across multiple stores. The Account
// + User stores accept a non-nil *sql.Tx via the Tx-suffixed
// methods (added per-need; not all impls have one yet).
func (s *Store) DB() *sql.DB { return s.db }

// accountLockNamespace is the classid of a per-account quota lock. Each
// value must stay unique: it is what keeps two quotas from sharing a lock.
type accountLockNamespace int32

const (
	lockNamespaceAPIKey accountLockNamespace = iota + 1
	lockNamespaceWebhook
	lockNamespacePriceAlert
)

// accountAdvisoryLockSQL takes the two-int4 form: namespaces differ in
// classid, so a hashtext collision on the account can never cross them.
const accountAdvisoryLockSQL = `SELECT pg_advisory_xact_lock($1::int4, hashtext($2::text))`

// lockAccount serialises tx against every other transaction holding the
// same (namespace, account) lock until tx ends.
func lockAccount(ctx context.Context, tx *sql.Tx, ns accountLockNamespace, accountID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, accountAdvisoryLockSQL, int32(ns), accountID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	return nil
}
