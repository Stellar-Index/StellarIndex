package dashboardwebhooks

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// webhookAuditKeysKept are the webhook audit metadata keys that say nothing about who the customer is, so an account erasure may
// leave them. Every other key the handlers write must be one audit_log_erase_metadata strips.
var webhookAuditKeysKept = map[string]bool{
	"events":      true,
	"enabled":     true,
	"url_changed": true,
}

// An erasure can only rewrite audit metadata through audit_log_erase_metadata, which removes a fixed key list. A key added to
// a webhook audit row is kept forever unless it is on that list, so each one must be classified here.
func TestWebhookAuditMetadataKeysAreClassifiedForErasure(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func webhookAuditMeta(")
	if start < 0 {
		t.Fatal("webhookAuditMeta not found in handlers.go")
	}
	literal := body[start : start+strings.Index(body[start:], "\n}\n")]
	written := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-z_]+)":`).FindAllStringSubmatch(literal, -1) {
		written[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`meta\["([a-z_]+)"\]\s*=`).FindAllStringSubmatch(body, -1) {
		written[m[1]] = true
	}
	if !written["url_host"] || !written["previous_url_host"] {
		t.Fatalf("scan found keys %v; it no longer sees the keys the handlers write", written)
	}

	stripped := erasureStrippedKeys(t)
	keys := make([]string, 0, len(written))
	for k := range written {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if webhookAuditKeysKept[k] == stripped[k] {
			t.Errorf("webhook audit key %q: kept=%v stripped=%v; list it in webhookAuditKeysKept, or add it to "+
				"audit_log_erase_metadata in a new migration, and not both", k, webhookAuditKeysKept[k], stripped[k])
		}
	}
}

// erasureStrippedKeys reads the key list from the newest migration that defines audit_log_erase_metadata.
func erasureStrippedKeys(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../../../migrations/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	list := regexp.MustCompile(`(?s)FUNCTION audit_log_erase_metadata\(m jsonb\).*?ARRAY\[(.*?)\]::text\[\]`)
	for i := len(files) - 1; i >= 0; i-- {
		sql, err := os.ReadFile(files[i])
		if err != nil {
			t.Fatal(err)
		}
		m := list.FindSubmatch(sql)
		if m == nil {
			continue
		}
		out := map[string]bool{}
		for _, k := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
			out[string(k[1])] = true
		}
		return out
	}
	t.Fatal("no migration defines audit_log_erase_metadata")
	return nil
}
