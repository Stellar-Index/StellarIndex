#!/usr/bin/env bash
# resolve-pages-branch.sh — the `--branch` label for every workflow that runs
# `wrangler pages deploy`, exported to $GITHUB_ENV as BRANCH.
#
# Cloudflare Pages treats a deploy labelled with the project's production
# branch (`main`) as the production deployment, so this label IS the
# production switch and must agree with the ref actually checked out:
#   production  only from refs/heads/main (the checkout builds github.sha of
#               that ref); labelled `main`. Any other ref is refused rather
#               than built and published as main.
#   preview     labelled with the dispatching ref's name; `main` is refused,
#               because that label would publish production without the
#               production approval gate.
# The label reaches wrangler's command line, so it is allowlisted (F-004).
#
# Env: DEPLOY_ENVIRONMENT (production|preview), GITHUB_REF, GITHUB_REF_NAME,
# GITHUB_ENV — the last three are set by the Actions runner.
set -euo pipefail

: "${DEPLOY_ENVIRONMENT:?DEPLOY_ENVIRONMENT must be production or preview}"
: "${GITHUB_REF:?GITHUB_REF must be set}"
: "${GITHUB_REF_NAME:?GITHUB_REF_NAME must be set}"
: "${GITHUB_ENV:?GITHUB_ENV must be set}"

case "$DEPLOY_ENVIRONMENT" in
  production)
    if [ "$GITHUB_REF" != "refs/heads/main" ]; then
      echo "::error::Refusing a production deploy from '$GITHUB_REF': production publishes as branch 'main', so it must be dispatched from refs/heads/main (use environment=preview for any other ref)" >&2
      exit 1
    fi
    branch=main
    ;;
  preview)
    branch="$GITHUB_REF_NAME"
    if [ "$branch" = main ]; then
      echo "::error::Refusing a preview deploy labelled 'main': Cloudflare Pages publishes that label as production (use environment=production, which is gated)" >&2
      exit 1
    fi
    ;;
  *)
    echo "::error::Unknown deploy environment '$DEPLOY_ENVIRONMENT' (want production or preview)" >&2
    exit 1
    ;;
esac

# Whole-string match ([[ =~ ]], not line-oriented grep) so an embedded newline
# can't slip a second line past the allowlist and into $GITHUB_ENV.
if [[ ! "$branch" =~ ^[A-Za-z0-9._/-]+$ ]]; then
  echo "::error::Refusing to deploy: branch '$branch' has characters outside [A-Za-z0-9._/-]" >&2
  exit 1
fi
printf 'BRANCH=%s\n' "$branch" >> "$GITHUB_ENV"
echo "Cloudflare Pages branch: $branch"
