# syntax=docker/dockerfile:1
FROM node:22-bookworm-slim AS node

FROM golang:1.25.13-trixie

ARG TARGETARCH
ARG GOFUMPT_VERSION=v0.8.0
ARG GOIMPORTS_VERSION=v0.42.0
ARG GOLANGCI_LINT_VERSION=v2.11.4
ARG GOVULNCHECK_VERSION=v1.1.4
ARG GITLEAKS_VERSION=8.21.2
ARG ZIZMOR_VERSION=1.30.0
ARG ZIZMOR_AMD64_SHA256=ec8c95cd800845abb9bbc5f377ec7c57d2eb8e2386a00a201d3a74ee4092e5ed
ARG ZIZMOR_ARM64_SHA256=018a024d6b6d09733b07f6ef42838d984c23ec04bc9b2acd55f7d67826aeafe5
ARG PROM_VERSION=2.55.1
ARG ALERTMANAGER_VERSION=0.28.1
ARG OPENAPI_TO_POSTMAN_VERSION=6.0.1

ENV PATH="/root/.local/bin:/go/bin:/pnpm:${PATH}" \
    PIPX_HOME=/root/.local/pipx \
    PIPX_BIN_DIR=/root/.local/bin \
    PNPM_HOME=/pnpm

COPY --from=node /usr/local/ /usr/local/

RUN go install mvdan.cc/gofumpt@${GOFUMPT_VERSION} \
    && go install golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION} \
    && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} \
    && go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq make openssh-client pipx python3 python3-jinja2 python3-yaml rsync shellcheck xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && corepack enable pnpm \
    && corepack prepare pnpm@10.33.0 --activate \
    && npm install --global "openapi-to-postmanv2@${OPENAPI_TO_POSTMAN_VERSION}"

RUN set -eux; \
    case "${TARGETARCH}" in amd64|arm64) arch="${TARGETARCH}" ;; *) echo "unsupported architecture: ${TARGETARCH}"; exit 1 ;; esac; \
    curl -fsSLO "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${arch}.tar.gz"; \
    curl -fsSLO "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_checksums.txt"; \
    grep "  gitleaks_${GITLEAKS_VERSION}_linux_${arch}.tar.gz$" "gitleaks_${GITLEAKS_VERSION}_checksums.txt" | sha256sum -c -; \
    tar -xzf "gitleaks_${GITLEAKS_VERSION}_linux_${arch}.tar.gz" -C /usr/local/bin gitleaks; \
    case "${arch}" in amd64) zizmor_arch=x86_64; zizmor_sha="${ZIZMOR_AMD64_SHA256}" ;; arm64) zizmor_arch=aarch64; zizmor_sha="${ZIZMOR_ARM64_SHA256}" ;; esac; \
    curl -fsSLO "https://github.com/zizmorcore/zizmor/releases/download/v${ZIZMOR_VERSION}/zizmor-${zizmor_arch}-unknown-linux-gnu.tar.gz"; \
    echo "${zizmor_sha}  zizmor-${zizmor_arch}-unknown-linux-gnu.tar.gz" | sha256sum -c -; \
    tar -xzf "zizmor-${zizmor_arch}-unknown-linux-gnu.tar.gz" -C /usr/local/bin zizmor; \
    rm -f ./*.tar.gz ./*checksums.txt

RUN set -eux; \
    case "${TARGETARCH}" in amd64) prom_arch=amd64 ;; arm64) prom_arch=arm64 ;; *) exit 1 ;; esac; \
    curl -fsSLO "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/prometheus-${PROM_VERSION}.linux-${prom_arch}.tar.gz"; \
    curl -fsSLO "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/sha256sums.txt"; \
    grep "  prometheus-${PROM_VERSION}.linux-${prom_arch}.tar.gz$" sha256sums.txt | sha256sum -c -; \
    tar -xzf "prometheus-${PROM_VERSION}.linux-${prom_arch}.tar.gz" --strip-components=1 -C /usr/local/bin "prometheus-${PROM_VERSION}.linux-${prom_arch}/promtool"; \
    curl -fsSLO "https://github.com/prometheus/alertmanager/releases/download/v${ALERTMANAGER_VERSION}/alertmanager-${ALERTMANAGER_VERSION}.linux-${prom_arch}.tar.gz"; \
    curl -fsSLO "https://github.com/prometheus/alertmanager/releases/download/v${ALERTMANAGER_VERSION}/sha256sums.txt"; \
    grep "  alertmanager-${ALERTMANAGER_VERSION}.linux-${prom_arch}.tar.gz$" sha256sums.txt | sha256sum -c -; \
    tar -xzf "alertmanager-${ALERTMANAGER_VERSION}.linux-${prom_arch}.tar.gz" --strip-components=1 -C /usr/local/bin "alertmanager-${ALERTMANAGER_VERSION}.linux-${prom_arch}/amtool"; \
    rm -f ./*.tar.gz ./sha256sums.txt

RUN pipx install --include-deps ansible==14.2.0 \
    && pipx install ansible-lint==26.6.0

COPY entrypoint.sh /usr/local/bin/stellarindex-verify
RUN chmod +x /usr/local/bin/stellarindex-verify
ENTRYPOINT ["stellarindex-verify"]
