package controlwiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Q250: ansible must never guess which region it is talking to ──
//
// configs/ansible/ansible.cfg used to set `inventory = ./inventory` — the
// whole DIRECTORY. Ansible merges a directory inventory, and every region
// file declares the same `archival_nodes` group, so ONE forgotten `-i`
// pointed playbooks/deploy-binary.yml (`hosts: all`) and archival-node.yml /
// monitoring.yml (`hosts: archival_nodes`) at production r1 and both test
// nets at once. The merge also silently rewrote per-region host vars by load
// order: si-futurenet resolved to stellar_network=testnet, region_id=testnet,
// galexie_start_ledger=4340000 and postgres_replication_role=async-replica
// against r1-01.stellarindex.io.
//
// The sibling of this footgun has already fired on this project: omitting
// `-e secrets_file=` applied r1's secrets to a test net and took it down. So
// the default is fail-closed — NO inventory at all, rather than a default
// naming one region, which would only move the silent target.
//
// Two legs. The first states the invariant in the config. The second — where
// ansible is on PATH — resolves the REAL playbooks and asserts the dangerous
// one (`hosts: all`) reaches nothing without -i, and exactly one region with
// it. The second leg is the load-bearing one: it is the difference between
// "the cfg line changed" and "no region is reachable by accident".
const ansibleDir = "configs/ansible"

// realInventoryHosts must never be resolved by a run that did not name an
// inventory. r2/r3 are example templates today but are listed for the day
// they are not.
var realInventoryHosts = []string{
	"si-testnet",
	"si-futurenet",
	"r1-01.stellarindex.io",
	"r2-01.stellarindex.io",
	"r3-01.stellarindex.io",
}

func TestQ250_AnsibleCfgHasNoDefaultInventory(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	inv, ok := ansibleCfgValue(t, root)["defaults.inventory"]
	if !ok {
		t.Fatalf("ansible.cfg has no [defaults] inventory key; this test must be re-derived")
	}
	resolved := filepath.Join(root, ansibleDir, filepath.FromSlash(inv))

	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("default inventory %q (%s) does not exist: %v", inv, resolved, err)
	}
	if info.IsDir() {
		t.Fatalf("ansible.cfg's default inventory %q is a DIRECTORY — ansible merges every "+
			"region file in it into one `archival_nodes` group, so a run without -i targets "+
			"production r1 and both test nets together. Point it at a guard file that no "+
			"inventory plugin can parse.", inv)
	}

	// The guard must not itself be (or become) a usable inventory: outside its
	// comments it must carry no `key:` line, i.e. it is not a YAML mapping, so
	// it declares no groups and no hosts to run against.
	body, err := os.ReadFile(resolved) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read default inventory %s: %v", resolved, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, found := strings.Cut(line, ":"); found && !strings.ContainsAny(key, " \t") {
			t.Errorf("default inventory %q declares %q — a mapping key makes it a parseable "+
				"YAML inventory, so a run without -i would proceed against whatever it names",
				inv, strings.TrimSpace(line))
		}
	}
}

func TestQ250_AnsibleResolvesNoHostsWithoutExplicitInventory(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skipf("ansible-playbook not on PATH: %v (config leg covered by "+
			"TestQ250_AnsibleCfgHasNoDefaultInventory)", err)
	}
	env := ansibleTestEnv(t)
	dir := filepath.Join(repoRoot(t), ansibleDir)

	// deploy-binary.yml is `hosts: all` — the widest pattern in the tree, and
	// the play that ships binaries. Without -i it must match nothing.
	out, _ := runAnsible(t, bin, dir, env,
		"--list-hosts", "playbooks/deploy-binary.yml")
	for _, host := range realInventoryHosts {
		if strings.Contains(out, host) {
			t.Errorf("`ansible-playbook --list-hosts playbooks/deploy-binary.yml` with no -i "+
				"resolved %q — a forgotten -i can still reach a region:\n%s", host, out)
		}
	}
	if !strings.Contains(out, "hosts (0)") {
		t.Errorf("`hosts: all` with no -i did not resolve an empty host set:\n%s", out)
	}

	// Control: the explicit form still resolves exactly one region, so the
	// leg above is not merely "ansible is broken in this environment".
	out, err = runAnsible(t, bin, dir, env,
		"--list-hosts", "-i", "inventory/testnet.yml", "playbooks/deploy-binary.yml")
	if err != nil {
		t.Fatalf("explicit -i inventory/testnet.yml failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "si-testnet") {
		t.Errorf("explicit -i inventory/testnet.yml did not resolve si-testnet:\n%s", out)
	}
	for _, host := range realInventoryHosts[1:] {
		if strings.Contains(out, host) {
			t.Errorf("explicit -i inventory/testnet.yml leaked %q from another region:\n%s", host, out)
		}
	}
}

// runAnsible runs an ansible CLI from the ansible directory so ansible.cfg is
// the one under test, with a sanitised environment so an ANSIBLE_* var in the
// caller's shell cannot decide the verdict.
func runAnsible(t *testing.T, bin, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // fixed binary, literal args
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ansibleTestEnv drops inherited ANSIBLE_* settings and substitutes a stub
// vault password file, so the test never reads (or races) the operator's.
func ansibleTestEnv(t *testing.T) []string {
	t.Helper()
	vaultPass := filepath.Join(t.TempDir(), "vault-pass")
	if err := os.WriteFile(vaultPass, []byte("test-only\n"), 0o600); err != nil {
		t.Fatalf("write stub vault password file: %v", err)
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANSIBLE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "ANSIBLE_VAULT_PASSWORD_FILE="+vaultPass)
}

// ansibleCfgValue returns the ansible.cfg keys as "section.key" → value.
func ansibleCfgValue(t *testing.T, root string) map[string]string {
	t.Helper()
	path := filepath.Join(root, ansibleDir, "ansible.cfg")
	b, err := os.ReadFile(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[section+"."+strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
