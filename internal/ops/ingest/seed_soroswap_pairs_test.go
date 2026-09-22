package ingest

import (
	"bytes"
	"strings"
	"testing"
)

// A keyed stellar-rpc provider carries its API key in the URL path
// (e.g. .../v2/<KEY>), the same shape RLT-441 found printed raw to
// stderr. logSeedStart must not repeat it.
func TestLogSeedStartRedactsKeyedRPCEndpoint(t *testing.T) {
	var buf bytes.Buffer
	const pathCredentialFragment = "provider-cred-9f2c7a1b4e6d8091"
	logSeedStart(&buf, "CFACTORYCONTRACTSTRKEY", "https://rpc.example-provider.com/v2/"+pathCredentialFragment)

	out := buf.String()
	if strings.Contains(out, pathCredentialFragment) {
		t.Fatalf("RPC endpoint secret leaked into log line: %q", out)
	}
	if !strings.Contains(out, "factory=CFACTORYCONTRACTSTRKEY") {
		t.Fatalf("factory contract missing from log line: %q", out)
	}
	const wantRedacted = "rpc=https://rpc.example-provider.com/<redacted>"
	if !strings.Contains(out, wantRedacted) {
		t.Fatalf("expected redacted endpoint form %q, got: %q", wantRedacted, out)
	}
}
