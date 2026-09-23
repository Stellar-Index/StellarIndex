package ingest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDocsCiteTheDeployedConfigPath — every `-config /etc/stellarindex…`
// an operator copies out of docs/ must name the file ansible actually
// templates. Runbooks citing /etc/stellarindex/config.toml sent operators
// mid-incident to a path that does not exist on any host.
func TestDocsCiteTheDeployedConfigPath(t *testing.T) {
	root := ingestDocsRepoRoot(t)
	task, err := os.ReadFile(filepath.Join(root, "configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"))
	if err != nil {
		t.Fatal(err)
	}
	dest := regexp.MustCompile(`(?m)^\s*src: stellarindex\.toml\.j2\s*\n\s*dest: (\S+)`).FindSubmatch(task)
	if dest == nil {
		t.Fatal("no stellarindex.toml.j2 template dest in 14-stellarindex-services.yml — regex drifted, fix the test")
	}
	deployed := string(dest[1])

	cite := regexp.MustCompile(`-config[ =](/etc/stellarindex[^\s` + "`" + `"')]*)`)
	var bad []string
	cites := 0
	err = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range cite.FindAllStringSubmatch(line, -1) {
				cites++
				if m[1] != deployed {
					rel, _ := filepath.Rel(root, path)
					bad = append(bad, rel+":"+strconv.Itoa(i+1)+": "+m[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cites == 0 {
		t.Fatal("no -config /etc/stellarindex… citations found under docs/ — regex drifted, fix the test")
	}
	if len(bad) > 0 {
		t.Errorf("docs cite a config path ansible does not deploy (want %s):\n%s", deployed, strings.Join(bad, "\n"))
	}
}
