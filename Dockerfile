# syntax=docker/dockerfile:1.28

FROM node:24.22.1-slim AS frontend
WORKDIR /build/app
# Corepack resolves pnpm from the packageManager field of app/package.json.
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable
COPY app/package.json app/pnpm-lock.yaml app/pnpm-workspace.yaml app/.npmrc ./
COPY app/scripts/install-deps.mjs ./scripts/install-deps.mjs
COPY app/third_party ./third_party
RUN pnpm install --frozen-lockfile
COPY VERSION /build/VERSION
COPY app/ .
ENV PUBLIC_KOMBIFY_EDITION=selfhost-oss
RUN pnpm build

FROM golang:1.27.4-alpine AS backend
WORKDIR /build
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=frontend /build/app/build-static ./internal/frontend/dist
ARG HOMELAB_PRODUCT_VERSION=""
ARG GIT_COMMIT=""
RUN set -eu; \
    version="${HOMELAB_PRODUCT_VERSION:-$(tr -d '[:space:]' < VERSION)}"; \
    printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; \
    revision="${GIT_COMMIT:-dev}"; \
    if [ "$revision" != dev ]; then printf '%s' "$revision" | grep -Eq '^[0-9a-f]{40}$'; fi; \
    CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath \
      -tags homelab_static_ui \
      -ldflags="-s -w -X main.version=${version} -X main.buildRevision=${revision}" \
      -o /out/homelab ./cmd/homelab; \
    CGO_ENABLED=0 go build -mod=readonly -buildvcs=false -trimpath \
      -o /out/guard-release-tool ./cmd/guard-release-tool

# Pre-sign source CI stops at backend. Only the numeric product release builds
# this complete runtime, after Guard has been published from that public main SHA.
FROM alpine:3.24 AS signed-runtime
RUN apk add --no-cache bash ca-certificates curl jq tar
WORKDIR /build
COPY VERSION ./VERSION
COPY --from=backend /out/guard-release-tool /usr/local/bin/guard-release-tool
COPY scripts/new-linux-stackkit-spec-templates.sh ./scripts/new-linux-stackkit-spec-templates.sh
COPY docs/examples/seeds ./docs/examples/seeds
COPY Dockerfile ./Dockerfile
ARG STACKKIT_RELEASE_TAG="v0.61.5"
ARG STACKKIT_RELEASE_KIT="cloud-kit"
ARG STACKKIT_RELEASE_BOOTSTRAP_KIT="basement-kit"
ARG STACKKIT_RELEASE_REPO="kombifyio/StackKits"
ARG STACKKIT_RELEASE_ARCHIVE_SHA256="c2c9c4e6826aa786e7577d3e08a8da9ed2e21ce6c83d31f04493a77c11f29248"
ARG STACKKIT_RELEASE_INDEX_SHA256="489c1ea06c7e968bf649cbdda1615f90ec01d08f46c2ee95621f6630bc076acd"
ARG STACKKIT_RELEASE_COMPATIBILITY_SHA256="328f07ce00fd6e9dac10fbdff5b308b0c02d25e64ceef062c6b20e64cc5907b7"
ARG STACKKIT_RELEASE_USE_CASE_CATALOG_SHA256="b35aad5fed287e88cb7ea61e244d7cba468e5a2e07dfa9248944377b574c7740"
ARG GIT_COMMIT=""
ARG HOMELAB_PRODUCT_VERSION=""
RUN set -eu; \
    version="${HOMELAB_PRODUCT_VERSION:-$(tr -d '[:space:]' < VERSION)}"; \
    test "$version" = "$(tr -d '[:space:]' < VERSION)"; \
    printf '%s' "$GIT_COMMIT" | grep -Eq '^[0-9a-f]{40}$'; \
    base="https://github.com/kombifyio/Homelab/releases/download/guard-v${version}"; \
    mkdir -p /app/guard-release; \
    for asset in release-manifest.json release-manifest.sigstore.json \
      homelab-linux-amd64 homelab-linux-amd64.sigstore.json \
      homelab-linux-arm64 homelab-linux-arm64.sigstore.json \
      stackkit-runtime-linux-amd64.tar.gz stackkit-runtime-linux-arm64.tar.gz; do \
      curl --fail --silent --show-error --location "${base}/${asset}" --output "/app/guard-release/${asset}"; \
    done; \
    guard-release-tool verify /app/guard-release --expected-source "$GIT_COMMIT" --require-runtime-bundles; \
    jq -e --arg version "$version" --arg stackkits "$STACKKIT_RELEASE_TAG" \
      '.version == $version and .stackkits.version == $stackkits' /app/guard-release/release-manifest.json >/dev/null; \
    tar --extract --gzip --file /app/guard-release/stackkit-runtime-linux-amd64.tar.gz --directory /app; \
    test "$(jq -er '.version' /app/.stackkit/stackkits-release-pin.json)" = "$STACKKIT_RELEASE_TAG"

# Catalog and authoring templates belong to the controller. They never modify
# the immutable signed archive/tree served to a Guard.
RUN set -eu; \
    base="https://github.com/${STACKKIT_RELEASE_REPO}/releases/download/${STACKKIT_RELEASE_TAG}"; \
    curl --fail --silent --show-error --location "${base}/stackkits-release-index-v1.json" --output /tmp/index.json; \
    echo "${STACKKIT_RELEASE_INDEX_SHA256}  /tmp/index.json" | sha256sum -c -; \
    mkdir -p /app/stackkits; \
    for kit in basement-kit cloud-kit modern-homelab; do \
      entry="$(jq -er --arg kit "$kit" --arg version "$STACKKIT_RELEASE_TAG" \
        '[.assets[] | select(.kit == $kit and .version == $version and .platform.os == "linux" and .platform.arch == "amd64") | .archive] | if length == 1 then .[0].name + " " + .[0].sha256 else error("ambiguous or missing archive") end' /tmp/index.json)"; \
      set -- $entry; \
      curl --fail --silent --show-error --location "${base}/$1" --output /tmp/catalog.tar.gz; \
      echo "$2  /tmp/catalog.tar.gz" | sha256sum -c -; \
      tar --extract --gzip --file /tmp/catalog.tar.gz --directory /app/stackkits \
        --exclude=stackkit --exclude=stackkit-server --exclude=stackkit-mcp --exclude=stackkit-content-bridge \
        --exclude=tofu --exclude=terramate --exclude=providers; \
      test -s "/app/stackkits/$kit/stackkit.yaml"; \
    done; \
    bash scripts/new-linux-stackkit-spec-templates.sh; \
    cp -R dist/spec-templates /app/stackkits/spec-templates

FROM alpine:3.24
ARG GIT_COMMIT=""
ARG HOMELAB_PRODUCT_VERSION=""
LABEL org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.version="${HOMELAB_PRODUCT_VERSION}" \
      org.opencontainers.image.source="https://github.com/kombifyio/Homelab"
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates curl docker-cli docker-cli-compose git jq openssh-client
RUN addgroup -S homelab && adduser -S -G homelab homelab && mkdir -p /data && chown homelab:homelab /data
COPY --from=backend /out/homelab /app/homelab
COPY install.sh /app/install.sh
COPY install.ps1 /app/install.ps1
COPY --from=signed-runtime /app/.stackkit /app/.stackkit
COPY --from=signed-runtime /app/stackkits /app/stackkits
COPY --from=signed-runtime /app/guard-release /app/guard-release
ENV KOMBIFY_EDITION=selfhost-oss
ENV HOMELAB_AUTH_MODE=local
ENV HOMELAB_DATA_DIR=/data
ENV HOMELAB_STACKKITS_DIR=/app/stackkits
ENV HOMELAB_STACKKIT_CLI=/app/.stackkit/bin/stackkit
ENV HOMELAB_STACKKIT_ARTIFACTS_MODE=cli
ENV HOMELAB_STACKKIT_RELEASE_CACHE=/app/.stackkit/releases
ENV HOMELAB_STACKKIT_RELEASE_PIN=/app/.stackkit/stackkits-release-pin.json
ENV HOMELAB_STACKKIT_COMPATIBILITY_MANIFEST=/app/.stackkit/stackkits-compatibility-v1.json
ENV HOMELAB_STACKKIT_USE_CASE_CATALOG=/app/.stackkit/stackkits-use-case-catalog-v1.json
ENV HOMELAB_STACKKIT_SPEC_TEMPLATES=/app/stackkits/spec-templates
ENV HOMELAB_STACKKIT_RELEASE_BUNDLE=/app/guard-release/stackkit-runtime-linux-amd64.tar.gz
ENV HOMELAB_GUARD_RELEASE_DIR=/app/guard-release
ENV HOMELAB_GUARD_RELEASE_SOURCE=kombifyio/Homelab
ENV PORT=5260
ENV HOMELAB_PORT=5260
EXPOSE 5260 5263
WORKDIR /app
VOLUME ["/data"]
USER homelab
ENTRYPOINT ["/app/homelab"]
