// Package accounterasure erases a platform account: the one writer both
// DELETE /v1/dashboard/account and `stellarindex-ops account-erase` call
// (GH #809).
//
// Sequence, each step safe to repeat:
//
//  1. Plan: read the account, its members and Postgres keys; refuse one
//     with billing state or a staff member.
//  2. Collect the Redis self-service keys the account's identifier holds.
//  3. One Postgres transaction closes the account, scrubs its audit rows,
//     renames its usage rows to one fresh erased:<uuid> subject, deletes
//     everything else and records a hashed slug tombstone plus an
//     account.erase audit row. A failure rolls all of it back.
//  4. After the commit: delete the Redis key records, cache rows and
//     usage counters, then do it again and re-run the usage rename with
//     the same uuid. The second pass catches a key mint or a rollup sweep
//     that was in flight across the commit.
//
// A record that slips past both passes cannot authenticate: its account
// is gone, and the tombstone keeps any new account off the slug.
package accounterasure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// afterCommitTimeout bounds the post-commit cleanup, which runs detached
// from the caller's context.
const afterCommitTimeout = 30 * time.Second

// ErrBlocked wraps [postgresstore.ErrErasureBlocked].
var ErrBlocked = postgresstore.ErrErasureBlocked

// Store is the Postgres half; *postgresstore.AccountStore satisfies it.
type Store interface {
	PlanErasure(ctx context.Context, id uuid.UUID) (postgresstore.ErasurePlan, error)
	EraseAccount(ctx context.Context, req postgresstore.ErasureRequest) (postgresstore.ErasureCounts, error)
	RenameUsageSubjects(ctx context.Context, subjects []string, erased string) (int64, error)
	SlugErased(ctx context.Context, slug string) (bool, error)
}

// Eraser runs an erasure. Redis may be nil on a Redis-less deployment,
// which holds no Redis state to erase.
type Eraser struct {
	Store  Store
	Redis  redis.Cmdable
	Logger *slog.Logger

	// betweenPasses, when set, runs between the two post-commit Redis
	// passes. Test seam for the in-flight-mint race.
	betweenPasses func()
}

// Report is what one erasure did.
type Report struct {
	// AlreadyErased is true when the account no longer existed; nothing
	// was changed.
	AlreadyErased bool
	Plan          postgresstore.ErasurePlan
	Counts        postgresstore.ErasureCounts
	// ErasedSubject is the erased:<uuid> usage subject the account's usage
	// rows now carry.
	ErasedSubject string
	RedisKeys     int
	RedisDeleted  int64
	// LateUsageRows is how many usage_daily rows the post-commit rename
	// found under the old subjects.
	LateUsageRows int64
}

// Erase erases the account. A second call for the same account reports
// AlreadyErased. An error before the commit leaves the account untouched;
// an error after it leaves Redis state that FinishBySlug removes.
func (e *Eraser) Erase(ctx context.Context, id uuid.UUID, actor platform.ActorKind) (Report, error) {
	plan, err := e.Store.PlanErasure(ctx, id)
	if errors.Is(err, platform.ErrNotFound) {
		return Report{AlreadyErased: true}, nil
	}
	if err != nil {
		return Report{}, err
	}
	rep := Report{Plan: plan}

	identifier := auth.AccountIdentifier(plan.Slug)
	var extra []string
	if e.Redis != nil {
		recs, err := auth.NewRedisAPIKeyStore(e.Redis).ListKeysForIdentifier(ctx, identifier)
		if err != nil {
			return Report{}, fmt.Errorf("account erasure: list redis keys: %w", err)
		}
		for _, r := range recs {
			extra = append(extra, r.KeyID)
		}
	}
	erased := "erased:" + uuid.NewString()
	rep.ErasedSubject = erased
	rep.Counts, err = e.Store.EraseAccount(ctx, postgresstore.ErasureRequest{
		Plan: plan, ExtraKeyIDs: extra, ErasedSubject: erased, Actor: actor,
	})
	if errors.Is(err, platform.ErrNotFound) {
		return Report{AlreadyErased: true}, nil
	}
	if err != nil {
		return Report{}, err
	}

	keyIDs := append(append([]string{}, plan.KeyIDs...), extra...)
	// The commit is durable; a caller that hangs up now must not strand
	// the Redis cleanup.
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterCommitTimeout)
	defer cancel()
	if err := e.afterCommit(postCtx, plan.Slug, keyIDs, plan.KeyHashes, erased, &rep); err != nil {
		return rep, err
	}
	e.logger().Info("account erased",
		"account_id", plan.AccountID, "actor", actor,
		"users", rep.Counts.Users, "api_keys", rep.Counts.APIKeys, "redis_keys", rep.RedisKeys,
		"audit_rows_scrubbed", rep.Counts.AuditRowsScrubbed, "late_usage_rows", rep.LateUsageRows)
	return rep, nil
}

// FinishBySlug re-runs the post-commit cleanup for an account whose
// Postgres erasure committed but whose Redis cleanup did not finish. It
// refuses a slug that was never erased, so an operator typo cannot wipe
// a live account's keys.
func (e *Eraser) FinishBySlug(ctx context.Context, slug string) (Report, error) {
	ok, err := e.Store.SlugErased(ctx, slug)
	if err != nil {
		return Report{}, err
	}
	if !ok {
		return Report{}, fmt.Errorf("account erasure: slug %q was never erased", slug)
	}
	rep := Report{ErasedSubject: "erased:" + uuid.NewString()}
	var keyIDs []string
	if e.Redis != nil {
		recs, err := auth.NewRedisAPIKeyStore(e.Redis).ListKeysForIdentifier(ctx, auth.AccountIdentifier(slug))
		if err != nil {
			return Report{}, fmt.Errorf("account erasure: list redis keys: %w", err)
		}
		for _, r := range recs {
			keyIDs = append(keyIDs, r.KeyID)
		}
	}
	return rep, e.afterCommit(ctx, slug, keyIDs, nil, rep.ErasedSubject, &rep)
}

func (e *Eraser) afterCommit(
	ctx context.Context, slug string, keyIDs, keyHashes []string, erased string, rep *Report,
) error {
	subjects := postgresstore.UsageSubjects(slug, keyIDs...)
	if e.Redis != nil {
		for pass := 0; pass < 2; pass++ {
			if pass == 1 && e.betweenPasses != nil {
				e.betweenPasses()
			}
			n, del, err := e.deleteRedis(ctx, slug, subjects, keyHashes)
			if err != nil {
				return fmt.Errorf("account erasure: redis pass %d: %w", pass+1, err)
			}
			rep.RedisKeys += n
			rep.RedisDeleted += del
		}
	}
	late, err := e.Store.RenameUsageSubjects(ctx, subjects, erased)
	if err != nil {
		return fmt.Errorf("account erasure: post-commit usage rename: %w", err)
	}
	rep.LateUsageRows = late
	return nil
}

// deleteRedis removes the identifier's key records and index entries, the
// Postgres keys' validator records and cache rows, and every usage
// counter. Rate-limit and touch-debounce keys expire within minutes and
// are keyed by the same identifiers, so they are left to their TTL.
func (e *Eraser) deleteRedis(
	ctx context.Context, slug string, subjects, keyHashes []string,
) (keys int, deleted int64, err error) {
	ids, err := auth.NewRedisAPIKeyStore(e.Redis).DeleteKeysForIdentifier(ctx, auth.AccountIdentifier(slug))
	if err != nil {
		return 0, 0, err
	}
	keys = len(ids)
	if len(keyHashes) > 0 {
		names := make([]string, 0, 2*len(keyHashes))
		for _, h := range keyHashes {
			names = append(names, cachekeys.APIKey(h).String(), cachekeys.APIKeyCache(h).String())
		}
		n, err := e.Redis.Del(ctx, names...).Result()
		if err != nil {
			return keys, 0, fmt.Errorf("delete key records: %w", err)
		}
		deleted += n
	}
	counter := usage.New(e.Redis)
	for _, s := range subjects {
		n, err := counter.DeleteSubject(ctx, s)
		if err != nil {
			return keys, deleted, err
		}
		deleted += n
	}
	return keys, deleted, nil
}

func (e *Eraser) logger() *slog.Logger {
	if e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
