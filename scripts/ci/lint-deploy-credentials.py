"""A job that holds a production credential must sit behind an environment gate.

The credentials that can change production (the r1 root SSH key, the
ansible vault, the r1 inventory, the account-level Cloudflare token) are
only as protected as the least-gated job that reads them. An approval rule
or branch policy on an environment binds nothing a job without
`environment:` does, and a repo-level secret is readable from any branch
by any workflow. So, for every job that references one of them:

  1. the job declares `environment:` — the precondition for scoping the
     secret to that environment instead of the repo;
  2. a step runs scripts/ci/assert-deploy-gate.sh BEFORE the first step
     that references the credential, and the credential is not exposed
     through job-level `env:` (which every step, including those before
     the gate, would see);
  3. no job passes `secrets: inherit` or serializes `secrets` wholesale,
     which would hand the credential on without either check.

Usage: python3 scripts/ci/lint-deploy-credentials.py [workflows-dir]
"""
import glob
import json
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("lint-deploy-credentials: FAIL — PyYAML not available; refusing to "
          "pass vacuously (install pyyaml)", file=sys.stderr)
    sys.exit(2)

CREDENTIALS = (
    "DEPLOY_SSH_PRIVATE_KEY",
    "ANSIBLE_VAULT_PASSWORD",
    "ANSIBLE_VAULT_FILE_B64",
    "R1_INVENTORY_B64",
    "CLOUDFLARE_API_TOKEN",
)
CRED_REF = re.compile(
    r"secrets\s*(?:\.\s*(?:%s)\b|\[\s*['\"](?:%s)['\"]\s*\])"
    % ("|".join(CREDENTIALS), "|".join(CREDENTIALS)))
WHOLESALE = re.compile(r"toJSON\s*\(\s*secrets\s*\)", re.I)
GATE = "scripts/ci/assert-deploy-gate.sh"


def text(node):
    return json.dumps(node, default=str)


def refs_credential(node):
    return bool(CRED_REF.search(text(node)))


def job_violations(name, job):
    if not isinstance(job, dict):
        return [], False
    out = []
    if job.get("secrets") == "inherit" or WHOLESALE.search(text(job)):
        out.append("%s: passes every secret on (`secrets: inherit` / "
                   "toJSON(secrets)); name the ones it needs" % name)
    if not refs_credential(job):
        return out, bool(out)
    if not job.get("environment"):
        out.append("%s: reads a production credential but declares no "
                   "`environment:`" % name)
    if refs_credential(job.get("env")) or refs_credential(job.get("container")) \
            or refs_credential(job.get("services")):
        out.append("%s: exposes a production credential at job level, ahead "
                   "of any gate step" % name)
    steps = job.get("steps") or []
    first_cred = next((i for i, st in enumerate(steps) if refs_credential(st)),
                      None)
    gate = next((i for i, st in enumerate(steps)
                 if isinstance(st, dict) and GATE in str(st.get("run") or "")),
                None)
    if gate is None:
        out.append("%s: reads a production credential but never runs %s"
                   % (name, GATE))
    elif first_cred is not None and gate > first_cred:
        out.append("%s: step %d reads a production credential before the "
                   "gate step %d" % (name, first_cred + 1, gate + 1))
    return out, True


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else ".github/workflows"
    files = sorted(glob.glob(os.path.join(root, "*.yml"))
                   + glob.glob(os.path.join(root, "*.yaml")))
    if not files:
        print("lint-deploy-credentials: FAIL — no workflows under %s" % root,
              file=sys.stderr)
        return 1
    violations, checked = [], 0
    for path in files:
        with open(path) as fh:
            wf = yaml.safe_load(fh)
        jobs = (wf or {}).get("jobs") or {}
        for name, job in jobs.items():
            found, holds = job_violations(
                "%s::%s" % (os.path.basename(path), name), job)
            violations += found
            checked += holds
    for v in violations:
        print("lint-deploy-credentials: " + v, file=sys.stderr)
    print("lint-deploy-credentials: %d workflow(s), %d credential-holding "
          "job(s) checked, %d violation(s)"
          % (len(files), checked, len(violations)))
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main())
