# judge-worker: static Go binary, no shell, non-root (§17.2). Build context = repo root:
#   docker build -f images/judge-worker/Dockerfile --build-arg VERSION=$(git describe) .
FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/judge-worker ./cmd/judge-worker
RUN mkdir -p /out/var/lib/judge-worker/cache /out/var/lib/judge-worker/spool \
             /out/var/lib/judge-worker/scratch /out/var/lib/judge-worker/state

# distroless nonroot = UID/GID 65532
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/judge-worker /usr/local/bin/judge-worker
COPY --from=build --chown=65532:65532 /out/var/lib/judge-worker /var/lib/judge-worker
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/judge-worker"]
CMD ["-config", "/etc/judge-worker/config.yaml"]
