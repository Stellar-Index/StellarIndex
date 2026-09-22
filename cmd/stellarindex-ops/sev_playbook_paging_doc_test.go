package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSevPlaybookPagingWarningMatchesAlertmanagerConfig pins SL20
// (reverification sweep, checked against 8078fdaec): sev-playbook.md's
// §3 warning claimed Alertmanager's fanout receivers were "no-op
// stubs" that "reach no human". That stopped being true once
// configs/alertmanager/alertmanager.r1.yml's chat-page/chat-default/
// chat-informational receivers gained real discord_configs (migration
// off Slack, 38b98ecd9). The doc must say so instead of repeating the
// stale claim — and it must keep warning that PagerDuty escalation
// (§4/§7's assumption) is still genuinely unbuilt, since that config
// file has no pagerduty_configs for any severity.
func TestSevPlaybookPagingWarningMatchesAlertmanagerConfig(t *testing.T) {
	root := repoRootForOpsTest(t)

	playbook, err := os.ReadFile(filepath.Join(root, "docs/operations/sev-playbook.md"))
	if err != nil {
		t.Fatalf("read sev-playbook.md: %v", err)
	}
	pb := string(playbook)

	if strings.Contains(pb, "Alertmanager's fanout receivers are no-op stubs") {
		t.Error("sev-playbook.md still claims Alertmanager's fanout receivers are no-op stubs; " +
			"configs/alertmanager/alertmanager.r1.yml's chat-page/chat-default/chat-informational " +
			"receivers carry real discord_configs, not empty stubs")
	}
	if !strings.Contains(pb, "PagerDuty") {
		t.Error("sev-playbook.md's paging warning dropped its PagerDuty callout; " +
			"the PagerDuty escalation path §4 and §7 assume is still unbuilt and must stay flagged")
	}

	alertmanagerCfg, err := os.ReadFile(filepath.Join(root, "configs/alertmanager/alertmanager.r1.yml"))
	if err != nil {
		t.Fatalf("read alertmanager.r1.yml: %v", err)
	}
	cfg := string(alertmanagerCfg)

	if !strings.Contains(cfg, "discord_configs") {
		t.Fatal("alertmanager.r1.yml has no discord_configs at all — the doc's " +
			"'Discord fanout is wired' claim would be false")
	}
	// Match an actual receiver key, not the "silent" receiver's comment
	// explaining that it deliberately has none.
	pagerdutyKey := regexp.MustCompile(`(?m)^\s*pagerduty_configs:`)
	if pagerdutyKey.MatchString(cfg) {
		t.Fatal("alertmanager.r1.yml now has a pagerduty_configs block — the doc's " +
			"'PagerDuty is not wired' warning would be false and must be updated, not this test")
	}
}
