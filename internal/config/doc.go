// Package config is the one TOML configuration shape every binary reads
// ([Config]; each binary uses the substructs it needs), its loader and
// the generator for docs/reference/config/README.md.
//
// Every field carries `doc:"…"` and `default:` tags; lint-docs.sh fails
// a field with no doc tag. After adding a field, regenerate the reference
// with `go run ./cmd/stellarindex-ops docs-config` in the same PR.
//
// [Default] always passes [Config.Validate]; a zero Config{} does not,
// so load through [Load] or [LoadWithEnv]. Secrets never live in the
// file: a field names its source, e.g. "env:STELLARINDEX_PG_PASSWORD".
package config
