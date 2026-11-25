package controlwiring

import (
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// PR CI runs the Docker integration shards only when a diff matches the
// `integration` class. The suite imports ~70 module packages, most left to the
// nightly full run on purpose (the full closure would run the shards on nearly
// every Go PR). These tests make that a recorded decision per package: a
// package the suite starts importing directly must be added to the class or
// listed here with a reason, so a dependency cannot fall out of PR coverage
// unnoticed, as internal/ops/chops did.

const (
	// The risky kind: SQL or Redis calls that only the shards run against a
	// real engine. The first candidates to promote into the class.
	nightlyOwnQueries = "imports a database or Redis client itself, so only the shards exercise those calls; left to the nightly run"
	nightlyUnitTested = "covered on a PR by its own package tests and imports no database or Redis client; the suite composes it nightly"
	nightlyTypesOnly  = "declarations only, no functions"
)

// nightlyOnlyIntegrationImports lists packages test/integration or test/harness
// import directly whose changes do not run the shards on a PR.
var nightlyOnlyIntegrationImports = map[string]string{
	"internal/accounterasure":                    nightlyOwnQueries,
	"internal/aggregate/anomaly":                 nightlyUnitTested,
	"internal/aggregate/baseline":                nightlyUnitTested,
	"internal/aggregate/freeze":                  nightlyOwnQueries,
	"internal/aggregate/mev":                     nightlyOwnQueries,
	"internal/api/streaming/redispub":            nightlyOwnQueries,
	"internal/auth":                              nightlyOwnQueries,
	"internal/cachekeys":                         nightlyUnitTested,
	"internal/canonical":                         nightlyUnitTested,
	"internal/canonical/discovery":               nightlyUnitTested,
	"internal/completeness":                      nightlyUnitTested,
	"internal/config":                            nightlyUnitTested,
	"internal/consumer":                          nightlyTypesOnly,
	"internal/contractid":                        nightlyUnitTested,
	"internal/customerwebhook":                   nightlyUnitTested,
	"internal/decimalsguard":                     nightlyUnitTested,
	"internal/dispatcher":                        nightlyUnitTested,
	"internal/divergence":                        nightlyOwnQueries,
	"internal/domain":                            nightlyTypesOnly,
	"internal/entryhistory":                      nightlyUnitTested,
	"internal/events":                            nightlyUnitTested,
	"internal/ledgerstream":                      nightlyUnitTested,
	"internal/obs":                               nightlyUnitTested,
	"internal/ops/incident":                      nightlyOwnQueries,
	"internal/ops/ingest":                        nightlyUnitTested,
	"internal/ops/opsutil":                       nightlyUnitTested,
	"internal/pipeline":                          nightlyUnitTested,
	"internal/platform":                          nightlyUnitTested,
	"internal/pricealerts":                       nightlyUnitTested,
	"internal/pricelesscoverage":                 nightlyUnitTested,
	"internal/pricing/readset":                   nightlyUnitTested,
	"internal/pricingguard":                      nightlyUnitTested,
	"internal/projector":                         nightlyUnitTested,
	"internal/ratelimit":                         nightlyOwnQueries,
	"internal/rwa":                               nightlyUnitTested,
	"internal/scval":                             nightlyUnitTested,
	"internal/signupreaper":                      nightlyUnitTested,
	"internal/sources/aquarius":                  nightlyUnitTested,
	"internal/sources/band":                      nightlyUnitTested,
	"internal/sources/blend":                     nightlyUnitTested,
	"internal/sources/blnt":                      nightlyUnitTested,
	"internal/sources/cctp":                      nightlyUnitTested,
	"internal/sources/classicmovements":          nightlyUnitTested,
	"internal/sources/comet":                     nightlyUnitTested,
	"internal/sources/external":                  nightlyUnitTested,
	"internal/sources/external/binance":          nightlyUnitTested,
	"internal/sources/external/bitstamp":         nightlyUnitTested,
	"internal/sources/external/coingecko":        nightlyUnitTested,
	"internal/sources/external/ecb":              nightlyUnitTested,
	"internal/sources/external/exchangeratesapi": nightlyUnitTested,
	"internal/sources/external/scale":            nightlyUnitTested,
	"internal/sources/phoenix":                   nightlyUnitTested,
	"internal/sources/redstone":                  nightlyUnitTested,
	"internal/sources/reflector":                 nightlyUnitTested,
	"internal/sources/rozo":                      nightlyUnitTested,
	"internal/sources/sac_balances":              nightlyUnitTested,
	"internal/sources/sdex":                      nightlyUnitTested,
	"internal/sources/sep41_supply":              nightlyUnitTested,
	"internal/sources/sorobanevents":             nightlyUnitTested,
	"internal/sources/soroswap":                  nightlyUnitTested,
	"internal/sources/soroswap_router":           nightlyUnitTested,
	"internal/supply":                            nightlyUnitTested,
	"internal/usage":                             nightlyOwnQueries,
	"internal/xdrjson":                           nightlyUnitTested,
}

// database/sql/driver alone is a Scanner/Valuer type, not a client.
var storeClientImport = regexp.MustCompile(`^(database/sql$|github\.com/(jackc/pgx|lib/pq|ClickHouse/clickhouse-go|redis/go-redis)(/|$))`)

// suitePkg is one module package the integration suite imports directly.
type suitePkg struct {
	dir         string
	storeClient bool
	hasFuncs    bool
}

// integrationSuiteDirectImports returns, keyed by repo-relative directory, the
// module packages that test/integration and test/harness import directly
// under the integration build tag.
func integrationSuiteDirectImports(t *testing.T, root string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "integration",
		"-f", `{{join .Imports "\n"}}{{"\n"}}{{join .TestImports "\n"}}{{"\n"}}{{join .XTestImports "\n"}}`,
		"./test/integration/", "./test/harness/...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list the integration suite's imports: %v", err)
	}
	const modulePrefix = "github.com/Stellar-Index/StellarIndex/"
	dirs := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if rel, ok := strings.CutPrefix(strings.TrimSpace(line), modulePrefix); ok {
			dirs[rel] = true
		}
	}
	if len(dirs) == 0 {
		t.Fatal("go list returned no module import for the integration suite")
	}
	return dirs
}

// describeSuitePkgs reports, for each directory, whether it imports a store
// client and whether it declares any function.
func describeSuitePkgs(t *testing.T, root string, dirs []string) map[string]suitePkg {
	t.Helper()
	args := []string{
		"list", "-tags", "integration", "-f",
		`{{.Dir}}|{{.ImportPath}}|{{join .Imports " "}}|{{join .GoFiles " "}}`,
	}
	for _, d := range dirs {
		args = append(args, "./"+d)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list the suite's direct imports: %v", err)
	}
	const modulePrefix = "github.com/Stellar-Index/StellarIndex/"
	got := map[string]suitePkg{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			t.Fatalf("unexpected go list line %q", line)
		}
		p := suitePkg{dir: strings.TrimPrefix(f[1], modulePrefix)}
		for _, imp := range strings.Fields(f[2]) {
			if storeClientImport.MatchString(imp) {
				p.storeClient = true
			}
		}
		grep := exec.Command("grep", "-lE", `^func `)
		for _, g := range strings.Fields(f[3]) {
			grep.Args = append(grep.Args, f[0]+"/"+g)
		}
		if gout, _ := grep.Output(); len(gout) > 0 {
			p.hasFuncs = true
		}
		got[p.dir] = p
	}
	return got
}

func TestCIIntegrationClassDecidesEveryDirectImport(t *testing.T) {
	root := repoRoot(t)
	ciGlobs := ciIntegrationFilterGlobs(t, root)
	imports := integrationSuiteDirectImports(t, root)

	var uncovered []string
	for dir := range imports {
		if !globsFire(ciGlobs, dir+"/probe.go") {
			uncovered = append(uncovered, dir)
		}
	}
	sort.Strings(uncovered)
	pkgs := describeSuitePkgs(t, root, uncovered)
	for _, dir := range uncovered {
		reason, listed := nightlyOnlyIntegrationImports[dir]
		if !listed {
			t.Errorf("test/integration imports %s, but a PR changing it does not run the integration shards: "+
				"add it to the `integration` class (ci.yml preflight filter and scripts/ci/check-change-class.sh) "+
				"or to nightlyOnlyIntegrationImports with a reason", dir)
			continue
		}
		p := pkgs[dir]
		switch {
		case p.storeClient && reason != nightlyOwnQueries:
			t.Errorf("%s imports a database or Redis client; its nightlyOnlyIntegrationImports reason must be nightlyOwnQueries", dir)
		case !p.storeClient && reason == nightlyOwnQueries:
			t.Errorf("%s no longer imports a database or Redis client; nightlyOwnQueries no longer describes it", dir)
		case reason == nightlyTypesOnly && p.hasFuncs:
			t.Errorf("%s now declares functions; nightlyTypesOnly no longer describes it", dir)
		}
	}
	for dir := range nightlyOnlyIntegrationImports {
		switch {
		case !imports[dir]:
			t.Errorf("nightlyOnlyIntegrationImports lists %s, which the integration suite no longer imports directly: delete the entry", dir)
		case globsFire(ciGlobs, dir+"/probe.go"):
			t.Errorf("nightlyOnlyIntegrationImports lists %s, which the `integration` class already covers: delete the entry", dir)
		}
	}
}

// ciGlobRE translates a dorny/paths-filter glob into an anchored regexp:
// `**/` is any directory prefix, a trailing `/**` anything beneath, `*` one
// path segment's worth of characters.
func ciGlobRE(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// TestCIIntegrationFilterMatchesCheckChangeClass: the preflight filter and
// check-change-class.sh restate one rule. The workflow cross-checks them only
// on each PR's own diff; this compares them on every tracked path and on a
// probe under every glob.
func TestCIIntegrationFilterMatchesCheckChangeClass(t *testing.T) {
	root := repoRoot(t)
	ciGlobs := ciIntegrationFilterGlobs(t, root)
	classRE := checkChangeClassIntegrationRE(t, root)
	globREs := make([]*regexp.Regexp, len(ciGlobs))
	for i, g := range ciGlobs {
		globREs[i] = ciGlobRE(g)
	}
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, g := range ciGlobs {
		probe := strings.ReplaceAll(g, "**", "probe/nested")
		paths = append(paths, strings.ReplaceAll(probe, "*", "probe"))
	}
	mismatches := 0
	for _, p := range paths {
		if mismatches == 20 {
			t.Fatal("stopping after 20 mismatches")
		}
		ci := false
		for _, re := range globREs {
			if re.MatchString(p) {
				ci = true
				break
			}
		}
		if sh := classRE.MatchString(p); sh != ci {
			mismatches++
			t.Errorf("%s: ci.yml preflight `integration` filter says %v, check-change-class.sh class_integration says %v", p, ci, sh)
		}
	}
}

func TestCIGlobRE(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"internal/storage/**", "internal/storage/timescale/x.go", true},
		{"internal/storage/**", "internal/storagex/x.go", false},
		{"deploy/clickhouse/*.sql", "deploy/clickhouse/001.sql", true},
		{"deploy/clickhouse/*.sql", "deploy/clickhouse/sub/001.sql", false},
		{"**/testdata/**", "testdata/x", true},
		{"**/testdata/**", "internal/a/testdata/x", true},
		{"go.mod", "internal/go.mod", false},
	}
	for _, c := range cases {
		if got := ciGlobRE(c.glob).MatchString(c.path); got != c.want {
			t.Errorf("ciGlobRE(%q) on %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}
