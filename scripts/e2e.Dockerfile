# Throwaway worker image for scripts/e2e-dispatch.sh. The release image
# (deploy/Dockerfile, M6) replaces it. Build context: a directory that
# contains a statically linked "worker" binary.
FROM docker:29-cli
RUN apk add --no-cache restic && restic version && docker compose version
COPY worker /app/worker
