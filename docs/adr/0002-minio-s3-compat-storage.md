---
adr: 0002
title: Self-hosted storage is S3-compatible (MinIO), not local filesystem
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0002: Self-hosted storage is S3-compatible (MinIO), not local filesystem

## Context

Galexie's datastore backends are GCS, S3-compatible, and local filesystem. The filesystem backend silently
drops all nine metadata keys on each object and is unsafe for concurrent writers; SDF marks it dev-only.

## Decision

Self-hosted deployments use an S3-compatible object store, MinIO by default, through the Galexie S3
backend with `endpoint_url` overridden. MinIO runs erasure-coded on our own hardware as the primary
tier, with async replication to a cloud bucket for backup and disaster recovery. Third-party
operators may use any S3-compatible backend (AWS S3, GCS, R2, B2, Wasabi). The filesystem backend is
allowed only in local development, documented as dev-only in `deploy/docker-compose/`.

## Invariant

Galexie never writes through its local filesystem backend outside local development, so all nine
metadata keys survive on every object (AGENTS.md invariant 3, enforced in review).

Consumers read the lake through the SDK `datastore` abstraction, so the backend is swappable by
config, not code.

## Consequences

Metadata is preserved, writes are safe across processes, and any machine can reach the lake. The
cost is a MinIO to deploy and monitor (capacity, heal state, replication lag). Backup and DR rest on
S3 semantics (versioning, lifecycle, replication). `rs-stellar-archivist` only reads S3, so archives
are mirrored to local disk then synced into MinIO.

## Evidence

Dev-only filesystem use: `deploy/docker-compose/`. Rule cited in AGENTS.md invariant 3.
