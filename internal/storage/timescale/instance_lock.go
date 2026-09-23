package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Instance locks make "one indexer, one aggregator per database"
// (ha-plan §3.7/§3.8) something Postgres enforces rather than a deployment
// convention: a second process would race the cursors and double-write
// every sink. Each is a session-level advisory lock on a dedicated
// connection, so Postgres drops it with the session and a crashed holder
// never wedges its successor.
const (
	IndexerInstanceLockName    = "instance:stellarindex-indexer"
	AggregatorInstanceLockName = "instance:stellarindex-aggregator"

	// InstanceLockVerifyInterval bounds how long two instances can overlap
	// after the holder's session dies and another process takes the lock.
	InstanceLockVerifyInterval = 30 * time.Second
	instanceLockVerifyTimeout  = 10 * time.Second

	instanceLockTry    = `SELECT pg_try_advisory_lock(hashtext($1::text))`
	instanceLockUnlock = `SELECT pg_advisory_unlock(hashtext($1::text))`
	instanceLockPing   = `SELECT 1`
)

// ErrInstanceLockHeld means another session holds the instance lock:
// another process of the same kind is running against this database.
var ErrInstanceLockHeld = errors.New("timescale: instance lock is held by another session")

// InstanceLock is a held instance lock. See [Store.HoldInstanceLock].
type InstanceLock struct {
	db     *sql.DB
	name   string
	onLost func()
	logger *slog.Logger

	mu   sync.Mutex
	conn *sql.Conn // nil while the session is gone and the lock not yet retaken
	lost error

	stopWatch context.CancelFunc
	watchDone chan struct{}
}

// HoldInstanceLock takes the named instance lock without waiting — a held
// lock is [ErrInstanceLockHeld] — and re-verifies it every
// [InstanceLockVerifyInterval] until ctx ends or [InstanceLock.Release].
// If the session dies, the lock is retaken on a new one; if another
// session got there first, onLost is called once and [InstanceLock.Lost]
// reports it.
func (s *Store) HoldInstanceLock(ctx context.Context, name string, onLost func(), logger *slog.Logger) (*InstanceLock, error) {
	l := &InstanceLock{db: s.db, name: name, onLost: onLost, logger: logger}
	conn, err := l.take(ctx)
	if err != nil {
		return nil, err
	}
	l.conn = conn
	l.startWatch(ctx, InstanceLockVerifyInterval)
	return l, nil
}

func (l *InstanceLock) startWatch(ctx context.Context, every time.Duration) {
	watchCtx, stop := context.WithCancel(ctx)
	l.stopWatch, l.watchDone = stop, make(chan struct{})
	go l.watch(watchCtx, every)
}

func (l *InstanceLock) take(ctx context.Context) (*sql.Conn, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("timescale: dedicated connection for instance lock %q: %w", l.name, err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, instanceLockTry, l.name).Scan(&got); err != nil {
		// The try may have been granted server-side before the error, so
		// the session must not go back to the pool still holding it.
		discardConn(conn)
		return nil, fmt.Errorf("timescale: pg_try_advisory_lock(hashtext('%s')): %w", l.name, err)
	}
	if !got {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %q", ErrInstanceLockHeld, l.name)
	}
	return conn, nil
}

// verify confirms the holding session is alive, retaking the lock on a
// new session when it is not.
func (l *InstanceLock) verify(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		pingCtx, cancel := context.WithTimeout(ctx, instanceLockVerifyTimeout)
		var one int
		err := l.conn.QueryRowContext(pingCtx, instanceLockPing).Scan(&one)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// Shutting down: keep the session so the lock outlives the drain.
			return ctx.Err()
		}
		discardConn(l.conn)
		l.conn = nil
		l.logger.Warn("instance lock session lost; retaking", "lock", l.name, "err", err)
	}
	conn, err := l.take(ctx)
	if err != nil {
		return err
	}
	l.conn = conn
	return nil
}

func (l *InstanceLock) watch(ctx context.Context, every time.Duration) {
	defer close(l.watchDone)
	defer worker.Recover(l.logger, "instance-lock-watch:"+l.name)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := l.verify(ctx)
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.Is(err, ErrInstanceLockHeld):
			l.mu.Lock()
			l.lost = err
			l.mu.Unlock()
			l.logger.Error("instance lock taken by another session while ours was lost; shutting down", "lock", l.name, "err", err)
			l.onLost()
			return
		default:
			l.logger.Warn("instance lock verify failed; retrying", "lock", l.name, "every", every, "err", err)
		}
	}
}

// Lost returns the error that ended the hold when another session took
// the lock, else nil. Nil-safe: a dry-run holds no lock.
func (l *InstanceLock) Lost() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// Release stops verification, unlocks and closes the holding session.
// Nil-safe.
func (l *InstanceLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.stopWatch()
	<-l.watchDone
	l.mu.Lock()
	defer l.mu.Unlock()
	conn := l.conn
	l.conn = nil
	if conn == nil {
		return nil
	}
	defer discardConn(conn)
	var released bool
	if err := conn.QueryRowContext(ctx, instanceLockUnlock, l.name).Scan(&released); err != nil {
		return fmt.Errorf("timescale: pg_advisory_unlock(hashtext('%s')): %w", l.name, err)
	}
	if !released {
		return fmt.Errorf("timescale: pg_advisory_unlock(hashtext('%s')) returned false: the lock was not held by this session", l.name)
	}
	return nil
}

// discardConn closes conn's session rather than returning it to the pool,
// so no pooled session can keep an advisory lock alive.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
