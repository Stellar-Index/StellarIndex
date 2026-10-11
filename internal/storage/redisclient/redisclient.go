// Package redisclient builds a [redis.UniversalClient] from [config.StorageConfig]: a
// Sentinel-aware FailoverClient when Sentinel addresses are configured (production), else a
// plain Client against RedisAddr for dev / single-node.
//
// The API and aggregator share this builder so Sentinel handling lives in one place. Tests use
// miniredis or the integration harness, so the FailoverClient branch is exercised only in
// deployment.
package redisclient

import (
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// Build returns a [redis.UniversalClient] for the API and aggregator binaries.
//
// Selection rule:
//   - cfg.RedisSentinelAddrs non-empty → FailoverClient (survives failover without a config
//     change); cfg.RedisAddr is ignored.
//   - Sentinel empty AND cfg.RedisAddr non-empty → plain Client.
//   - both empty → nil; the caller decides whether that is fatal (aggregator) or tolerable
//     (API caches fall through gracefully).
//
// The client is safe for concurrent use; close it on shutdown.
func Build(cfg config.StorageConfig) redis.UniversalClient {
	// Username is empty by default (Redis's default-user path) and set
	// to "stellarindex" (or per-component) when the operator sets
	// redis_acl_lockdown=true in the redis-sentinel ansible role.
	var c redis.UniversalClient
	switch {
	case len(cfg.RedisSentinelAddrs) > 0:
		c = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.RedisMasterName,
			SentinelAddrs: cfg.RedisSentinelAddrs,
			// Same secret authenticates both the data plane and
			// Sentinel — see ADR-0024 §Decision: requirepass +
			// masterauth + sentinel auth-pass all share the vault
			// entry.
			Username:         cfg.RedisUsername,
			Password:         cfg.RedisPassword,
			SentinelPassword: cfg.RedisPassword,
			DisableIdentity:  true,
		})
	case cfg.RedisAddr != "":
		c = redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Username: cfg.RedisUsername,
			Password: cfg.RedisPassword,
			// Redis < 7.2 rejects the CLIENT SETINFO / MAINT_NOTIFICATIONS
			// handshake; go-redis ignores that, but the error hook would
			// count it as failed commands on every new connection.
			DisableIdentity:          true,
			MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
		})
	default:
		return nil
	}
	c.AddHook(errorMetricsHook{})
	return c
}

// Mode reports which branch [Build] would take. Used by the
// startup logger so operators see "redis mode=sentinel" or
// "redis mode=single" in the boot line — matters for SEV-1
// triage when the config doesn't match the deployed topology.
func Mode(cfg config.StorageConfig) string {
	switch {
	case len(cfg.RedisSentinelAddrs) > 0:
		return "sentinel"
	case cfg.RedisAddr != "":
		return "single"
	default:
		return "disabled"
	}
}
