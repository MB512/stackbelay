# syntax=docker/dockerfile:1
#
# Release image: manager and worker in one image (the manager starts its
# workers from its own image). Build from the repository root:
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=v0.1.0 -t stackvault .
#
# Multi-platform builds (linux/amd64, linux/arm64) cross-compile the Go
# binaries on the build platform; only the small final stage runs per platform.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/MB512/stackbelay/internal/buildinfo.Version=${VERSION}" \
      -o /out/ ./cmd/manager ./cmd/worker

# docker CLI with the compose plugin (Alpine based); restic from Alpine.
FROM docker:29-cli
ARG VERSION=dev
RUN apk add --no-cache restic \
 && restic version && docker compose version
COPY --from=build /out/manager /out/worker /app/
ENV STACKVAULT_DATA_DIR=/data
EXPOSE 8080
LABEL org.opencontainers.image.title="StackVault" \
      org.opencontainers.image.description="Backs up Docker Compose stacks to restic repositories" \
      org.opencontainers.image.source="https://github.com/MB512/stackbelay" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
# The base image's entrypoint (docker-entrypoint.sh) is not wanted: workers
# are started as "/app/worker run /job" from this image without --entrypoint.
# The manager's health check (GET /healthz on STACKVAULT_LISTEN). Worker
# containers use the same image and are started with --no-healthcheck.
# A "healthcheck:" in a compose file still overrides this one.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/manager", "healthcheck"]
ENTRYPOINT []
CMD ["/app/manager"]
