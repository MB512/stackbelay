# Throwaway image for the end-to-end scripts (manager + worker, restic, the
# docker CLI with compose). The release image (deploy/Dockerfile, M6)
# replaces it. Build context: a directory with statically linked "manager"
# and "worker" binaries.
FROM docker:29-cli
RUN apk add --no-cache restic && restic version && docker compose version
COPY manager worker /app/
ENTRYPOINT []
CMD ["/app/manager"]
