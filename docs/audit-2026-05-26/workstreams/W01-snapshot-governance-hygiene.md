# W01 — Snapshot, governance, repo hygiene

## Scope

The repo as a managed artifact. Includes:

- exact commit SHA + dirty-worktree caveats at audit start
- `.gitignore` truth — what's ignored vs what should be
- `.gitleaks.toml` rules vs actual secrets in tracked files
- root docs: `README.md`, `CLAUDE.md`, `AGENTS.md`,
  `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`,
  `CODEOWNERS`, `VERSIONS.md`, `LICENSE`, `CHANGELOG.md`,
  `commitlint.config.js`
- `.github/` content: workflows, dependabot, issue/PR templates,
  release notes template
- discovery repo checkouts under `.discovery-repos/`
- residue: prebuilt binaries at repo root, `.DS_Store`,
  `.wrangler/`, prior audit residue (`docs/audit-2026-04-29`,
  `docs/audit-2026-05-02`, `docs/audit-2026-05-12`)
- `commitlint.config.js` rules vs `git log` reality

## Inputs

- `inventory/repo-snapshot.md`
- `git status` + `git log --oneline -200`
- `.gitignore`, `.gitleaks.toml`
- root files (README..CHANGELOG)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W01.1 | Exact commit SHA + clean tree | `git rev-parse HEAD`, `git status --porcelain` |
| W01.2 | `.gitignore` ignores no source files | inventory pass + git ls-files vs gitignore probe |
| W01.3 | `.gitleaks.toml` scans every tracked file with no leaks | `gitleaks detect --no-banner` |
| W01.4 | Every root doc reads cleanly + accurately | per-doc audit loop §11 |
| W01.5 | CHANGELOG row for rc.71..rc.81 cites real PRs | `git log` cross-ref |
| W01.6 | Prior audit residue: 04-29, 05-02, 05-12 directories are read-only references; nothing in product code reads them | grep for path refs |
| W01.7 | No prebuilt binaries committed at repo root | `git ls-files --modified ratesengine-*` |
| W01.8 | `.discovery-repos/` is gitignored AND not imported in product code | `git check-ignore` + `grep -r '.discovery-repos' internal/` |
| W01.9 | `commitlint.config.js` rules hold against last 200 commits | rebuild commitlint locally |
| W01.10 | CODEOWNERS reachable; every protected path covered | gh API + ownership map |
| W01.11 | dependabot config exists + reasonable cadence | `.github/dependabot.yml` |
| W01.12 | Branch protection on `main` enforces required checks | `gh api` |
| W01.13 | No `.DS_Store` or `.wrangler` in tracked files | `git ls-files` |
| W01.14 | NEW: prior agent-memory state at session-pause is NOT silently authoritative; the audit treats memory as hypothesis-only (per protocol §20) | memory-truth pass |
| W01.15 | NEW: CLAUDE.md drift since 2026-05-12 — are the "Things that will surprise you" lines all still accurate? | per-line audit |

## Evidence expectations

- One commit-SHA + worktree-state EV row at the top of evidence/log.md
- One CMD row for each shell command in this workstream
- Findings on every drift discovered

## Closure criteria

Every check above terminal. At least one EV row per check.
