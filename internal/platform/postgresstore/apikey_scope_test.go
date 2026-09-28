// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package postgresstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// recordingDriver captures every Exec so a test can assert the SQL a store
// method sends without a Postgres. Each Exec reports rowsAffected rows.
type recordingDriver struct {
	mu           sync.Mutex
	execs        []recordedExec
	rowsAffected int64
}

type recordedExec struct {
	query string
	args  []driver.NamedValue
}

func (d *recordingDriver) Connect(context.Context) (driver.Conn, error) { return recordingConn{d}, nil }
func (d *recordingDriver) Driver() driver.Driver                        { return nil }

type recordingConn struct{ d *recordingDriver }

func (recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("recordingConn: Prepare unsupported")
}
func (recordingConn) Close() error                             { return nil }
func (recordingConn) Begin() (driver.Tx, error)                { return recordingTx{}, nil }
func (recordingConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (c recordingConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.execs = append(c.d.execs, recordedExec{query: q, args: args})
	return driver.RowsAffected(c.d.rowsAffected), nil
}

type recordingTx struct{}

func (recordingTx) Commit() error   { return nil }
func (recordingTx) Rollback() error { return nil }

func newRecordingStore(t *testing.T) (*Store, *recordingDriver) {
	t.Helper()
	d := &recordingDriver{rowsAffected: 1}
	db := sql.OpenDB(d)
	t.Cleanup(func() { _ = db.Close() })
	return New(db), d
}

var accountPredicate = regexp.MustCompile(`WHERE\s+id\s*=\s*\$1\s+AND\s+account_id\s*=\s*\$(\d+)\s*$`)

// assertAccountScoped fails unless the statement's WHERE clause pins
// account_id to a bound parameter that carries want.
func assertAccountScoped(t *testing.T, e recordedExec, want uuid.UUID) {
	t.Helper()
	m := accountPredicate.FindStringSubmatch(e.query)
	if m == nil {
		t.Fatalf("statement is not scoped by account_id:\n%s", e.query)
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > len(e.args) {
		t.Fatalf("account_id bound to $%d but only %d args were sent", n, len(e.args))
	}
	if got := fmt.Sprint(e.args[n-1].Value); got != want.String() {
		t.Fatalf("account_id arg $%d = %q, want %q", n, got, want)
	}
}

// Invariant: a key's editable fields and revocation are writable only by a
// caller acting for the key's owning account; the account is a SQL
// predicate, so a caller that skips its own ownership check still cannot
// reach another account's key.
func TestAPIKeyStore_UpdateAndRevokeAreAccountScoped(t *testing.T) {
	ctx := context.Background()
	owner := uuid.New()

	t.Run("update", func(t *testing.T) {
		s, d := newRecordingStore(t)
		k := platform.APIKey{ID: "kid_abc", AccountID: uuid.New(), Name: "n", RateLimitPerMin: 10}
		if err := NewAPIKeyStore(s).Update(ctx, owner, k); err != nil {
			t.Fatal(err)
		}
		// The scope is the explicit accountID, never the record's own field.
		assertAccountScoped(t, d.execs[0], owner)
	})

	t.Run("revoke", func(t *testing.T) {
		s, d := newRecordingStore(t)
		if err := NewAPIKeyStore(s).Revoke(ctx, owner, "kid_abc", uuid.Nil, "r"); err != nil {
			t.Fatal(err)
		}
		assertAccountScoped(t, d.execs[0], owner)
	})

	t.Run("no row is not found", func(t *testing.T) {
		s, d := newRecordingStore(t)
		d.rowsAffected = 0
		st := NewAPIKeyStore(s)
		if err := st.Update(ctx, owner, platform.APIKey{ID: "kid_abc"}); err != platform.ErrNotFound {
			t.Fatalf("Update err = %v, want ErrNotFound", err)
		}
		if err := st.Revoke(ctx, owner, "kid_abc", uuid.Nil, ""); err != platform.ErrNotFound {
			t.Fatalf("Revoke err = %v, want ErrNotFound", err)
		}
	})
}

// Two quotas must never share a lock, whatever hashtext does to the
// account: the namespace travels as its own int4 key, not inside the hash.
func TestLockAccount_NamespaceIsSeparateKey(t *testing.T) {
	s, d := newRecordingStore(t)
	acct := uuid.New()
	namespaces := []accountLockNamespace{lockNamespaceAPIKey, lockNamespaceWebhook, lockNamespacePriceAlert}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := lockAccount(context.Background(), tx, ns, acct); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback()
		key := fmt.Sprint(d.execs[len(d.execs)-1].args[0].Value)
		if seen[key] {
			t.Fatalf("namespace %d reuses lock classid %s", ns, key)
		}
		seen[key] = true
	}
	twoKey := regexp.MustCompile(`pg_advisory_xact_lock\(\$1::int4,\s*hashtext\(\$2::text\)\)`)
	for _, e := range d.execs {
		if !twoKey.MatchString(e.query) || len(e.args) != 2 {
			t.Fatalf("lock must be the two-key form, got %q with %d args", e.query, len(e.args))
		}
	}
}
