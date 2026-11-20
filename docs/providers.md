# Model providers and subscriptions

Thicket thinks with models on one of three kinds of service:

- **OpenRouter** (the default): one key, hundreds of models, and extras
  only OpenRouter has (PDF parsing and OCR, voice transcription, web
  search, per-call costs).
- **A hosted provider** from the list below: a pay-as-you-go API or a
  flat-rate subscription.
- **Your own server**: Ollama, LM Studio, llama.cpp, vLLM
  ([Local models](local-models.md)).

## The providers

| Provider | `THICKET_PROVIDER` | Billing | Key looks like |
| --- | --- | --- | --- |
| [OpenCode Zen](https://opencode.ai/docs/zen/) | `opencode-zen` | pay as you go | send with the name |
| [OpenCode Go](https://opencode.ai/go) | `opencode-go` | subscription | send with the name |
| [Nous Portal](https://portal.nousresearch.com) (Nous Research) | `nous` | subscription or credits | send with the name |
| [OpenAI](https://platform.openai.com/api-keys) | `openai` | pay as you go | send with the name |
| [Anthropic](https://console.anthropic.com/settings/keys) | `anthropic` | pay as you go | `sk-ant-…` |
| [Google Gemini](https://aistudio.google.com/apikey) | `gemini` | pay as you go, free tier | `AIza…` |
| [xAI (Grok)](https://console.x.ai) | `xai` | pay as you go | `xai-…` |
| [Mistral](https://console.mistral.ai/api-keys) | `mistral` | pay as you go, free tier | send with the name |
| [DeepSeek](https://platform.deepseek.com/api_keys) | `deepseek` | pay as you go | send with the name |
| [Groq](https://console.groq.com/keys) | `groq` | pay as you go, free tier | `gsk_…` |
| [Cerebras](https://cloud.cerebras.ai) | `cerebras` | pay as you go, free tier | `csk-…` |
| [Together AI](https://api.together.ai/settings/api-keys) | `together` | pay as you go | send with the name |
| [Fireworks AI](https://fireworks.ai/account/api-keys) | `fireworks` | pay as you go | `fw_…` |
| [DeepInfra](https://deepinfra.com/dash/api_keys) | `deepinfra` | pay as you go | send with the name |
| [Hugging Face](https://huggingface.co/settings/tokens) | `huggingface` | pay as you go, free credits | `hf_…` |
| [NVIDIA NIM](https://build.nvidia.com) | `nvidia` | free credits | `nvapi-…` |
| [Moonshot AI (Kimi)](https://platform.moonshot.ai) | `moonshot` | pay as you go | send with the name |
| [Z.ai](https://z.ai) | `zai` | pay as you go | send with the name |
| [Z.ai GLM Coding Plan](https://z.ai/subscribe) | `zai-coding` | subscription | send with the name |
| [Chutes](https://chutes.ai) | `chutes` | subscription | `cpk_…` |
| [Synthetic](https://synthetic.new) | `synthetic` | subscription | send with the name |
| [Venice AI](https://venice.ai) | `venice` | pay as you go | send with the name |

Anything else that speaks the OpenAI chat API works too: set `model_url`
(or `THICKET_MODEL_URL`) to its address and `THICKET_MODEL_API_KEY` to its
key, as for a [local server](local-models.md).

## Setting one up

**In chat.** DM Juni the key. A key only one provider uses (like Groq's
`gsk_…`) is recognised on its own; for the rest, send the provider's name
with it:

```
opencode go sk-…
nous sk-…
deepseek sk-…
```

Juni deletes the message if the chat app allows it, checks the key by
listing the provider's models, picks a main model and a cheaper one for
background work (memory review, personality checks), and tells you which.
A key with no name and no telling shape is never tried on several
services in turn: that would hand your key to the wrong companies. Ask
"use <model>" later to switch models, or send another provider's key to
switch providers.

**With `thicket init`.** Choose 3 at the model question, then the provider,
then paste the key; it lists the models and suggests one.

**In the environment.**

```yaml
THICKET_PROVIDER: groq
THICKET_MODEL_API_KEY: gsk_…
THICKET_MODEL: openai/gpt-oss-120b   # optional: otherwise Thicket picks one
```

`thicket init --from-env` checks and stores them; otherwise Juni picks the
models on the first DM.

## Spend caps

OpenRouter reports what each call cost. Other providers don't, so:

- On a **subscription** (OpenCode Go, Nous Portal's plan, the GLM Coding
  Plan, Chutes, Synthetic) each call counts as $0. The plan's own limits
  apply instead.
- On a **pay-as-you-go** provider, tell Juni its prices for your model and
  the caps count it: "set model_usd_per_mtok_in to 0.15 and
  model_usd_per_mtok_out to 0.6" (dollars per million prompt and reply
  tokens). Until you do, its calls count as $0, so the caps don't protect
  you; set a spend limit in the provider's own console too.

## What differs from OpenRouter

- PDFs are read the way they are for a local server: Thicket extracts the
  text itself, so scanned PDFs can't be read.
- Voice messages aren't transcribed (that uses OpenRouter's audio input).
- Web search needs your own SearXNG (`search_url`); OpenRouter's search
  isn't available.
- The fallback model is on the same provider.
- Model ids are the provider's own (`gpt-oss-120b`, not
  `openai/gpt-oss-120b` on OpenRouter), and models come and go: Thicket
  picks from what the provider lists when you connect.

Notes on particular providers:

- **OpenCode Go** serves some models only through Anthropic's or OpenAI's
  newer APIs. Thicket speaks chat completions, so use the GLM, Kimi or
  Grok models there.
- **Nous Portal**: Hermes models are good conversationalists; for sprigs
  that use many tools, prefer the largest one.
- **Z.ai**: Coding Plan keys only work on the Coding Plan endpoint, so
  pick `zai-coding` for them and `zai` for pay-as-you-go keys.
- **Anthropic** and **Gemini** are reached through their
  OpenAI-compatible endpoints.
