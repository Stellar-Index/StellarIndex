#!/usr/bin/env bash
# Load fixture data into the local dev stack (`make dev`).
#
# "Fixture data" here is what the migrations themselves seed —
# migrations/0032_seed_soroswap_router, 0033_seed_defindex_vaults,
# 0104_seed_soroswap_aggregator_exec, and the schema every other
# local run needs — applied via the same stellarindex-migrate binary
# `make db-migrate-up` uses. No network access required: this only
# talks to the local Postgres/Timescale container from deploy/docker-
# compose/dev.yaml.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

: "${STELLARINDEX_POSTGRES_DSN:=postgres://${POSTGRES_USER:-stellarindex}:${POSTGRES_PASSWORD:-stellarindex-dev}@127.0.0.1:${POSTGRES_PORT:-5432}/${POSTGRES_DB:-stellarindex}?sslmode=disable}"
export STELLARINDEX_POSTGRES_DSN

echo "seed: applying migrations (incl. seed data) to $STELLARINDEX_POSTGRES_DSN"
go run ./cmd/stellarindex-migrate up
echo "seed: done — schema + migration-seeded rows (soroswap router, defindex vaults, soroswap aggregator exec) are loaded."
