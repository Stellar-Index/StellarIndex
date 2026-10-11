// Package config is the one TOML configuration shape every binary reads ([Config]), its loader, and the generator
// for docs/reference/config/README.md from `doc:`/`default:` tags (regenerate with docs-config in the same PR).
// [Default] passes [Config.Validate], a zero Config{} does not; secrets never live in the file, fields name their source.
package config
