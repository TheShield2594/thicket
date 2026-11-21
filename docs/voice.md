# Voice: hearing and speaking on your own machine

Sprigs hear voice messages (Telegram voice notes, Discord audio files,
the web app's recorder) and can answer out loud (see
[Speaking out loud](characters.md#speaking-out-loud)). Both need a
speech engine. The optional **voice plugin** runs them on your own
hardware:

- **whisper.cpp** turns voice messages into text (speech-to-text);
- **Piper** reads replies aloud (text-to-speech).

Nothing you say leaves the machine for either, there is nothing to pay,
and neither needs an OpenRouter key: with the voice plugin and a
[local model server](local-models.md), a voice message on Telegram or in
the web app is heard, answered and spoken without any cloud service.

## Turning it on

```
docker compose --profile voice up -d
```

That's all on a new install. The first start downloads the models (about
130 MB) into the `voice-models` volume and checks each one against a
pinned SHA-256 sum; until then the plugin answers "still fetching its
models" and a voice message gets a polite "couldn't transcribe it".
`docker compose logs voice` shows the progress.

Thicket finds the plugin at `http://voice:7071` (or `THICKET_VOICE_URL`)
within a minute of starting and, while `local_voice` is on, uses it for
both voice messages and spoken replies. `thicket doctor` checks it:

```
plugin.voice  ok  http://voice:7071, token accepted, version 0.1.0, 1 tools; round trip in 1.4s (speech 0.4s, transcription 1.0s)
```

The round trip says a short phrase with Piper and has whisper.cpp hear it
back. If the words come back different, the line says what was heard
(advice, not a failure: the small model can mishear).

## Which engine is used

For **voice messages**, in order:

1. `transcribe_url`, if set: any OpenAI-compatible speech-to-text server
   (`POST {url}/audio/transcriptions`), such as whisper.cpp's own server,
   Speaches or LocalAI. The model name sent is `transcribe_model` if you
   changed it, otherwise `whisper-1`.
2. The voice plugin, while it's running and `local_voice` is on.
3. OpenRouter, with `transcribe_model` (a model that hears audio). Other
   model servers can't hear audio, so with neither of the above the sprig
   is told the message wasn't transcribed and asks you to type it.

For **spoken replies**, in order:

1. `speech_url`, if set (for example a Kokoro-FastAPI server).
2. The voice plugin, while it's running and `local_voice` is on.
3. The model server (`model_url`), which is OpenRouter's speech models by
   default.

Local transcription and speech are recorded in usage like any call, at
$0. They still count as calls for the kill switch and spend caps: when
Thicket is paused, nothing is heard or spoken.

## New installs and upgrades

`local_voice` is on for new installs. On the first start of the version
that added it, Thicket looks at its database: if anyone was already set up
there, the install is an upgrade, and `local_voice` is stored **off** so
nothing about how it hears and speaks changes. That decision is made once
(the marker is the `voice.local_default_decided` setting); after that the
setting is yours.

To switch an upgraded install to the voice plugin, start it and tell Juni:

```
set local_voice to true
```

(or `thicket settings set local_voice true`). Cloud stays available:
`set local_voice to false` goes back to OpenRouter for voice messages and
the model server for speech, and `transcribe_url` / `speech_url` always
win over both.

## Voices

Piper speaks in the plugin's default voice, US English "lessac"
(`en_US-lessac-medium`). OpenAI voice names such as `alloy` or `nova`
(the `speech_voice` default, and what character files may name) don't
exist in Piper, so they get the default voice. To give sprigs other
voices, put more Piper voices in the models volume, each a
`<name>.onnx` with its `<name>.onnx.json` (from
[rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices)), under
`piper/`, and name them in a character file (`voice.sounds_like:
en_GB-alan-medium`) or in `speech_voice`. The plugin's `list_voices` tool
lists what it has.

To change the default voice, set `VOICE_PIPER_VOICE` on the voice service
to its name, with `VOICE_PIPER_VOICE_URL`, `VOICE_PIPER_VOICE_SHA256`,
`VOICE_PIPER_CONFIG_URL` and `VOICE_PIPER_CONFIG_SHA256` so it is
downloaded and checked (or put the files in the volume yourself and leave
the URLs empty).

whisper.cpp hears any language its model knows (the default model is the
multilingual `base`, quantised). Set `VOICE_LANGUAGE` (e.g. `de`) to stop
it guessing. For better accuracy at the cost of speed, point
`VOICE_WHISPER_MODEL_URL` and `VOICE_WHISPER_MODEL_SHA256` at a bigger
model from [ggerganov/whisper.cpp](https://huggingface.co/ggerganov/whisper.cpp)
(`ggml-small.bin`, `ggml-medium-q5_0.bin`); the SHA-256 of each is on
its file page.

## Kokoro instead of Piper

Kokoro sounds more natural than Piper and wants more CPU (or a GPU). The
voice plugin doesn't bundle it: run
[Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI) beside
Thicket and set `speech_url` to it (for example
`http://kokoro:8880/v1`), `speech_model` to `kokoro` and `speech_voice`
to one of its voices (`af_heart`). The voice plugin keeps hearing voice
messages.

## Security

- The plugin publishes no port: only containers in the compose project
  reach it, and every endpoint but `/health` needs the token Thicket
  writes to `THICKET_VOICE_TOKEN_FILE` (the `voice-auth` volume, mounted
  read-only in the plugin), like the [sandbox](sandbox.md).
- It runs as a non-root user with no capabilities and keeps nothing but
  the models: recordings and audio live in a temporary folder for the
  length of one request.
- Model files are used only if their SHA-256 matches. A file that
  doesn't match is downloaded again, never used. A download address you
  set without a checksum is used unchecked, and the log says so.
- It needs the internet only to download models. Once they're in the
  volume you can take it off the default network.

## Settings

| Setting | Default | |
|---|---|---|
| `local_voice` | `true` (new installs) | use the voice plugin while it runs and `transcribe_url` / `speech_url` are empty |
| `transcribe_url` | | an OpenAI-compatible speech-to-text server, e.g. `http://192.168.1.20:7071/v1` |
| `transcribe_model` | `google/gemini-3.8-flash` | OpenRouter's model for voice messages; with `transcribe_url`, the model name sent if changed |
| `speech_url`, `speech_model`, `speech_voice` | | see [Speaking out loud](characters.md#speaking-out-loud) |

The voice service's own variables are in
[plugins/voice/README.md](../plugins/voice/README.md).

## Hardware

whisper.cpp's base model transcribes a short voice note in a second or
two on a recent x86 CPU or a Raspberry Pi 5, and in a few seconds on a
Pi 4. Piper speaks faster than real time on all of them. The service is
limited to 1.5 GB of memory in the compose file; bigger whisper models
need more.
