#!/usr/bin/env bash
# pages-deploy-branch-test.sh — every `wrangler pages deploy` job must label
# its deploy with a branch that agrees with the ref it built (GH-844).
#
# For each job in .github/workflows/ whose step runs `pages deploy`, this
# finds the step that exports BRANCH, evaluates that step's `env:` against a
# simulated workflow_dispatch (dispatching ref x environment input), runs its
# real `run:` script and checks the outcome. Also pins the structure that
# makes the check meaningful: the checkout takes no `ref:` (it builds the
# dispatching ref), the deploy passes `--branch ${{ env.BRANCH }}`, and the
# BRANCH step goes through scripts/ci/resolve-pages-branch.sh.
# No network, no gh.
#
# Run: bash scripts/ci/pages-deploy-branch-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOWS_DIR="${WORKFLOWS_DIR:-.github/workflows}"

python3 - "$WORKFLOWS_DIR" <<'PY'
import glob, os, re, subprocess, sys, tempfile
import yaml

TOKEN = re.compile(r"\s*(\|\||&&|==|!=|\(|\)|'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_.-]*)")


def evaluate(expr, ctx):
    """Evaluate the GitHub-expression subset these steps use."""
    toks, pos = [], 0
    expr = expr.strip()
    while pos < len(expr):
        m = TOKEN.match(expr, pos)
        if not m:
            raise ValueError(f"unsupported expression: {expr!r}")
        toks.append(m.group(1))
        pos = m.end()
    toks.append(None)
    i = 0

    def peek():
        return toks[i]

    def take():
        nonlocal i
        i += 1
        return toks[i - 1]

    def primary():
        t = take()
        if t == "(":
            v = disj()
            take()
            return v
        if t.startswith("'"):
            return t[1:-1].replace("''", "'")
        if t in ("true", "false"):
            return t == "true"
        if t not in ctx:
            raise ValueError(f"unknown context {t!r} in {expr!r}")
        return ctx[t]

    def cmp():
        v = primary()
        if peek() in ("==", "!="):
            op, r = take(), primary()
            return (v == r) == (op == "==")
        return v

    def conj():
        v = cmp()
        while peek() == "&&":
            take()
            r = cmp()
            v = r if v else v
        return v

    def disj():
        v = conj()
        while peek() == "||":
            take()
            r = conj()
            v = v if v else r
        return v

    return disj()


def render(value, ctx):
    value = str(value)
    whole = re.fullmatch(r"\$\{\{(.*)\}\}", value.strip(), re.S)
    if whole:
        return str(evaluate(whole.group(1), ctx))
    return re.sub(r"\$\{\{(.*?)\}\}", lambda m: str(evaluate(m.group(1), ctx)), value)


passed = failed = 0


def check(ok, msg):
    global passed, failed
    print(("  ok   " if ok else "  FAIL ") + msg)
    if ok:
        passed += 1
    else:
        failed += 1


def run_step(step, env_input, ref):
    name = ref.split("/", 2)[2]
    ctx = {
        "github.ref": ref,
        "github.ref_name": name,
        "github.event.inputs.environment": env_input,
        "inputs.environment": env_input,
    }
    with tempfile.NamedTemporaryFile("r", suffix=".env") as out:
        env = {"PATH": os.environ["PATH"], "GITHUB_ENV": out.name,
               "GITHUB_REF": ref, "GITHUB_REF_NAME": name}
        for k, v in (step.get("env") or {}).items():
            env[k] = render(v, ctx)
        proc = subprocess.run(["bash", "-c", step["run"]], env=env,
                              capture_output=True, text=True)
        branch = None
        for line in out.read().splitlines():
            if line.startswith("BRANCH="):
                branch = line[len("BRANCH="):]
        return proc.returncode, branch


def branch_step(steps):
    for step in steps:
        run = step.get("run") or ""
        if "resolve-pages-branch.sh" in run or ("BRANCH=" in run and "GITHUB_ENV" in run):
            return step
    return None


def check_job(label, job, has_env_input):
    steps = job.get("steps") or []
    checkout = next((s for s in steps if str(s.get("uses", "")).startswith("actions/checkout@")), None)
    check(checkout is not None and "ref" not in (checkout.get("with") or {}),
          f"{label}: checkout builds the dispatching ref (no `ref:` override)")
    deploy = next(s for s in steps if "pages deploy" in str((s.get("with") or {}).get("command", "")))
    cmd = " ".join(str(deploy["with"]["command"]).split())
    check(re.search(r"--branch[= ]\$\{\{ env\.BRANCH \}\}(\s|$)", cmd) is not None,
          f"{label}: deploy passes --branch ${{{{ env.BRANCH }}}} (got {cmd!r})")
    step = branch_step(steps)
    check(step is not None and "scripts/ci/resolve-pages-branch.sh" in step["run"],
          f"{label}: BRANCH is resolved by scripts/ci/resolve-pages-branch.sh")
    if step is None:
        return
    cases = [("production", "refs/heads/main", "main"),
             ("production", "refs/heads/feature/x", None),
             ("production", "refs/tags/main", None)]
    if has_env_input:
        cases += [("preview", "refs/heads/feature/x", "feature/x"),
                  ("preview", "refs/heads/main", None),
                  ("preview", "refs/heads/x$(id)", None)]
    for env_input, ref, want in cases:
        rc, got = run_step(step, env_input, ref)
        what = f"{label}: environment={env_input} ref={ref}"
        if want is None:
            check(rc != 0 and got is None, f"{what} is refused (rc={rc}, BRANCH={got})")
        else:
            check(rc == 0 and got == want, f"{what} deploys as {want} (rc={rc}, BRANCH={got})")


found = 0
for path in sorted(glob.glob(os.path.join(sys.argv[1], "*.yml"))):
    wf = yaml.safe_load(open(path))
    trigger = wf.get(True) or wf.get("on") or {}
    inputs = ((trigger.get("workflow_dispatch") or {}) if isinstance(trigger, dict) else {}) or {}
    has_env_input = "environment" in (inputs.get("inputs") or {})
    for job_id, job in (wf.get("jobs") or {}).items():
        if any("pages deploy" in str((s.get("with") or {}).get("command", ""))
               for s in job.get("steps") or []):
            found += 1
            check_job(f"{os.path.basename(path)}:{job_id}", job, has_env_input)

if found == 0:
    print("pages-deploy-branch-test: no `pages deploy` job found — the scan did not run")
    sys.exit(2)
print(f"pages-deploy-branch-test: {found} deploy job(s), {passed} passed, {failed} failed")
sys.exit(1 if failed else 0)
PY
