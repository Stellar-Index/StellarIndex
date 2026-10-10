package main

import (
	"errors"
	"flag"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

type writeClass int

const (
	// readOnly writes nothing but its own stdout and stderr.
	readOnly writeClass = iota
	// gated writes a datastore and carries opsutil's shared -write/-dry-run gate.
	gated
	// exempt writes something, but not a datastore a preview could protect:
	// local files, metrics, or a publish that is the whole command.
	exempt
)

type subcommandClass struct {
	class writeClass
	// why is required for exempt (why no gate).
	why string
}

func ro() subcommandClass                      { return subcommandClass{class: readOnly} }
func gate() subcommandClass                    { return subcommandClass{class: gated} }
func exemptBecause(why string) subcommandClass { return subcommandClass{class: exempt, why: why} }

// subcommandClasses classifies every dispatchable stellarindex-ops command by
// how it writes. A namespace's sub-verbs are keyed "namespace verb".
var subcommandClasses = map[string]subcommandClass{
	"docs-config":           ro(),
	"mint-key":              gate(),
	"upgrade-key":           gate(),
	"emit-incident":         gate(),
	"usage-rollup-backfill": gate(),
	"freeze-unfreeze":       gate(),
	"account-erase":         gate(),
	"change-summary-reset":  gate(),

	"rpc-probe":             ro(),
	"verify-decoders":       ro(),
	"verify-external":       ro(),
	"hubble-check":          ro(),
	"hubble-soroban-events": ro(),

	"backfill":                gate(),
	"backfill-external":       gate(),
	"backfill-chainlink":      gate(),
	"detect-gaps":             ro(),
	"list-cursors":            ro(),
	"reap-cursors":            gate(),
	"resume-stalled":          gate(),
	"find-data-gaps":          ro(),
	"census-backfill":         gate(),
	"tag-routed-via":          gate(),
	"tag-signer":              gate(),
	"tag-tx-index":            gate(),
	"seed-soroswap-pairs":     gate(),
	"seed-protocol-contracts": gate(),
	"seed-entry-counts":       gate(),
	"projector-replay":        gate(),
	"scan-soroban-events":     ro(),
	"state-snapshot":          gate(),
	"issuer-enrich":           gate(),
	"sep1-refresh":            gate(),
	"issuer-flags":            gate(),
	"directory-sync":          gate(),
	"directory-override":      gate(),
	"listing-sync":            gate(),
	"curated-rwa-sync":        gate(),
	"asset-registry-backfill": gate(),

	"verify-archive":            exemptBecause("verifier; serves metrics and writes its -textfile-output and -state-file only"),
	"cross-region-check":        ro(),
	"cross-region-monitor":      exemptBecause("serves a metrics listener only"),
	"trim-galexie-archive":      gate(),
	"rehydrate-galexie-archive": gate(),
	"galexie-mirror-verify":     exemptBecause("verifier; writes its -textfile-output metrics only"),
	"wasm-history":              exemptBecause("writes local JSONL outputs and -checkpoint-dir only"),
	"wasm-history-merge-jsonl":  exemptBecause("merges local checkpoint JSONL into its -output file only"),
	"extract-wasm-from-galexie": exemptBecause("writes WASM blobs into its local -output-dir only"),
	"compare-entry-changes":     ro(),

	"discovery list": ro(),

	"archive-completeness check":  exemptBecause("writes its -output-file report only"),
	"archive-completeness fix":    gate(),
	"archive-completeness verify": gate(),

	"supply audit":                   ro(),
	"supply snapshot":                gate(),
	"supply seed-observations":       gate(),
	"supply seed-sac-balances":       gate(),
	"supply seed-claimable-balances": gate(),
	"supply seed-sep41-genesis":      gate(),
	"supply verify-rollup":           exemptBecause("verifier; writes its -textfile-output metrics only"),

	"ch-backfill":                  gate(),
	"ch-gate":                      ro(),
	"ch-reproject":                 ro(),
	"ch-rebuild":                   gate(),
	"trades-cagg-refresh":          gate(),
	"ch-supply":                    gate(),
	"ch-txindex-backfill":          gate(),
	"ch-contract-ledgers-backfill": gate(),
	"ch-instance-backfill":         gate(),
	"ch-census-rollup":             gate(),
	"ch-cap67-movements":           gate(),
	"ch-entry-history":             gate(),
	"ch-holders-rollup":            gate(),
	"ch-creators-rollup":           gate(),
	"ch-sponsors-rollup":           gate(),
	"ch-cohort-rollup":             gate(),
	"ch-participant-backfill":      gate(),
	"ch-recognition":               ro(),
	"verify-recognition":           ro(),
	"verify-reconciliation":        ro(),
	"compute-completeness":         gate(),
	"verify-served-values":         exemptBecause("verifier; writes its -textfile metrics only"),
	"verify-usd-volume":            ro(),
	"usd-volume-restamp":           gate(),

	"classic-movements-backfill": gate(),
	"projected-rebuild":          gate(),
	"reconcile-balances":         ro(),
	"verify-contiguity":          ro(),
	"verify-hashchain":           ro(),
	"verify-lake":                exemptBecause("verifier; writes its -textfile metrics only"),
	"verify-network-state":       exemptBecause("verifier; writes its -textfile metrics only"),
	"wasm-drift":                 exemptBecause("verifier; writes its -textfile metrics only"),
}

// pinnedReadOnly is literal so reclassifying a command as read-only is an edit
// here, not a drive-by relabel.
var pinnedReadOnly = []string{
	"ch-gate",
	"ch-recognition",
	"ch-reproject",
	"compare-entry-changes",
	"cross-region-check",
	"detect-gaps",
	"discovery list",
	"docs-config",
	"find-data-gaps",
	"hubble-check",
	"hubble-soroban-events",
	"list-cursors",
	"reconcile-balances",
	"rpc-probe",
	"scan-soroban-events",
	"supply audit",
	"verify-contiguity",
	"verify-decoders",
	"verify-external",
	"verify-hashchain",
	"verify-recognition",
	"verify-reconciliation",
	"verify-usd-volume",
}

var namespaceUsageRe = regexp.MustCompile(`usage: (\S+) <?([a-z0-9|-]+)>?`)

// TestEverySubcommandIsClassified makes the table above the coverage: a new
// subcommand, or a new namespace verb, fails here until someone decides how it
// writes.
func TestEverySubcommandIsClassified(t *testing.T) {
	seen := map[string]bool{}
	for name := range subcommands {
		classifiedVerbs := classifiedSubVerbs(name)
		if len(classifiedVerbs) == 0 {
			if _, ok := subcommandClasses[name]; !ok {
				t.Errorf("%s is not in subcommandClasses: classify it as gated, readOnly or exempt", name)
			}
			seen[name] = true
			continue
		}
		m := namespaceUsageRe.FindStringSubmatch(helpOutput(t, []string{name}))
		if m == nil || m[1] != name {
			t.Errorf("%s has classified sub-verbs but its -h prints no `usage: %s <verb|…>` line to check them against", name, name)
			continue
		}
		listed := strings.Split(m[2], "|")
		sort.Strings(listed)
		if strings.Join(listed, " ") != strings.Join(classifiedVerbs, " ") {
			t.Errorf("%s dispatches %v but subcommandClasses classifies %v", name, listed, classifiedVerbs)
		}
		for _, v := range listed {
			seen[name+" "+v] = true
		}
	}
	for path := range subcommandClasses {
		if !seen[path] {
			t.Errorf("subcommandClasses lists %q, which stellarindex-ops does not dispatch: delete the entry", path)
		}
	}
}

// classifiedSubVerbs returns, sorted, the verbs classified under namespace name.
func classifiedSubVerbs(name string) []string {
	var verbs []string
	for path := range subcommandClasses {
		if verb, ok := strings.CutPrefix(path, name+" "); ok {
			verbs = append(verbs, verb)
		}
	}
	sort.Strings(verbs)
	return verbs
}

// TestSubcommandWriteGateMatchesItsClass reads each command's declared flags
// off its own -h. A gated command must carry the shared fail-closed pair; every
// other class must not.
func TestSubcommandWriteGateMatchesItsClass(t *testing.T) {
	paths := make([]string, 0, len(subcommandClasses))
	for path := range subcommandClasses {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		c := subcommandClasses[path]
		usage, err := helpResult(t, strings.Fields(path))
		if c.class != readOnly {
			if !errors.Is(err, flag.ErrHelp) || usage == "" {
				t.Errorf("%s -h must return flag.ErrHelp with usage output before doing any work; got err=%v, %d bytes", path, err, len(usage))
			}
		}
		writeLine := usageFlagBlock(usage, "write")
		hasGate := strings.Contains(writeLine, opsutil.WriteFlagUsage)
		switch c.class {
		case gated:
			switch {
			case !hasGate:
				t.Errorf("%s is classified gated but its -write is missing or not the shared opsutil gate. Got:\n%s", path, writeLine)
			case strings.Contains(writeLine, "(default true)"):
				t.Errorf("%s defaults -write to TRUE: the gate is fail-open", path)
			case !strings.Contains(usageFlagBlock(usage, "dry-run"), "the DEFAULT"):
				t.Errorf("%s does not declare the shared -dry-run alias with dry run as the DEFAULT", path)
			}
		case exempt:
			if hasGate {
				t.Errorf("%s carries the shared write gate but is classified exempt: classify it gated", path)
			}
		case readOnly:
			if writeLine != "" {
				t.Errorf("%s is classified readOnly but declares -write: classify it gated", path)
			}
		}
		if c.class == exempt && c.why == "" {
			t.Errorf("%s has no reason: an exempt entry names why no gate applies", path)
		}
	}
}

// usageFlagBlock returns one flag's usage block from flag.FlagSet defaults:
// its `  -name` line plus the indented description, or "" when undeclared.
func usageFlagBlock(usage, name string) string {
	lines := strings.Split(usage, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, "  -") || len(fields) == 0 || fields[0] != "-"+name {
			continue
		}
		block := line
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "    \t") && !strings.HasPrefix(next, "\t") {
				break
			}
			block += "\n" + next
		}
		return block
	}
	return ""
}

// TestWriteClassSetsArePinned fails when a command enters or leaves readOnly
// without pinnedReadOnly changing with it.
func TestWriteClassSetsArePinned(t *testing.T) {
	var got []string
	for path, c := range subcommandClasses {
		if c.class == readOnly {
			got = append(got, path)
		}
	}
	sort.Strings(got)
	want := append([]string(nil), pinnedReadOnly...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("readOnly set drifted from pinnedReadOnly.\n got: %v\nwant: %v\nEdit the pinned list deliberately.", got, want)
	}
}
