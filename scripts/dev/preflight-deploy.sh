#!/usr/bin/env bash
# preflight-deploy.sh — answer, on this machine and in one run, every
# question .github/workflows/deploy.yml is about to ask.
#
# Four failed deploys on 2026-09-07 were all knowable before the dispatch:
# two config-apply-gate reds whose surface diffs turned out to be
# comment-only (learned twice at the cost of a failed run each; the gate
# now answers that case itself), one six-binary set dispatched at
# a region that runs five (futurenet has no aggregator unit; the health
# probe failed and the binary rolled back), and one four-of-six set that
# left `stellarindex-migrate` and `stellarindex-sla-probe` behind and
# tripped the `stellarindex_binary_version_skew` probe.
#
# Reported here, before any of that costs a round trip:
#
#   1. Config surfaces changed since the version LIVE ON THE TARGET, each
#      classified comment-only, already-applied, or substantive. Both the
#      surface list and the classifier are READ from
#      scripts/ci/config-apply-gate.sh (--payload / --ddl-objects) so there
#      is one copy of each, and that gate is then executed for its own
#      verdict rather than re-implemented. "Already applied" is not
#      assumed: for a ClickHouse DDL diff that adds only whole new
#      statements, the target is asked whether every object it creates is
#      present in system.tables.
#   2. The binary set the target region actually runs, from a checked-in
#      manifest derived from the hosts (scripts/dev/region-binaries.tsv).
#      There is no per-region manifest anywhere else — deploy.yml carries
#      one default list for three regions.
#   3. Per-binary skew: the version live on the target against the release.
#   4. Migrations in the range, with the CS-099 caveat.
#
# The last line of output is the exact `gh workflow run deploy.yml` command
# for that region. It never carries `-f config_acknowledged=true`: both
# cases that flag used to cover are now cleared by machine, so what is left
# is an operator asserting something no checker can read, which this script
# has no basis to assert for them. Exit is non-zero whenever an operator
# decision is outstanding.
#
# Usage:
#   scripts/dev/preflight-deploy.sh --region r1 --version v0.63.0
#   make preflight-deploy REGION=r1 VERSION=v0.63.0
#
#   --no-host           make no ssh connection. The baseline then falls back
#                       to the previous release tag by ancestry, exactly as
#                       the gate does unaided, and the report says so.
#   --refresh-manifest  re-derive this region's binary set from the host and
#                       rewrite its manifest row (commit the result).
#   --migrations-ack    record that the range's migrations have been read for
#                       old-binary compatibility (CS-099).
#
# Everything this reads from a host is read-only: `cat` of the deploy
# sidecars, `systemctl is-enabled` / `is-active`, and a `test -x`. Nothing
# is written, restarted or deployed.
#
# See docs/operations/local-preflight.md.

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

MANIFEST="scripts/dev/region-binaries.tsv"
GATE="scripts/ci/config-apply-gate.sh"
DEPLOY_PLAYBOOK="configs/ansible/playbooks/deploy-binary.yml"
DEPLOY_WORKFLOW=".github/workflows/deploy.yml"
SSH_TIMEOUT="${PREFLIGHT_SSH_TIMEOUT:-15}"

REGION=""
VERSION=""
NO_HOST=0
REFRESH=0
MIGRATIONS_ACK=0

# Findings that require an operator decision before the dispatch. Anything
# appended here makes the exit non-zero; the recommended command is still
# printed, because knowing the command and knowing it is not yet safe are
# separate facts.
BLOCKERS=()
NOTES=()

usage() {
    cat >&2 <<'USAGE'
usage: scripts/dev/preflight-deploy.sh --region <r1|testnet|futurenet> --version vX.Y.Z
                                       [--no-host] [--refresh-manifest] [--migrations-ack]
       make preflight-deploy REGION=r1 VERSION=vX.Y.Z

exit 0  ready — the printed dispatch is warranted by the evidence above it
exit 1  an operator decision is outstanding (each one is listed)
exit 2  usage error, or an input that does not resolve in this checkout
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --region)  REGION="${2:-}"; shift 2 || exit 2 ;;
        --version) VERSION="${2:-}"; shift 2 || exit 2 ;;
        --no-host) NO_HOST=1; shift ;;
        --refresh-manifest) REFRESH=1; shift ;;
        --migrations-ack) MIGRATIONS_ACK=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "preflight-deploy: unknown argument '$1'" >&2; usage; exit 2 ;;
    esac
done

[ -n "$REGION" ] && [ -n "$VERSION" ] || { usage; exit 2; }

rule() { printf '\n── %s\n' "$1"; }
block() { BLOCKERS+=("$1"); }
note()  { NOTES+=("$1"); }

# ── 0. Inputs ────────────────────────────────────────────────────────────
#
# The same SemVer shape deploy.yml's "Validate inputs" step enforces, so a
# tag this refuses is a tag that workflow would refuse too.
if ! grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?(\+[A-Za-z0-9.-]+)?$' <<<"$VERSION"; then
    echo "preflight-deploy: version '$VERSION' must match SemVer vX.Y.Z[-prerelease][+build]" >&2
    exit 2
fi
case "$REGION" in
    r1|testnet|futurenet) ;;
    *) echo "preflight-deploy: region '$REGION' has no inventory mapping in $DEPLOY_WORKFLOW" >&2; exit 2 ;;
esac
if ! git rev-parse -q --verify "refs/tags/${VERSION}" >/dev/null; then
    {
        echo "preflight-deploy: '$VERSION' is not a tag in this checkout."
        echo "  Pass a released tag:  make preflight-deploy REGION=${REGION} VERSION=vX.Y.Z"
        echo "  With no VERSION= the Makefile substitutes 'git describe', which is not one."
        echo "  If the tag is simply not fetched yet:  git fetch --tags origin"
    } >&2
    exit 2
fi
for required in "$MANIFEST" "$GATE" "$DEPLOY_PLAYBOOK" "$DEPLOY_WORKFLOW"; do
    [ -f "$required" ] || { echo "preflight-deploy: required input '$required' is missing" >&2; exit 2; }
done

echo "preflight-deploy  region=${REGION}  version=${VERSION}  checkout=$(git rev-parse --short=12 HEAD)"

# ── 1. Release artefacts ─────────────────────────────────────────────────
#
# deploy.yml downloads the binaries from the GitHub Release for the tag. A
# tag that exists in git but has no release (or no assets) fails the deploy
# at the download step, several minutes in.
rule "1. release artefacts"
if command -v gh >/dev/null 2>&1; then
    assets="$(gh release view "$VERSION" --json assets --jq '.assets | length' 2>/dev/null)"
    if [ -z "$assets" ]; then
        echo "  no GitHub Release for ${VERSION} readable here (offline, unauthenticated, or not published)"
        note "release ${VERSION} could not be read; deploy.yml downloads its artefacts and will fail if it is absent"
    elif [ "$assets" -eq 0 ]; then
        echo "  release ${VERSION} exists but carries NO assets"
        block "release ${VERSION} has no assets — deploy.yml has nothing to download"
    else
        echo "  release ${VERSION}: ${assets} asset(s) published"
    fi
else
    echo "  gh is not installed — release existence unverified"
    note "gh is not installed; the release for ${VERSION} was not checked"
fi

# ── 2. Region binary manifest ────────────────────────────────────────────
#
# Which binaries a region runs is a property of the HOST, not of the repo:
# a daemon binary is deployable there only if its systemd unit exists and is
# enabled, because deploy-one-binary.yml restarts the unit and then probes
# it. The CLI binaries (deploy-binary.yml's cli_binaries deny-list) have no
# unit to restart — they get a stat smoke — so their test is installation.
#
# The manifest caches that derivation so a routine run needs no host at all.
rule "2. region binary set"

# The deny-list is deploy-binary.yml's, read rather than copied.
cli_binaries="$(awk '
    /^ *cli_binaries:/ { grab = 1; next }
    grab && /^ *- / { sub(/^ *- */, ""); print; next }
    grab { exit }
' "$DEPLOY_PLAYBOOK")"

# Deploy ORDER is deploy.yml's default list; only the ordering is taken from
# it, never the membership.
canonical_order="$(grep -oE "default: 'stellarindex-[a-z,-]+'" "$DEPLOY_WORKFLOW")"
canonical_order="${canonical_order#*\'}"
canonical_order="${canonical_order%\'*}"
canonical_order="$(tr ',' ' ' <<<"$canonical_order")"
[ -n "$canonical_order" ] || canonical_order="stellarindex-indexer stellarindex-aggregator stellarindex-api stellarindex-sla-probe stellarindex-ops stellarindex-migrate"

manifest_row() { awk -F'\t' -v r="$1" '$1 == r { print; rows++ } END { exit rows ? 0 : 1 }' "$MANIFEST"; }

row="$(manifest_row "$REGION")"
if [ -z "$row" ]; then
    echo "preflight-deploy: region '$REGION' has no row in $MANIFEST — re-derive it with --refresh-manifest" >&2
    exit 2
fi
IFS=$'\t' read -r _ HOST SSH_USER JUMP DEPLOY_SET EXCLUDED DERIVED_UTC <<<"$row"
[ "$JUMP" = "-" ] && JUMP=""
[ "$EXCLUDED" = "-" ] && EXCLUDED=""

# The test-net VMs are NAT-only and reached through the KVM host, exactly as
# deploy.yml's region step does it. Two spellings rather than an array,
# because bash 3.2 (the /bin/bash a Mac ships) treats an empty "${a[@]}"
# as an unbound variable under set -u.
ssh_read() { # ssh_read <remote-sh-snippet>
    if [ -n "$JUMP" ]; then
        ssh -o BatchMode=yes -o ConnectTimeout="$SSH_TIMEOUT" \
            -o "ProxyJump=${JUMP}" "${SSH_USER}@${HOST}" "$1"
    else
        ssh -o BatchMode=yes -o ConnectTimeout="$SSH_TIMEOUT" \
            "${SSH_USER}@${HOST}" "$1"
    fi
}

# ch_objects_present <newline-separated db.object list> — ask the target's
# ClickHouse which of those objects exist. Prints "all" or the missing
# ones; returns non-zero when the question could not be ASKED (no client,
# host unreachable, server down). A question that could not be asked must
# never read as an answer, so the caller then leaves the surface
# substantive rather than claiming it applied.
#
# The whole database.name list is fetched and compared here rather than
# interpolated into an IN(...) clause: nothing derived from a diff reaches
# the query text.
ch_objects_present() {
    local objects="$1" existing missing="" obj
    existing="$(ssh_read "command -v clickhouse-client >/dev/null 2>&1 || exit 9; clickhouse-client --port \"\${CH_PORT:-9300}\" -q \"SELECT concat(database, '.', name) FROM system.tables\"")" || return 1
    [ -n "$existing" ] || return 1
    while IFS= read -r obj; do
        [ -n "$obj" ] || continue
        grep -qxF -- "$obj" <<<"$existing" || missing="${missing}${obj} "
    done <<<"$objects"
    if [ -n "$missing" ]; then printf '%s' "${missing% }"; else printf 'all'; fi
    return 0
}

# Remote derivation: unit enablement for the daemons, installation for the
# CLIs. One connection, three `systemctl` reads and three `test -x`.
derive_remote_script() {
    local daemons="" clis="" b
    for b in $canonical_order; do
        if grep -qxF "$b" <<<"$cli_binaries"; then clis="$clis $b"; else daemons="$daemons $b"; fi
    done
    cat <<REMOTE
for b in ${daemons}; do
  e=\$(systemctl is-enabled "\$b.service" 2>/dev/null); [ -n "\$e" ] || e=not-found
  a=\$(systemctl is-active "\$b.service" 2>/dev/null); [ -n "\$a" ] || a=unknown
  printf '%s\tservice\t%s/%s\n' "\$b" "\$e" "\$a"
done
for b in ${clis}; do
  if [ -x "/usr/local/bin/\$b" ]; then printf '%s\tcli\tinstalled\n' "\$b"
  else printf '%s\tcli\tabsent\n' "\$b"; fi
done
REMOTE
}

# Deployability of a daemon, from its <is-enabled>/<is-active> pair.
#
# deploy-one-binary.yml restarts <binary>.service and then requires
# `systemctl is-active` to report active, so a unit systemd does not have —
# or refuses to start — is a failed deploy and a rolled-back binary. That is
# futurenet's aggregator: no unit at all, and the residue of the attempt is
# still on the host as stellarindex-aggregator.failed-v0.62.0.
#
# Enablement alone is the wrong test, and testnet is the counter-example:
# its aggregator is UnitFileState=disabled and ActiveState=active — off at
# boot, running now, and deployed there at v0.62.0 by this very workflow.
# Excluding it would leave it behind and re-create the skew this script
# exists to prevent. What is refused is a unit that is absent or masked, or
# one that is switched off AND not running: restarting that starts a daemon
# somebody deliberately stopped.
unit_deployable() { # unit_deployable <is-enabled>/<is-active>
    local enabled="${1%%/*}" active="${1##*/}"
    case "$enabled" in
        not-found|bad|masked|masked-runtime|"") return 1 ;;
    esac
    [ "$enabled" = disabled ] && [ "$active" != active ] && return 1
    return 0
}

host_derived=""
host_excluded=""
host_states=""
if [ "$NO_HOST" -eq 0 ]; then
    if host_states="$(ssh_read "$(derive_remote_script)")" && [ -n "$host_states" ]; then
        while IFS=$'\t' read -r bin kind state; do
            [ -n "$bin" ] || continue
            if [ "$kind" = cli ]; then
                if [ "$state" = installed ]; then host_derived="$host_derived $bin"
                else host_excluded="${host_excluded}${bin}:not-installed "; fi
            elif unit_deployable "$state"; then
                host_derived="$host_derived $bin"
            else
                # No spaces in the reason: host_excluded is a
                # space-separated list that becomes a comma-separated
                # manifest field, and a space would split one entry into two.
                case "${state%%/*}" in
                    not-found|bad|masked|masked-runtime) reason="unit-${state%%/*}" ;;
                    *) reason="unit-off-$(tr '/' '-' <<<"$state")" ;;
                esac
                host_excluded="${host_excluded}${bin}:${reason} "
            fi
        done <<<"$host_states"
        host_derived="${host_derived# }"
        host_excluded="${host_excluded% }"
    else
        echo "  host ${HOST} unreachable — using the manifest row derived ${DERIVED_UTC}"
        note "host ${HOST} was unreachable, so the binary set comes from the manifest cache, not from the host"
        host_states=""
    fi
fi

if [ "$REFRESH" -eq 1 ]; then
    if [ -z "$host_derived" ]; then
        echo "preflight-deploy: --refresh-manifest needs the host, and ${HOST} did not answer" >&2
        exit 2
    fi
    new_set="$(tr ' ' ',' <<<"$host_derived")"
    new_excluded="${host_excluded:--}"
    new_excluded="$(tr ' ' ',' <<<"$new_excluded")"
    stamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    tmp="$(mktemp)"
    awk -F'\t' -v OFS='\t' -v r="$REGION" -v s="$new_set" -v x="$new_excluded" -v t="$stamp" '
        $1 == r { $5 = s; $6 = x; $7 = t } { print }
    ' "$MANIFEST" >"$tmp" && mv "$tmp" "$MANIFEST"
    echo "  manifest row for ${REGION} refreshed from ${HOST} at ${stamp}"
    DEPLOY_SET="$new_set"
    EXCLUDED="$new_excluded"
    [ "$EXCLUDED" = "-" ] && EXCLUDED=""
    DERIVED_UTC="$stamp"
fi

manifest_set="$(tr ',' ' ' <<<"$DEPLOY_SET")"
echo "  manifest (${DERIVED_UTC}): $(tr ' ' ',' <<<"$manifest_set")"
if [ -n "$host_states" ]; then
    printf '  host %s:\n' "$HOST"
    printf '%s\n' "$host_states" | sed 's/^/      /'
    if [ "$host_derived" != "$manifest_set" ]; then
        echo "  MANIFEST DRIFT: host derives '$(tr ' ' ',' <<<"$host_derived")'"
        echo "  The host wins — the dispatch below names the host-derived set."
        block "the manifest row for ${REGION} disagrees with the host — re-run with --refresh-manifest and commit the row"
    fi
    # The cache is a convenience; the host is the fact. Everything downstream
    # (the sidecar read, the dispatch line) uses what the host reported, so a
    # stale row can never be the thing that gets dispatched.
    manifest_set="$host_derived"
    [ -n "$host_excluded" ] && EXCLUDED="$(tr ' ' ',' <<<"$host_excluded")"
fi
if [ -n "$EXCLUDED" ]; then
    echo "  not deployable at ${REGION}: $(tr ',' ' ' <<<"$EXCLUDED")"
fi

# Every entry must still be a real cmd/ directory, which is deploy.yml's own
# validation: a manifest row naming a retired binary fails the dispatch.
for b in $manifest_set; do
    [ -d "cmd/$b" ] || block "manifest names '${b}', which has no cmd/${b}/ directory — deploy.yml rejects it"
done

# ── 3. Live versions and binary skew ─────────────────────────────────────
#
# The baseline is computed exactly as deploy.yml's "Capture the host's live
# version" step computes it: the LOWEST SemVer across the managed binaries,
# with stellarindex-migrate excluded because it legitimately lags (#427).
rule "3. live versions on ${REGION}"
live_pairs=()
rollback_count=0
BASELINE=""
baseline_source=""
if [ "$NO_HOST" -eq 0 ]; then
    sidecar_script="d=/var/lib/stellarindex/deployed-versions
for b in ${manifest_set}; do
  if [ -f \"\$d/\$b\" ]; then printf '%s\t' \"\$b\"; awk 1 \"\$d/\$b\" || exit 3
  else printf '%s\t-\n' \"\$b\"; fi
done"
    if sidecars="$(ssh_read "$sidecar_script")" && [ -n "$sidecars" ]; then
        candidates=""
        while IFS=$'\t' read -r bin ver; do
            [ -n "$bin" ] || continue
            live_pairs+=("${bin}=${ver}")
            [ "$bin" = stellarindex-migrate ] && continue
            grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' <<<"$ver" && candidates="${candidates}${ver}"$'\n'
        done <<<"$(tr -d '\r' <<<"$sidecars")"
        if [ -n "$candidates" ]; then
            # printf '%s', not a here-string: <<< appends a newline of its
            # own, and the resulting blank line sorts FIRST under -V, so the
            # baseline came back empty and silently fell through to ancestry.
            sorted="$(printf '%s' "$candidates" | sort -V)"
            BASELINE="$(sed -n '1p' <<<"$sorted")"
            baseline_source="host sidecars (lowest across binaries, migrate excluded)"
        fi
    else
        block "could not read the deployed-versions sidecars on ${HOST}; deploy.yml FAILS CLOSED on exactly this read rather than falling back"
    fi
fi

if [ ${#live_pairs[@]} -gt 0 ]; then
    for pair in "${live_pairs[@]}"; do
        bin="${pair%%=*}"; ver="${pair#*=}"
        if [ "$ver" = "$VERSION" ]; then verdict="at ${VERSION}"
        elif [ "$ver" = "-" ]; then verdict="NO SIDECAR — never deployed here"
        elif ! grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+' <<<"$ver"; then verdict="NOT A RELEASE TAG — deployed off a working tree"
        else
            older="$(sort -V <<<"${ver}"$'\n'"${VERSION}" | sed -n '1p')"
            if [ "$older" = "$ver" ]; then
                verdict="behind"
            else
                verdict="AHEAD of ${VERSION} — this dispatch is a rollback"
                rollback_count=$((rollback_count + 1))
            fi
        fi
        printf '  %-26s %-34s %s\n' "$bin" "$ver" "$verdict"
    done
    behind_count=0
    for pair in "${live_pairs[@]}"; do
        [ "${pair#*=}" = "$VERSION" ] || behind_count=$((behind_count + 1))
    done
    if [ "$behind_count" -gt 0 ]; then
        echo "  ${behind_count} of ${#live_pairs[@]} binaries are not on ${VERSION}."
    fi
    if [ "$rollback_count" -gt 0 ]; then
        block "${rollback_count} binary/binaries on ${REGION} are AHEAD of ${VERSION}: this dispatch is a ROLLBACK. Migrations do not roll back with it (CS-099) — follow docs/operations/rollback.md rather than dispatching a deploy"
    fi
    echo "  The dispatch below names the region's WHOLE set, so none is left behind:"
    echo "  a partial list is what leaves stellarindex_binary_version_skew non-zero."
elif [ "$NO_HOST" -eq 1 ]; then
    echo "  skipped (--no-host)"
fi

if [ -z "$BASELINE" ]; then
    BASELINE="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null)"
    baseline_source="previous release tag by ancestry — correct ONLY if the target is exactly one release behind"
    if [ -z "$BASELINE" ]; then
        echo "preflight-deploy: no release tag precedes ${VERSION}; there is no baseline to diff against" >&2
        exit 2
    fi
    [ "$NO_HOST" -eq 1 ] && note "baseline is the ancestry fallback because --no-host was passed; deploy.yml will read the host and may use a different one"
fi
echo "  baseline: ${BASELINE}  (${baseline_source})"

# ── 4. Config surfaces ───────────────────────────────────────────────────
#
# The surface list is READ from the gate, and the gate is then RUN, so this
# section reports the verdict deploy.yml will reach rather than a second
# opinion about it. What is added on top is the comment-only
# classification, which is the part that decides whether
# config_acknowledged=true is honest or a rubber stamp.
rule "4. config surfaces ${BASELINE} → ${VERSION}"
SURFACES=()
while IFS= read -r surface; do
    [ -n "$surface" ] && SURFACES+=("$surface")
done <<<"$(awk '
    /^SURFACES=\(/ { grab = 1; next }
    grab && /^\)/ { grab = 0; next }
    grab {
        sub(/#.*/, ""); gsub(/^[ \t]+|[ \t]+$/, ""); gsub(/'\''/, "")
        if ($0 != "") print
    }
' "$GATE")"
if [ ${#SURFACES[@]} -eq 0 ]; then
    echo "preflight-deploy: could not read the SURFACES list out of ${GATE}" >&2
    exit 2
fi

changed="$(git diff --name-only "$BASELINE" "$VERSION" -- "${SURFACES[@]}")"

# Comment-only classification, and — for ClickHouse DDL — whether the
# substantive half is ALREADY APPLIED on this host.
#
# Both judgements come from the gate (--payload / --ddl-objects) rather
# than from a second implementation here: since 2026-09-07 the gate PASSES
# a comment-only surface itself, so a local classifier that disagreed with
# it would mispredict the verdict this section exists to preview.
#
# The three cases the gate distinguishes are reported here with the same
# names it uses.
substantive=0
comment_only=0
applied_verified=0
# Paths proven applied on the host — handed to the gate below as its
# "[applied]" argument, exactly as deploy.yml hands it the surfaces its own
# steps applied and verified.
VERIFIED_PATHS=""
if [ -z "$changed" ]; then
    echo "  no config-surface changes — config_acknowledged is not needed"
else
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        payload="$(bash "$GATE" --payload "$BASELINE" "$VERSION" "$f")"
        if [ -z "$payload" ]; then
            printf '  %-12s %s\n' "comment-only" "$f"
            comment_only=$((comment_only + 1))
            continue
        fi
        # Substantive. For a ClickHouse DDL diff that adds only whole new
        # statements the host can be ASKED whether the objects exist —
        # which is the difference between "already applied" and
        # "unapplied", and the difference the 2026-09-07 acknowledgement
        # got right only by luck. Nothing equivalent exists for a systemd
        # unit or an ansible template: those stay substantive.
        if objects="$(bash "$GATE" --ddl-objects "$BASELINE" "$VERSION" "$f")" && [ -n "$objects" ]; then
            if [ "$NO_HOST" -eq 0 ] && present="$(ch_objects_present "$objects")"; then
                if [ "$present" = "all" ]; then
                    printf '  %-12s %s\n' "applied" "$f  (objects present on ${HOST}: $(tr '\n' ' ' <<<"$objects"))"
                    applied_verified=$((applied_verified + 1))
                    VERIFIED_PATHS="${VERIFIED_PATHS}${f} "
                    continue
                fi
                printf '  %-12s %s\n' "SUBSTANTIVE" "$f  (NOT applied on ${HOST}: ${present})"
                substantive=$((substantive + 1))
                continue
            fi
            printf '  %-12s %s\n' "SUBSTANTIVE" "$f  (creates $(tr '\n' ' ' <<<"$objects")— existence unchecked)"
            substantive=$((substantive + 1))
            continue
        fi
        printf '  %-12s %s\n' "SUBSTANTIVE" "$f"
        sed -n '1,4p' <<<"$payload" | sed 's/^/                 | /'
        substantive=$((substantive + 1))
    done <<<"$changed"
fi

# Run the gate with exactly the evidence deploy.yml will hand it, so what
# follows is that job's verdict rather than a second opinion about it.
gate_out="$(bash "$GATE" "$VERSION" false "$BASELINE" "$VERIFIED_PATHS" 2>&1)"
gate_rc=$?

printf '  gate verdict: %s (exit %d)\n' \
    "$([ "$gate_rc" -eq 0 ] && echo 'passes without acknowledgement' || echo 'FAILS without config_acknowledged=true')" "$gate_rc"
# The gate's own words, verbatim. It prints the baseline it used and the
# ::error:: line the deploy job will show, and reprinting them here is what
# makes this section a preview of that job rather than a second opinion.
printf '%s\n' "$gate_out" | sed 's/^/      | /'

# The dispatch printed at the end NEVER carries -f config_acknowledged=true.
# Both cases where an acknowledgement is warranted by evidence are now
# cleared by machine — comment-only by the gate, applied ClickHouse DDL by
# the object check above — so the flag has exactly one remaining meaning:
# an operator asserting, from their own knowledge, that a surface no
# checker can read is applied. This script has no basis to assert that on
# their behalf, and the rule it used to apply ("all comment-only, so
# acknowledge") is the rule that would have acknowledged v0.61.1..v0.62.0's
# unapplied CREATE TABLE.
if [ "$comment_only" -gt 0 ]; then
    note "the ${comment_only} comment-only surface file(s) still differ TEXTUALLY from the host copies until applied; the weekly ansible-drift job will report them"
fi
if [ "$applied_verified" -gt 0 ]; then
    echo "  ${applied_verified} substantive surface(s) are already applied on ${HOST} — every object their diff creates is present in system.tables."
fi
if [ "$gate_rc" -ne 0 ]; then
    echo "  ${substantive} surface(s) carry real changes nothing has proven applied: apply them per"
    echo "  docs/operations/deploy-config-apply.md, verify each landed on the host, and re-run this preflight."
    block "${substantive} config surface(s) changed substantively between ${BASELINE} and ${VERSION} and are not proven applied — acknowledging without verifying is what ships the feature dead"
fi

# ── 5. Migrations in the range ───────────────────────────────────────────
#
# CS-099 is deliberate policy: deploy-binary.yml runs `migrate up` in
# pre_tasks, BEFORE any binary swap, and a failed health probe rolls back
# the BINARY ONLY. The schema stays forward. So a release carrying
# migrations has to be read for old-binary compatibility before it is
# dispatched, not after.
rule "5. migrations in ${BASELINE} → ${VERSION}"
migrations="$(git diff --name-only "$BASELINE" "$VERSION" -- 'migrations/*.up.sql')"
if [ -z "$migrations" ]; then
    echo "  none — the schema is unchanged across this range"
else
    printf '%s\n' "$migrations" | sed 's/^/  /'
    echo "  CS-099: these apply BEFORE the binary swap and are NOT rolled back if a"
    echo "  health probe fails. Every one must leave ${BASELINE}'s binaries working"
    echo "  against the new schema."
    compat_dir="$(mktemp -d)"
    if git archive "$VERSION" migrations | tar -x -C "$compat_dir" 2>/dev/null; then
        compat_out="$(MIGRATIONS_DIR="${compat_dir}/migrations" bash scripts/ci/lint-migration-compat.sh --staged 2>&1)"
        compat_rc=$?
        if [ "$compat_rc" -eq 0 ]; then
            echo "  lint-migration-compat (--staged, ${VERSION}): clean — no mechanically-detectable"
            echo "  rule-9 violation. The judgement half is still yours."
        else
            printf '%s\n' "$compat_out" | sed 's/^/  /'
            block "lint-migration-compat fails over ${VERSION}'s migrations — deploy.yml runs the same gate before 'migrate up'"
        fi
    else
        note "could not materialise ${VERSION}'s migrations/ for the compat gate"
    fi
    rm -rf "$compat_dir"
    if [ "$MIGRATIONS_ACK" -eq 0 ]; then
        block "$(printf '%s\n' "$migrations" | grep -c .) migration(s) in this range have not been acknowledged — read them for old-binary compatibility, then re-run with --migrations-ack"
    else
        echo "  --migrations-ack: recorded as read for old-binary compatibility."
    fi
fi

# ── 6. Verdict ───────────────────────────────────────────────────────────
rule "6. verdict"
if [ ${#NOTES[@]} -gt 0 ]; then
    for n in "${NOTES[@]}"; do echo "  note: $n"; done
fi
if [ ${#BLOCKERS[@]} -gt 0 ]; then
    for b in "${BLOCKERS[@]}"; do echo "  DECISION NEEDED: $b"; done
else
    echo "  nothing outstanding."
fi

binaries_csv="$(tr ' ' ',' <<<"$manifest_set")"
echo ""
echo "gh workflow run deploy.yml \\"
echo "  -f region=${REGION} \\"
echo "  -f version=${VERSION} \\"
echo "  -f binaries=${binaries_csv}"

[ ${#BLOCKERS[@]} -eq 0 ] || exit 1
exit 0
