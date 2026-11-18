# Build a static binary, then ship it on distroless (no shell, no package
# manager, runs as non-root UID 65532).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN mkdir -p /out/data /out/sandbox-auth && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/thicket ./cmd/thicket

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/thicket /usr/local/bin/thicket
# Create /data owned by the nonroot user so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
# Where the token shared with the sandbox container is written.
COPY --from=build --chown=65532:65532 /out/sandbox-auth /sandbox-auth
ENV THICKET_DATA_DIR=/data \
    THICKET_HTTP_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/usr/local/bin/thicket", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/thicket"]
CMD ["serve"]
