package config_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// A config-validation failure is fatal at boot, so its message lands on
// stderr -> journald -> promtail -> Loki, where anyone with Grafana read
// access can read it for the 720h retention. That makes a validation
// branch that formats the offending VALUE into its message a credential
// leak whenever the value can be a credential — and the branches below
// are exactly the ones that fire on the paste-the-secret-where-the-name-
// goes mistake they exist to catch.
//
// Every case here is a real operator misconfiguration, not a contrived
// one:
//
//   - a managed-Redis URI (Upstash / Redis Cloud / Railway / Heroku all
//     issue rediss://default:<password>@host:port, never the bare
//     host:port storage.redis_addr wants) pasted into redis_addr or a
//     sentinel entry. net.SplitHostPort rejects it with an *net.AddrError
//     that embeds the whole address, so a `: %w` wrap re-leaks it even
//     when the %q is redacted.
//   - a real DSN with an inline password under a scheme we don't accept.
//   - an S3 secret key pasted where the env-var NAME belongs.
func TestValidate_FatalErrorsDoNotEchoCredentials(t *testing.T) {
	const secret = "hunter2-NOT-IN-THE-BOOT-LOG"

	cases := map[string]func(*config.Config){
		"redis_addr as a managed-Redis URI": func(c *config.Config) {
			c.Storage.RedisAddr = "rediss://default:" + secret + "@myredis.example.com:6380"
		},
		"redis_sentinel_addrs entry as a managed-Redis URI": func(c *config.Config) {
			c.Storage.RedisMasterName = "mymaster"
			c.Storage.RedisSentinelAddrs = []string{
				"127.0.0.1:26379",
				"rediss://default:" + secret + "@sentinel.example.com:26380",
			}
		},
		"postgres_dsn under a wrong scheme, password inline": func(c *config.Config) {
			c.Storage.PostgresDSN = "mysql://stellarindex:" + secret + "@db.example.com:3306/stellarindex"
		},
		"s3_access_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3AccessKeyEnv = secret
		},
		"s3_secret_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3SecretKeyEnv = secret
		},
		"s3_cold_access_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = secret
			c.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"
		},
		"s3_cold_secret_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
			c.Storage.S3ColdSecretKeyEnv = secret
		},
		"half a cold-tier pair, the set half holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = secret
			c.Storage.S3ColdSecretKeyEnv = ""
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error — this case is a misconfiguration")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("fatal boot error echoes the credential:\n  %v", err)
			}
		})
	}
}

// TestValidate_RedactedErrorsStillIdentifyTheSetting is the other half
// of the contract: withholding the value must not cost the operator the
// ability to tell WHICH setting is wrong (and, for the all-or-nothing
// cold-tier pair, which half of it they left out).
func TestValidate_RedactedErrorsStillIdentifyTheSetting(t *testing.T) {
	cases := map[string]struct {
		mutate func(*config.Config)
		want   []string
	}{
		"redis_addr": {
			func(c *config.Config) { c.Storage.RedisAddr = "127.0.0.1" },
			[]string{"storage.redis_addr", "host:port", "missing port"},
		},
		"redis_sentinel_addrs names the index": {
			func(c *config.Config) {
				c.Storage.RedisMasterName = "mymaster"
				c.Storage.RedisSentinelAddrs = []string{"127.0.0.1:26379", "127.0.0.1"}
			},
			[]string{"storage.redis_sentinel_addrs[1]", "host:port"},
		},
		"postgres_dsn keeps the scheme, which is the mistake": {
			func(c *config.Config) { c.Storage.PostgresDSN = "mysql://u:p@h/db" },
			[]string{"storage.postgres_dsn", "mysql://<redacted>", "postgres://"},
		},
		"s3_access_key_env": {
			func(c *config.Config) { c.Storage.S3AccessKeyEnv = "not-a-name" },
			[]string{"storage.s3_access_key_env", "UPPER_SNAKE_CASE"},
		},
		"cold pair says which half was set": {
			func(c *config.Config) {
				c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
				c.Storage.S3ColdSecretKeyEnv = ""
			},
			[]string{
				"storage.s3_cold_access_key_env (set)",
				"storage.s3_cold_secret_key_env (empty)",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			tc.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lost the %q diagnostic:\n  %v", want, err)
				}
			}
		})
	}
}

// TestValidate_EnvVarNameSwapStillEchoesTheName pins the deliberate
// exception to the no-echo rule, so a later tightening pass does not
// redact a message whose entire diagnostic is the value. The
// redis_password_env / clickhouse_serving_password_env branches fire
// ONLY when the value matched `^STELLARINDEX_[A-Z0-9_]+$` — i.e. it is
// provably one of this project's own env-var NAMES and provably not the
// password the field is supposed to hold.
func TestValidate_EnvVarNameSwapStillEchoesTheName(t *testing.T) {
	for field, mutate := range map[string]func(*config.Config){
		"storage.redis_password_env": func(c *config.Config) {
			c.Storage.RedisPassword = "STELLARINDEX_REDIS_PASSWORD"
		},
		"storage.clickhouse_serving_password_env": func(c *config.Config) {
			c.Storage.ClickHouseServingPassword = "STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"
		},
	} {
		t.Run(field, func(t *testing.T) {
			c := config.Default()
			mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("%s: expected the swapped-convention error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("%s: error does not name the setting:\n  %v", field, err)
			}
			if !strings.Contains(err.Error(), "STELLARINDEX_") {
				t.Errorf("%s: error withheld the env-var NAME, which is the whole diagnostic here:\n  %v",
					field, err)
			}
		})
	}
}
