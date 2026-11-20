# The hello plugin, a template for Thicket plugins. Build from this folder:
#   docker build -t thicket-hello plugins/hello
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/hello .

FROM alpine:3.20
# The token file Thicket writes is readable by its group (gid 65532) only.
RUN addgroup -g 65532 thicket && adduser -D -u 1000 plugin && addgroup plugin thicket && apk add --no-cache curl
COPY --from=build /out/hello /usr/local/bin/thicket-hello
USER plugin
ENV HELLO_ADDR=:7071 HELLO_TOKEN_FILE=/plugin-auth/token
EXPOSE 7071
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD addr="${HELLO_ADDR:-:7071}"; curl -fs "http://127.0.0.1:${addr##*:}/health" || exit 1
ENTRYPOINT ["/usr/local/bin/thicket-hello"]
