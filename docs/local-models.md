# Local models

Thicket can think with models on your own hardware instead of OpenRouter:
anything that speaks the OpenAI chat API, such as
[Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai),
llama.cpp's `llama-server` or vLLM. Your conversations are then
thought about on that server rather than sent to a model provider, and
there's no model bill (the spend caps simply stay at $0). Messages still
travel through the chat app you use (Discord or Telegram), and through any
integrations or web search you've turned on.

## Setting it up in chat

On the first DM, Juni asks for an OpenRouter key *or* the address of your
model server. Send the address, for example:

```
http://192.168.1.20:11434
```

or just "ollama" or "lm studio" to try the usual addresses. Thicket checks
the server, picks its first chat model (skipping embedding models), tells
you which others it found, and carries on with onboarding. To use another
model later, ask Juni: "use llama3.1:8b".

## Setting it up in the environment

```yaml
THICKET_MODEL_URL: http://host.docker.internal:11434/v1
THICKET_MODEL: qwen2.5:14b          # optional: otherwise Juni picks one on the first DM
THICKET_MODEL_API_KEY: ""            # only if your server wants a key (vLLM --api-key, a proxy)
```

`THICKET_CHEAP_MODEL` and `THICKET_FALLBACK_MODEL` default to OpenRouter
models, so set them too (to the same model is fine) if you set
`THICKET_MODEL` yourself. When Juni picks the model, she sets all three.

## Reaching the server from Docker

Inside the Thicket container, `localhost` is the container itself, not
your computer. Use one of:

- your computer's LAN address, like `http://192.168.1.20:11434`;
- `http://host.docker.internal:11434`, which the compose file maps to the
  host (`extra_hosts`);
- the service name, like `http://ollama:11434`, if Ollama runs in the same
  Compose project.

Ollama only listens on `127.0.0.1` by default. Start it with
`OLLAMA_HOST=0.0.0.0` so the container can reach it. In LM Studio, turn on
"Serve on local network".

## Choosing a model

Sprigs use tools to remember things, schedule jobs, hand over to each
other and use integrations, so pick a model that supports tool calling:
on Ollama, for example, `qwen2.5`, `llama3.1` or `mistral-nemo`. With a
model that can't use tools, sprigs still answer, but without those
abilities, and Juni tells you once a day which model is the problem.

Bigger is better for staying in character and following skills. On a CPU,
replies can take a while: Thicket waits up to 5 minutes per model call.

## Differences from OpenRouter

- Thicket reads a PDF's text itself and hands it to the model (the first
  12,000 characters with that message; `document_chars` of it is kept for
  follow-ups). That covers PDFs with a text layer, which is most PDFs made
  on a computer. Scanned PDFs have no text to read without OCR, which is
  an OpenRouter service, so sprigs say they can't read those. Neither
  can encrypted PDFs or ones in unusual compressions be read. Images work if the model supports vision (e.g. `llava`, `qwen2.5vl`).
- There's no fallback model: if the server is down, sprigs tell you it
  failed.
- Spend is recorded as $0.

## Switching

To move to OpenRouter later, paste a key to Juni and ask her to set
`model_url` back to `https://openrouter.ai/api/v1` (and the models to
OpenRouter ids). To move a running OpenRouter setup to a local server, ask
her to set `model_url` to your server's address and `model`,
`cheap_model` and `fallback_model` to one of its models.
