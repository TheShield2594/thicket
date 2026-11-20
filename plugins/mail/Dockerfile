# The mail plugin for Thicket (JMAP, or IMAP with SMTP). Build from this folder:
#   docker build -t thicket-mail plugins/mail
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/mail .

FROM alpine:3.20
# The token file Thicket writes is readable by its group (gid 65532) only.
# ca-certificates: the plugin verifies every mail server's certificate.
RUN addgroup -g 65532 thicket && adduser -D -u 1000 plugin && addgroup plugin thicket \
 && apk add --no-cache curl ca-certificates \
 && mkdir -p /var/lib/thicket-mail && chown plugin:plugin /var/lib/thicket-mail && chmod 700 /var/lib/thicket-mail
COPY --from=build /out/mail /usr/local/bin/thicket-mail
USER plugin
ENV MAIL_ADDR=:7072 MAIL_TOKEN_FILE=/plugin-auth/token MAIL_STATE_DIR=/var/lib/thicket-mail
# Only the daily send counts live here; credentials are never written.
VOLUME /var/lib/thicket-mail
EXPOSE 7072
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD addr="${MAIL_ADDR:-:7072}"; curl -fs "http://127.0.0.1:${addr##*:}/health" || exit 1
ENTRYPOINT ["/usr/local/bin/thicket-mail"]
