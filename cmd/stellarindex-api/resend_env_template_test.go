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
	raw, err := os.ReadFile(resendEnvTemplate)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "# Resend transactional email")
	if start < 0 {
		t.Fatal("template has no '# Resend transactional email' block — this test is asserting nothing")
	}
	const keyLine = "\nSTELLARINDEX_RESEND_API_KEY="
	rel := strings.Index(body[start:], keyLine)
	if rel < 0 {
		t.Fatal("template has no STELLARINDEX_RESEND_API_KEY line after the Resend header")
	}
	end := start + rel + len(keyLine)
	if nl := strings.IndexByte(body[end:], '\n'); nl >= 0 {
		end += nl + 1
	} else {
		end = len(body)
	}
	if n := strings.Count(body, "STELLARINDEX_RESEND_API_KEY="); n != 1 {
		t.Fatalf("template assigns STELLARINDEX_RESEND_API_KEY %d times, want exactly 1", n)
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
			dest := filepath.Join(t.TempDir(), "out.env")
			vars, err := json.Marshal(tc.vars)
			if err != nil {
				t.Fatalf("marshal vars: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, ansible, "localhost", "-c", "local",
				"-m", "ansible.builtin.template",
				"-a", "src="+src+" dest="+dest,
				"-e", string(vars))
			scratch := t.TempDir()
			cmd.Env = append(os.Environ(),
				"ANSIBLE_LOCAL_TEMP="+scratch,
				"ANSIBLE_REMOTE_TEMP="+scratch,
				"ANSIBLE_LOCALHOST_WARNING=False",
				"ANSIBLE_NOCOLOR=1",
			)
			out, runErr := cmd.CombinedOutput()

			if tc.wantFail {
				if runErr == nil {
					t.Fatalf("render succeeded, want a refusal:\n%s", out)
				}
				if !strings.Contains(string(out), "vault_resend_api_key is empty") {
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
			if got := lines[len(lines)-1]; got != tc.wantLine {
				t.Errorf("last rendered line = %q, want %q", got, tc.wantLine)
			}
			for _, l := range lines {
				// Everything but the assignment is a comment: the guard's
				// {% %} tags must leave no residue in the systemd
				// EnvironmentFile.
				if l != tc.wantLine && !strings.HasPrefix(l, "#") {
					t.Errorf("unexpected non-comment line in the rendered block: %q", l)
				}
			}
		})
	}
}
