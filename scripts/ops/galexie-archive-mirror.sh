#!/usr/bin/env bash
# galexie-archive-mirror.sh — off-site mirror of the Galexie archive
# (off-site-backup-plan.md §1, NS03).
#
# The archive (source of truth for a re-derive) had no off-site copy of any
# kind: only the ZFS local snapshot exists, which does nothing for a pool or
# box loss. This mirrors it to an R2 (or any S3-compatible) bucket via the
# MinIO client so a lost box restores from off-box in hours, not the
# 1-2 week re-ingest this doc's RTO table measures.
#
# Mechanism: `mc mirror` from the local galexie-archive bucket to a
# destination alias. `mc mirror` is incremental (only new/changed objects),
# so this is cheap to run often. Credentials/endpoint come from
# /etc/default/galexie-archive-mirror (a systemd EnvironmentFile, read
# VERBATIM by the unit — never sourced by this script) as an `mc alias set`
# the unit performs before invoking this script, or as MC_HOST_<alias> in
# the process environment; either way this script only ever sees DEST_ALIAS.
#
# Verification: a plain `mc mirror` can exit 0 while having copied nothing
# useful (e.g. it silently no-ops against a bucket it cannot list past page
# one). After the real mirror this runs `mc mirror --dry-run` against the
# same pair: a clean sync reports zero outstanding objects. The dry-run's
# exit status is captured SEPARATELY from its stdout — mc can exit non-zero
# (auth/network failure) while printing nothing, and treating empty stdout
# alone as "nothing left to copy" would stamp success on a dry-run that
# never actually ran. Success requires exit 0 AND empty stdout.
#
# Emits (node_exporter textfile collector):
#   stellarindex_galexie_archive_mirror_configured           1 iff DEST_ENDPOINT is set
#   stellarindex_galexie_archive_mirror_last_run_ok           1/0, every configured run
#   stellarindex_galexie_archive_mirror_last_success_timestamp  (clean, verified runs only)
# Alert: stellarindex_galexie_archive_mirror_stale (storage.yml, both trees).
#
# Exit code: 0 clean; 1 the mirror or its verification failed, or no
# DEST_ENDPOINT is configured.
set -uo pipefail

SOURCE_ALIAS="${SOURCE_ALIAS:-local/galexie-archive}"
DEST_ENDPOINT="${DEST_ENDPOINT:-}"
DEST_ALIAS="${DEST_ALIAS:-r2-archive}"
DEST_BUCKET="${DEST_BUCKET:-stellarindex-galexie-archive}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
MC_CMD="${MC_CMD:-mc}"

now="$(date -u +%s)"
run_ok=0

note() { echo "galexie-archive-mirror: $*" >&2; }

write_metrics() {
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return 0
  mkdir -p "$TEXTFILE_DIR"
  local out="$TEXTFILE_DIR/galexie_archive_mirror.prom" tmp
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_galexie_archive_mirror_configured 1 if an off-site mirror target (DEST_ENDPOINT) is configured on this host, else 0."
    echo "# TYPE stellarindex_galexie_archive_mirror_configured gauge"
    if [[ -n "$DEST_ENDPOINT" ]]; then
      echo "stellarindex_galexie_archive_mirror_configured 1"
    else
      echo "stellarindex_galexie_archive_mirror_configured 0"
    fi
    echo "# HELP stellarindex_galexie_archive_mirror_last_run_ok 1 if the most recent mirror+verify run succeeded, else 0."
    echo "# TYPE stellarindex_galexie_archive_mirror_last_run_ok gauge"
    echo "stellarindex_galexie_archive_mirror_last_run_ok $run_ok"
    if [[ "$run_ok" -eq 1 ]]; then
      echo "# HELP stellarindex_galexie_archive_mirror_last_success_timestamp Unix time of the most recent verified-clean mirror."
      echo "# TYPE stellarindex_galexie_archive_mirror_last_success_timestamp gauge"
      echo "stellarindex_galexie_archive_mirror_last_success_timestamp $now"
    fi
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
}

main() {
  if [[ -z "$DEST_ENDPOINT" ]]; then
    # A run that copied nothing must not read as a successful unit in
    # systemctl / journalctl; the gap shows as a failed unit, not a green one.
    note "no DEST_ENDPOINT configured — the archive has no off-site mirror on this host (stellarindex_galexie_archive_mirror_stale tickets it)"
    write_metrics
    return 1
  fi

  local dest="$DEST_ALIAS/$DEST_BUCKET"

  if ! $MC_CMD mirror --overwrite "$SOURCE_ALIAS" "$dest" >/dev/null; then
    note "mc mirror to $dest failed"
    write_metrics
    return 1
  fi

  local dry_out dry_rc
  dry_out="$($MC_CMD mirror --dry-run "$SOURCE_ALIAS" "$dest")"
  dry_rc=$?
  if [[ "$dry_rc" -ne 0 ]]; then
    note "post-mirror dry-run verification failed (rc=$dry_rc) — not stamping success"
    write_metrics
    return 1
  fi
  if [[ -n "$dry_out" ]]; then
    note "post-mirror dry-run still reports outstanding objects — mirror incomplete"
    write_metrics
    return 1
  fi

  run_ok=1
  note "mirror to $dest verified clean"
  write_metrics
  return 0
}

main "$@"
