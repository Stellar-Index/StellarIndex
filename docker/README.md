# Container packaging

One Dockerfile per binary — kept in-repo so a self-hoster can build
their own images on demand. **The release workflow no longer builds
or pushes these images to ghcr.io** (see the header comment in
`.github/workflows/release.yml` for the rationale — short version:
no consumer of the published images existed, multi-arch builds were
adding ~3-5 min of CI burn per release tag for zero return).
Decision: 2026-05-11, operator.

Local build (any one binary):

```sh
docker build -t stellarindex/stellarindex-api:local -f docker/stellarindex-api.Dockerfile .
```

All binaries (matches `make build-docker`):

```sh
make build-docker
```

If you want our team to start publishing images to ghcr.io again
(self-hosted Kubernetes / Docker Compose distribution), file an
issue or PR restoring the `containers:` job in
`.github/workflows/release.yml`. The git log under
`release: drop ghcr.io push` shows the exact block that was removed.

## Image shape

- **Builder stage** uses `golang:1.25-alpine` and runs the same
  `go build -trimpath -buildvcs=true -ldflags=...` invocation the
  release workflow does so the locally-built image and the
  CI-released one are byte-equivalent at the binary level. The
  Go major.minor must match `go.mod`'s `go` directive — F-1240
  (codex audit-2026-05-12) caught a previous drift where the
  Dockerfiles used `1.26-alpine` while `go.mod` and CI both used
  `1.25.x`, producing binaries that were not byte-identical to
  the release-channel artifacts. When `go.mod` bumps the `go`
  directive, update every Dockerfile in this directory in the
  same PR.
- **Runtime stage** uses `gcr.io/distroless/static-debian12:nonroot`
  — no shell, no package manager, runs as uid 65532. CA certs are
  baked in (needed for outbound HTTPS to CEX/FX vendors).
- Listening ports: API on 3000, indexer/aggregator metrics on 9464
  / 9465. Ops + migrate + sla-probe don't bind a port.

## Why distroless static (not alpine)

The Go binaries are statically linked (`CGO_ENABLED=0`), so the
runtime image needs nothing from the OS. Distroless's `static`
variant is ~2 MB vs Alpine's ~5 MB, has no shell (no
`exec`-into-prod attack surface), and gets the same OS-CVE
trickle from Google's distroless team that Alpine gets from
Alpine's security team.

The trade-off: no `apk add` / `bash` for "I want to debug this
container live". Acceptable because we have systemd-on-bare-metal
as the primary deploy target — containers are for portable
operator-side use (compose stacks, k8s if/when), not for live
debugging.

## Operator note: this is not the production deploy path

R1 today runs the binaries directly via systemd unit files (see
`/etc/systemd/system/stellarindex-*.service`). These container
images are for:

- Self-hosted operators wanting a docker-compose drop-in
- Future k8s deploys (post-multi-region)
- CI smoke tests of the full stack on tagged builds
