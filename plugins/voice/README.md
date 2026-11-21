# thicket-voice

Thicket's local speech plugin: **whisper.cpp** hears voice messages and
**Piper** reads replies aloud, behind the OpenAI-compatible endpoints
Thicket already speaks. What it does for people is in
[docs/voice.md](../../docs/voice.md).

```
docker build -t thicket-voice plugins/voice
cd plugins/voice && go vet ./... && go test -race ./...
```

The Go server fronts three programs, run once per request in a temporary
folder: `ffmpeg` (recordings to 16 kHz WAV for whisper; Piper's WAV to
what the caller asked for), `whisper-cli` and `piper`. The tests replace
all three with shell scripts, so they run anywhere.

| Endpoint | Token | |
|---|---|---|
| `GET /health` | no | `{"status":"starting|ready|error","detail":…,"voices":[…]}` |
| `POST /v1/audio/transcriptions` | yes | multipart `file` (≤ 25 MB, any format ffmpeg reads), optional `language`, `response_format` `json` (default), `verbose_json` or `text`; `model` is ignored. Answers `{"text": …}` |
| `POST /v1/audio/speech` | yes | JSON `input` (≤ 4,096 characters), `voice` (a Piper voice in the models folder; anything else gets the default), `response_format` `mp3` (default), `opus` (Ogg), `aac`, `flac`, `wav` or `pcm` (24 kHz s16le), `speed` 0.25–4; `model` is ignored |
| `POST /mcp` | yes | MCP: `initialize`, `tools/list`, `tools/call` (`list_voices`, read-only) |

Until the models are in place both audio endpoints answer 503 with
"still fetching its models". Errors use OpenAI's shape
(`{"error":{"message":…}}`).

## Environment

| Env | Default | |
|---|---|---|
| `VOICE_ADDR` | `:7071` | listen address |
| `VOICE_TOKEN_FILE` | `/plugin-auth/token` | the token shared with Thicket, read on every request (shorter than 32 characters or missing: everything is refused) |
| `VOICE_MODELS_DIR` | `/models` | `whisper/` and `piper/` model files (a volume) |
| `VOICE_WHISPER_MODEL_URL` / `_SHA256` | ggml-base-q5_1 (pinned) | the whisper.cpp model; a URL without a checksum is used unchecked |
| `VOICE_PIPER_VOICE` | `en_US-lessac-medium` | the default voice: `piper/<name>.onnx` and `.onnx.json` |
| `VOICE_PIPER_VOICE_URL` / `_SHA256`, `VOICE_PIPER_CONFIG_URL` / `_SHA256` | lessac (pinned) | where another default voice's files come from |
| `VOICE_LANGUAGE` | `auto` | whisper's language |
| `VOICE_THREADS` | CPUs, at most 4 | whisper's threads |
| `VOICE_JOBS` | `1` | transcriptions or syntheses at once (others wait) |
| `VOICE_TIMEOUT_SECONDS` | `120` | for one program run |
| `VOICE_WHISPER_BIN`, `VOICE_PIPER_BIN`, `VOICE_FFMPEG_BIN` | as in the image | the programs |

## Pinned downloads

| File | Source | SHA-256 |
|---|---|---|
| `ggml-base-q5_1.bin` | huggingface.co/ggerganov/whisper.cpp @ `5359861c` | `422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898` |
| `en_US-lessac-medium.onnx` | huggingface.co/rhasspy/piper-voices @ `375a0fe6` (v1.0.0) | `5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f` |
| `en_US-lessac-medium.onnx.json` | same | `efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0` |
| Piper 2023.11.14-2, linux x86_64 (image build) | github.com/rhasspy/piper releases | `a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992` |
| Piper 2023.11.14-2, linux aarch64 (image build) | same | `fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb` |

whisper.cpp itself is built from source at tag `v1.7.6`
(`--build-arg WHISPER_VERSION=…` to change it) with `GGML_NATIVE=OFF`, so
the image runs on any CPU of its architecture.

## Layout

| File | |
|---|---|
| `main.go` | configuration, pinned models, start-up |
| `server.go` | routes, token check, the audio endpoints, MCP |
| `engine.go` | running ffmpeg, whisper-cli and piper; voices |
| `models.go` | downloading and checking model files |
