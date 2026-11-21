# The voice plugin for Thicket: whisper.cpp hears voice messages, Piper
# reads replies aloud. Build from this folder:
#   docker build -t thicket-voice plugins/voice
# Models are not in the image: they are downloaded on first start into
# /models (a volume) and checked against pinned SHA-256 sums.

# The Go server cross-compiles.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/voice .

# whisper.cpp, built for the target (generic CPU: no -march=native, so the
# image runs on any amd64 or arm64 machine, a Raspberry Pi 4/5 included).
FROM debian:bookworm-slim AS whisper
ARG WHISPER_VERSION=v1.7.6
RUN apt-get update && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "$WHISPER_VERSION" https://github.com/ggml-org/whisper.cpp /src/whisper
WORKDIR /src/whisper
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF \
      -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF \
 && cmake --build build --config Release -j"$(nproc)" --target whisper-cli \
 && install -m 0755 build/bin/whisper-cli /out-whisper-cli

# Piper's release binaries (with their own onnxruntime and espeak-ng
# data), checked against their SHA-256.
FROM debian:bookworm-slim AS piper
ARG TARGETARCH
ARG PIPER_VERSION=2023.11.14-2
ARG PIPER_SHA256_AMD64=a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992
ARG PIPER_SHA256_ARM64=fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN set -e; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sum="$PIPER_SHA256_AMD64" ;; \
      arm64) arch=aarch64; sum="$PIPER_SHA256_ARM64" ;; \
      *) echo "no Piper build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/piper.tgz "https://github.com/rhasspy/piper/releases/download/${PIPER_VERSION}/piper_linux_${arch}.tar.gz"; \
    echo "$sum  /tmp/piper.tgz" | sha256sum -c -; \
    mkdir -p /opt && tar -xzf /tmp/piper.tgz -C /opt && rm /tmp/piper.tgz

FROM debian:bookworm-slim
# ffmpeg converts recordings for whisper and Piper's WAV into what each
# chat app plays (Ogg Opus, MP3). libgomp1: whisper.cpp's threads.
# The token file Thicket writes is readable by its group (gid 65532) only.
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libgomp1 ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 65532 thicket && useradd -m -u 1000 -G thicket plugin \
 && mkdir -p /models && chown plugin:plugin /models
COPY --from=whisper /out-whisper-cli /usr/local/bin/whisper-cli
COPY --from=piper /opt/piper /opt/piper
COPY --from=build /out/voice /usr/local/bin/thicket-voice
USER plugin
ENV VOICE_ADDR=:7071 VOICE_TOKEN_FILE=/plugin-auth/token VOICE_MODELS_DIR=/models \
    VOICE_WHISPER_BIN=/usr/local/bin/whisper-cli VOICE_PIPER_BIN=/opt/piper/piper VOICE_FFMPEG_BIN=/usr/bin/ffmpeg
VOLUME /models
EXPOSE 7071
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD addr="${VOICE_ADDR:-:7071}"; curl -fs "http://127.0.0.1:${addr##*:}/health" || exit 1
ENTRYPOINT ["/usr/local/bin/thicket-voice"]
