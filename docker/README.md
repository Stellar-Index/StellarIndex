# Container packaging

## Local verifier image

`docker/verify/Dockerfile` is a development-only, multi-architecture image for
`make prepush`. It pins the Go, Node, pnpm, lint, vulnerability, monitoring and
Ansible toolchain used by the strict Linux lane. Docker builds its native
platform, so Apple Silicon uses `linux/arm64` without emulation.

Use `make prepush`, not a direct `docker run`: the wrapper verifies a clean
committed snapshot, preserves access to real Git history for secret scanning,
selects integration tests from the commit range and mounts persistent caches.
The image is not a production artifact and is not published.

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

- **Base images are pinned by digest** — every `FROM` in the six
  `stellarindex-*.Dockerfile`s reads `image:tag@sha256:…`. The tag
  is kept for readability; the digest is what the build pulls, so a
  re-tagged upstream image (or a compromised registry tag) cannot
  change what ships without a diff in this directory. Each pin is the
  multi-platform *index* digest (linux/amd64 + linux/arm64), resolved
  with `docker buildx imagetools inspect <image:tag>`; the comment
  above each `FROM` records the resolution date. Dependabot's docker
  ecosystem (`.github/dependabot.yml`, `directory: /docker`) opens
  the PR when the tag moves to a new digest. When refreshing by hand,
  update all six Dockerfiles in the same commit and keep the two
  digests identical across them.
- **Builder stage** uses `golang:<major.minor>-alpine` and runs the same
  `go build -trimpath -buildvcs=true -ldflags=...` invocation the
  release workflow does, so the locally-built image and the
  CI-released one match at the binary level apart from VCS stamping
  (see the build-context bullet below). The
  Go major.minor must match `go.mod`'s `go` directive — F-1240
  (codex audit-2026-05-12) caught a previous drift where the
  Dockerfiles used `1.26-alpine` while `go.mod` and CI both used
  `1.25.x`, producing binaries that were not byte-identical to
  the release-channel artifacts. When `go.mod` bumps the `go`
  directive, update every Dockerfile in this directory in the
  same PR.
- **Build context is an allowlist.** The repo-root `.dockerignore`
  sends only `go.mod`, `go.sum`, `cmd/`, `internal/`, `pkg/` and
  `migrations/`, minus secret-shaped files inside them, so the
  builder's `COPY . .` never receives `.git`, gitignored `.env` /
  vault / service-account files or `node_modules`. With no `.git` in
  the context `-buildvcs=true` stamps nothing: an image's
  `version.Commit` reads `unknown` and its identity is the `VERSION`
  build arg (`make build-docker` passes the Makefile's `VERSION`).
  A new build input outside those trees must be re-included there;
  `test/controlwiring/dockerignore_test.go` fails otherwise.
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
