# The sandbox: sprigs' own Linux computer. A small Go server (speaking MCP
# to Thicket) on Debian with everyday tools. Build from the repo root:
#   docker build -f sandbox/Dockerfile -t thicket-sandbox .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/thicket-sandbox ./cmd/thicket-sandbox

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates curl wget git jq less nano procps file tree \
      zip unzip xz-utils sqlite3 python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash sprig \
    && mkdir -p /workspace && chown sprig:sprig /workspace
COPY --from=build /out/thicket-sandbox /usr/local/bin/thicket-sandbox
USER sprig
WORKDIR /workspace
# pip installs go to the user's own site-packages.
ENV PIP_BREAK_SYSTEM_PACKAGES=1 PIP_USER=1 PATH=/home/sprig/.local/bin:$PATH \
    SANDBOX_WORKDIR=/workspace SANDBOX_TOKEN_FILE=/sandbox-auth/token SANDBOX_ADDR=:7070
VOLUME ["/workspace", "/home/sprig"]
EXPOSE 7070
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD curl -fs http://127.0.0.1:7070/health || exit 1
ENTRYPOINT ["/usr/local/bin/thicket-sandbox"]
