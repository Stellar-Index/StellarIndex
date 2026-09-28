package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RLT-321, ansible leg. The env template used to render
//
//	STELLARINDEX_RESEND_API_KEY={{ vault_resend_api_key | default('') }}
//
// with nothing in front of it: a vault without the key rendered an empty
// value, the role applied green, and the API booted unable to deliver any
// sign-in email. These tests read the SHIPPED template, not a copy.

const resendEnvTemplate = "../../configs/ansible/roles/archival-node/templates/stellarindex.env.j2"

// resendTemplateBlock returns the template text from the Resend comment
// header through the STELLARINDEX_RESEND_API_KEY line — the guard and the
// line it guards, and nothing that needs the role's other variables.
func resendTemplateBlock(t *testing.T) string {
	t.Helper()
	return envTemplateBlock(t, "# Resend transactional email", "STELLARINDEX_RESEND_API_KEY")
}

// envTemplateBlock returns the shipped template text from header through
// the single `key=` line that follows it.
func envTemplateBlock(t *testing.T, header, key string) string {
	t.Helper()
	raw, err := os.ReadFile(resendEnvTemplate)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, header)
	if start < 0 {
		t.Fatalf("template has no %q block — this test is asserting nothing", header)
	}
	keyLine := "\n" + key + "="
	rel := strings.Index(body[start:], keyLine)
	if rel < 0 {
		t.Fatalf("template has no %s line after %q", key, header)
	}
	end := start + rel + len(keyLine)
	if nl := strings.IndexByte(body[end:], '\n'); nl >= 0 {
		end += nl + 1
	} else {
		end = len(body)
	}
	if n := strings.Count(body, "\n"+key+"="); n != 1 {
		t.Fatalf("template assigns %s %d times, want exactly 1", key, n)
	}
	return body[start:end]
}

// Static leg — runs everywhere, ansible or not.
func TestResendEnvTemplate_EmptyKeyIsGuardedOnProduction(t *testing.T) {
	block := resendTemplateBlock(t)
	for _, want := range []string{
		"undef(hint=",                     // the render is REFUSED, not defaulted
		"vault_resend_api_key",            // ...on this variable
		"region_deployment",               // ...scoped to production, so the test nets still render
		"stellarindex_dashboard_base_url", // ...and only where the dashboard is mounted
	} {
		if !strings.Contains(block, want) {
			t.Errorf("Resend block lacks %q: an empty vault_resend_api_key would render silently "+
				"on a production host (RLT-321)\n%s", want, block)
		}
	}
	guard := strings.Index(block, "undef(hint=")
	assign := strings.Index(block, "\nSTELLARINDEX_RESEND_API_KEY=")
	if guard < 0 || assign < guard {
		t.Errorf("the guard must precede the assignment it protects")
	}
}

// Render leg — the real template text through the real ansible, when one is
// installed. Skipped (not passed) otherwise; the static leg still holds.
func TestResendEnvTemplate_RendersUnderEveryDeploymentShape(t *testing.T) {
	ansible, err := exec.LookPath("ansible")
	if err != nil {
		t.Skip("ansible not on PATH — render leg skipped; static leg covers the guard's presence")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "resend-block.j2")
	if err := os.WriteFile(src, []byte(resendTemplateBlock(t)), 0o600); err != nil {
		t.Fatalf("write block: %v", err)
	}
	const fixtureValue = "rlt321-fixture-not-a-real-credential" // gitleaks:allow

	for _, tc := range []struct {
		name     string
		vars     map[string]any
		wantFail bool
		wantLine string
	}{
		{
			name:     "production, key absent: render refused",
			vars:     map[string]any{},
			wantFail: true,
		},
		{
			name:     "production, key blank-only: render refused",
			vars:     map[string]any{"vault_resend_api_key": "  "},
			wantFail: true,
		},
		{
			name:     "production, key present: rendered verbatim",
			vars:     map[string]any{"vault_resend_api_key": fixtureValue},
			wantLine: "STELLARINDEX_RESEND_API_KEY=" + fixtureValue,
		},
		{
			name:     "testnet, key absent: still renders empty (nobody signs in there)",
			vars:     map[string]any{"region_deployment": "testnet"},
			wantLine: "STELLARINDEX_RESEND_API_KEY=",
		},
		{
			name:     "futurenet, key absent: still renders empty",
			vars:     map[string]any{"region_deployment": "futurenet"},
			wantLine: "STELLARINDEX_RESEND_API_KEY=",
		},
		{
			name:     "production, dashboard unmounted: still renders empty",
			vars:     map[string]any{"stellarindex_dashboard_base_url": ""},
			wantLine: "STELLARINDEX_RESEND_API_KEY=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEnvBlockRender(t, ansible, src, tc.vars, tc.wantFail, "vault_resend_api_key is empty", tc.wantLine)
		})
	}
}

// assertEnvBlockRender renders src through ansible's template module with
// vars and checks it is refused with hint, or renders only comments plus
// wantLine as its last line.
func assertEnvBlockRender(t *testing.T, ansible, src string, vars map[string]any, wantFail bool, hint, wantLine string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out.env")
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		t.Fatalf("marshal vars: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ansible, "localhost", "-c", "local",
		"-m", "ansible.builtin.template",
		"-a", "src="+src+" dest="+dest,
		"-e", string(varsJSON))
	scratch := t.TempDir()
	cmd.Env = append(os.Environ(),
		"ANSIBLE_LOCAL_TEMP="+scratch,
		"ANSIBLE_REMOTE_TEMP="+scratch,
		"ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_NOCOLOR=1",
	)
	out, runErr := cmd.CombinedOutput()

	if wantFail {
		if runErr == nil {
			t.Fatalf("render succeeded, want a refusal:\n%s", out)
		}
		if !strings.Contains(string(out), hint) {
			t.Errorf("render failed, but not with the guard's hint:\n%s", out)
		}
		if _, statErr := os.Stat(dest); statErr == nil {
			t.Errorf("a refused render still wrote the env file")
		}
		return
	}
	if runErr != nil {
		t.Fatalf("render failed: %v\n%s", runErr, out)
	}
	rendered, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(rendered), "\n"), "\n")
	if got := lines[len(lines)-1]; got != wantLine {
		t.Errorf("last rendered line = %q, want %q", got, wantLine)
	}
	for _, l := range lines {
		// Everything but the assignment is a comment: the guard's
		// {% %} tags must leave no residue in the systemd
		// EnvironmentFile.
		if l != wantLine && !strings.HasPrefix(l, "#") {
			t.Errorf("unexpected non-comment line in the rendered block: %q", l)
		}
	}
}

// The dashboard code secret is required wherever the dashboard is mounted —
// test nets included — because passkeys are wired there and the API refuses
// to boot without it; the render must refuse first.
func TestDashboardCodeSecretEnvTemplate_RendersUnderEveryDeploymentShape(t *testing.T) {
	block := envTemplateBlock(t, "# Root server secret for dashboard auth", "STELLARINDEX_DASHBOARD_CODE_SECRET")
	guard := strings.Index(block, "undef(hint=")
	assign := strings.Index(block, "\nSTELLARINDEX_DASHBOARD_CODE_SECRET=")
	if guard < 0 || assign < guard || !strings.Contains(block, "stellarindex_dashboard_base_url") {
		t.Fatalf("code-secret block must refuse an empty secret where the dashboard is mounted, before the assignment:\n%s", block)
	}

	ansible, err := exec.LookPath("ansible")
	if err != nil {
		t.Skip("ansible not on PATH — render leg skipped; static leg covers the guard's presence")
	}
	src := filepath.Join(t.TempDir(), "code-secret-block.j2")
	if err := os.WriteFile(src, []byte(block), 0o600); err != nil {
		t.Fatalf("write block: %v", err)
	}
	const fixtureValue = "code-secret-fixture-not-a-real-credential" // gitleaks:allow
	const hint = "vault_dashboard_code_secret is empty"
	for _, tc := range []struct {
		name     string
		vars     map[string]any
		wantFail bool
		wantLine string
	}{
		{name: "production, secret absent: render refused", vars: map[string]any{}, wantFail: true},
		{name: "production, secret blank-only: render refused", vars: map[string]any{"vault_dashboard_code_secret": "  "}, wantFail: true},
		{name: "testnet, secret absent: render refused", vars: map[string]any{"region_deployment": "testnet"}, wantFail: true},
		{name: "futurenet, secret absent: render refused", vars: map[string]any{"region_deployment": "futurenet"}, wantFail: true},
		{
			name:     "secret present: rendered verbatim",
			vars:     map[string]any{"vault_dashboard_code_secret": fixtureValue},
			wantLine: "STELLARINDEX_DASHBOARD_CODE_SECRET=" + fixtureValue,
		},
		{
			name:     "dashboard unmounted, secret absent: renders empty",
			vars:     map[string]any{"stellarindex_dashboard_base_url": ""},
			wantLine: "STELLARINDEX_DASHBOARD_CODE_SECRET=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEnvBlockRender(t, ansible, src, tc.vars, tc.wantFail, hint, tc.wantLine)
		})
	}
}
