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

// Build returns a [redis.UniversalClient] for the API and aggregator: cfg.RedisSentinelAddrs non-empty gives a FailoverClient (RedisAddr ignored), else RedisAddr non-empty a plain Client, else nil (the caller decides whether that is fatal or tolerable). Safe for concurrent use; close on shutdown.
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
			PipelinePoolSize: -1,
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
			// One pool, as before go-redis 9.23: pipelines then honour MaxActiveConns, and the
			// dedicated pool's first dial would run its handshake under the error hook.
			PipelinePoolSize: -1,
		})
	default:
		return nil
	}
	c.AddHook(errorMetricsHook{})
	return c
}

// Mode reports which branch [Build] would take, so the boot line shows "redis mode=sentinel" or "single", which matters for triage when config does not match the deployed topology.
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
