package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const breachSectionHeading = "### 6.6 Personal-data breach notification assessment"

// TestSevPlaybookCarriesBreachNotificationAssessment pins T681: we hold
// customer emails and IPs (migration 0027), so the incident procedure must
// carry a jurisdiction-aware breach-notification assessment whose 72-hour
// regulator clock starts at awareness, and the paths that handle a PII
// exposure must route into it rather than ending at customer notification.
func TestSevPlaybookCarriesBreachNotificationAssessment(t *testing.T) {
	root := repoRootForOpsTest(t)
	pb := readRepoDoc(t, root, "docs/operations/sev-playbook.md")

	start := strings.Index(pb, breachSectionHeading)
	if start < 0 {
		t.Fatalf("sev-playbook.md has no %q section", breachSectionHeading)
	}
	section := pb[start:]
	if end := strings.Index(section[len(breachSectionHeading):], "\n## "); end >= 0 {
		section = section[:len(breachSectionHeading)+end]
	}

	for _, want := range []string{
		"72 hours",          // regulator deadline
		"aware",             // the clock runs from awareness, not resolution
		"Art. 33",           // regulator notification
		"Art. 34",           // data-subject notification
		"Art. 33(5)",        // every breach is recorded, notifiable or not
		"UK GDPR",           // jurisdiction: UK regime
		"EU GDPR",           // jurisdiction: EU regime
		"ICO",               // UK regulator named
		"fails closed",      // undecided means notify
		"Breach assessment", // where the register entry lives
	} {
		if !strings.Contains(section, want) {
			t.Errorf("§6.6 breach assessment is missing %q", want)
		}
	}

	const anchor = "#66-personal-data-breach-notification-assessment"
	if !strings.Contains(pb[:start], "("+anchor+")") {
		t.Error("sev-playbook.md §4/§6.5 must route a possible personal-data incident to §6.6 before it is resolved")
	}
	runbook := readRepoDoc(t, root, "docs/operations/runbooks/credential-exposure-redaction-fix.md")
	if !strings.Contains(runbook, "sev-playbook.md"+anchor) {
		t.Error("credential-exposure-redaction-fix.md's PII step must link sev-playbook §6.6; " +
			"customer notification alone does not settle whether a regulator must be told")
	}
}

func readRepoDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
