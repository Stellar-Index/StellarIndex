package v1

import (
	"os"
	"strings"
	"testing"
)

// TestIssuersCacheComment_NoDanglingConfigReference pins Q137: the doc
// comment on NewCachedIssuersReader must not point readers at a
// configs/example.toml issuers_cache_ttl knob that doesn't exist — there is
// no [api] issuers_cache_ttl field anywhere in internal/config/config.go or
// configs/example.toml, so a reader following that comment to "pin it
// shorter" finds nothing to pin.
func TestIssuersCacheComment_NoDanglingConfigReference(t *testing.T) {
	src, err := os.ReadFile("issuers_cache.go")
	if err != nil {
		t.Fatalf("read issuers_cache.go: %v", err)
	}
	if strings.Contains(string(src), "issuers_cache_ttl") {
		t.Fatalf("issuers_cache.go still references issuers_cache_ttl, but " +
			"neither internal/config/config.go nor configs/example.toml " +
			"define that field — the comment must not cite a config knob " +
			"that doesn't exist")
	}

	cfg, err := os.ReadFile("../../config/config.go")
	if err != nil {
		t.Fatalf("read internal/config/config.go: %v", err)
	}
	if strings.Contains(string(cfg), "issuers_cache_ttl") ||
		strings.Contains(string(cfg), "IssuersCacheTTL") {
		t.Fatalf("internal/config/config.go now defines an issuers cache " +
			"TTL field — update issuers_cache.go's NewCachedIssuersReader " +
			"comment to describe it instead of asserting there is none")
	}

	example, err := os.ReadFile("../../../configs/example.toml")
	if err != nil {
		t.Fatalf("read configs/example.toml: %v", err)
	}
	if strings.Contains(string(example), "issuers_cache_ttl") {
		t.Fatalf("configs/example.toml now defines issuers_cache_ttl — " +
			"update issuers_cache.go's NewCachedIssuersReader comment to " +
			"describe it instead of asserting there is none")
	}
}
