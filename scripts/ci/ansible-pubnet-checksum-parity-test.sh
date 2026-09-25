#!/usr/bin/env bash
# ansible-pubnet-checksum-parity-test.sh (Q255) — every pubnet region
# example inventory must set the supply-chain checksums the
# archival-node role hard-asserts on (09-minio.yml: minio_release_sha256,
# mc_release_sha256; 16-prometheus-exporters.yml: pgbackrest_exporter_
# release_sha256). testnet.example.yml / futurenet.example.yml already
# carried them; r1/r2/r3.example.yml silently omitted them, so an
# operator copying r1.example.yml -> r1.yml only discovers the missing
# vars when the role fails fast mid-apply.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1

VARS=(minio_release_sha256 mc_release_sha256 pgbackrest_exporter_release_sha256)
FILES=(
  configs/ansible/inventory/r1.example.yml
  configs/ansible/inventory/r2.example.yml
  configs/ansible/inventory/r3.example.yml
)

failed=0
for f in "${FILES[@]}"; do
  for v in "${VARS[@]}"; do
    line=$(grep -E "^\s*${v}:\s*\"[0-9a-f]{64}\"" "$f" || true)
    if [ -z "$line" ]; then
      echo "  FAIL — $f: ${v} not set to a 64-char hex sha256" >&2
      failed=1
    else
      echo "  ok — $f: ${v} set"
    fi
  done
done

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "ansible-pubnet-checksum-parity-test: PASS"
