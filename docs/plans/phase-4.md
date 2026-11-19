# Phase 4 plan: new abilities

Status: **planned, not started.** This file is the plan; the work is tracked
in GitHub issues labelled `phase-4`, one per section below (the tracking
issue links them all).

## Gate

Nothing here starts until:

1. Phase 3 is released: a `vX.Y.Z` tag exists, built by the release
   workflow from #17, with its `CHANGELOG.md` section.
2. That release has run for **14 days** with no open issue labelled
   `security` or `stability`. Phase 3 bugs found in that window get the
   `stability` label (new); the clock restarts when one is opened.
3. The security review (#26) has landed, including `docs/security.md`.
   Every Phase 4 issue extends that threat model rather than writing its
   own.

## Rules for every item

- **Off by default.** No plugin runs unless its compose profile is
  started, and no sprig can use it until it is granted to that sprig.
- **A plugin, never core.** Each ability is a separate program in its own
  container, connected as a built-in MCP integration, the way
  `thicket-sandbox` is today. Core gets only the small, general hooks
  listed under [Foundations](#foundations).
- **Per sprig.** Each plugin is granted per sprig (`mcp:<plugin>/*`),
  like `mcp:sandbox/*`. Granting it is in the trail and needs the sprig's
  owner (the person a private sprig belongs to; the owner or an admin for
  a shared one).
- **Kid sprigs can't have them,** enforced in code next to
  `agent/kids.go`, with a test. Voice is the exception: it stays behind
  the existing parent `voice` allowance.
- **Every schema change is a migration**: numbered SQL in the store's
  migrations folder, after whatever Phase 3 ends on.
- **Tests and a doctor check per plugin.** Each plugin is tested against
  a fake of the outside service (fake JMAP, IMAP/SMTP and CDP servers),
  and registers a `thicket doctor` check.
- **Docs**: one page per plugin in `docs/`, plus the README and
  `docs/architecture.md`. The README line "no bundled browser, no plugin
  marketplace" becomes "the core bundles none; plugins are optional
  containers".
- **Dependencies stay out of `go.mod` at the root.** Each plugin lives in
  `plugins/<name>/` with its own `go.mod` or `package.json`, `Dockerfile`
  and tests, so the core binary's dependency list doesn't grow.
- Everything a plugin returns is untrusted content: wrapped, taint
  tracked, and subject to the "act needs the person's request" rule in
  `docs/architecture.md#untrusted-content-and-act`.

## Decisions

These were open questions in the spec; this is what we're going with.

| Question | Decision |
|---|---|
| What is a "plugin"? | An MCP server in its own container, started by a compose profile (`--profile mail`, `browser`, `voice`), registered as a built-in integration with a shared-volume token like the sandbox. Granted per sprig with the existing scope system. |
| A browser container per sprig? | **No Docker socket.** Giving Thicket the Docker socket is root on the host. One `thicket-browser` container runs one Chromium per active sprig, each with its own profile folder and its own egress proxy port; idle browsers are closed. A separate container per sprig is possible later by adding compose services by hand. |
| Browser automation stack | Playwright (Node) inside the plugin container, exposing MCP tools and a CDP screencast WebSocket. Nothing Node in core. |
| Domain allowlist enforcement | An egress proxy inside the browser container, per sprig, not navigation checks. Redirects, subresources and downloads all go through it. The container's network reaches only the internet, not the LAN or Thicket's ports (other than the MCP link). |
| Email: own adapter or Fastmail MCP? | Own `thicket-mail` plugin (Go, JMAP over plain HTTP). IMAP/SMTP fallback in the same plugin. The generic Fastmail MCP connector remains an alternative for people's *own* mail; `thicket-mail` is for *sprigs'* addresses. |
| Who creates addresses? | You do, at first (an alias or a mailbox with an app password, given to Juni in chat). Automatic Fastmail Masked Email creation is a later issue. |
| Email 2FA codes during a browser login | Never auto-filled. A code arriving in a sprig's mailbox while its browser is paused at a 2FA wall is shown to you in "needs you" next to the live-view link; you type it in during takeover. |
| "skills/ folder per sprig" | Skills get an optional owner sprig (`sprig_id`, migration). Taught and gallery-imported skills belong to one sprig; you can promote one to the shared library. The files live under `characters/<id>/skills/` and are mirrored into the store. |
| "Runs" of a skill | New: a skill run is recorded when a sprig follows a skill (it calls `use_skill`), with outcome. Needed for "first 3 runs ask first". |
| Payment always needs approval | New **level floor**: a tool call can carry a minimum level (`ask`) that permission levels can't lower. Used by browser checkout pages and taught skills' first runs. |
| Teach: local processing | **Local model only by default.** Teach needs a local model server (Ollama/LM Studio). With none, the feature says so. An owner can opt in to the main (cloud) model install-wide; redaction runs first either way, in code. |
| Voice: where speech runs | A `thicket-voice` plugin container with whisper.cpp (STT) and Piper (TTS) behind the OpenAI-compatible `/audio/transcriptions` and `/audio/speech` endpoints that core already speaks. Cloud (OpenRouter) stays as an option. New installs default to local when the voice profile is on; existing installs keep what they have and are told about it in the changelog. |
| "Episodic memory" for voice | Means the conversation history (messages table) that memory review reads. Transcripts are stored as ordinary messages marked `spoken`; raw audio is dropped after transcription unless `voice.keep_audio` is on. |
| Voice drift check | Per voice sample: synthesize it, check the voice config hasn't changed (engine, model, voice id), transcribe it back and require low word error rate, and compare a stored acoustic fingerprint (made when you approved the voice) within a tolerance. Fingerprints are local, never exported. |
| "Soul" and "job" | In Thicket a sprig is one `character.md`: personality and voice are its soul, role and tools its job. The gallery format names files by what they are. |
| Gallery signing and trust | Ed25519 (Go standard library). Trust on first use per author key, and the gallery index lists each author's key; an update must be signed by the same key. The index carries a revocation list. |
| Gallery package contents | A new export profile `--for-gallery`: `character.md`, `avatar.json`, `voice_tests/*.yaml`, `skills/*.md`, `manifest.json`, `signature`. No routines, memories or picture (the avatar is redrawn from `avatar.json`). Anything else is rejected on import. |
| Kid sprigs | Excluded from email, browser, teach and gallery imports (an imported sprig can't be made a kid sprig without the usual parent flow). |

## Foundations

Small general changes to core that the plugins build on. They land
first, each in its own PR.

1. **Plugin model** (`internal/plugin`, small): a registry of known
   plugins (name, compose profile, token file, default URL, doctor
   check, kid policy). On start, a plugin whose URL answers is registered
   as a built-in integration. `thicket doctor` gets a check per
   registered plugin. `docs/plugins.md` describes how to write one.
2. **Level floors**: `Tool` calls can carry `MinLevel`; `toolLevel`
   returns `max(level, floor)`. A floor is set by the plugin's tool
   result metadata (checkout detected) or by core (taught skill run
   count < 3). Shown in the approval prompt ("always asks: payment page").
3. **Sprig-owned skills and skill runs**: migration adds `skills.sprig_id`
   (nullable) and a `skill_runs` table (skill, sprig, started, outcome,
   forced_ask). `use_skill` records runs. Skills with a `sprig_id` are
   offered only to that sprig. Promote to shared needs owner/admin.

## Workstreams

Order: Foundations → Gallery → Voice → Email → Browser → Teach. Gallery
and Voice reuse the most existing code; Teach needs the browser.

### 1. Sprig gallery

- **Signed gallery packages**: `thicket export-sprig --for-gallery
  --sign KEY`, `thicket gallery keygen`. Format: a zip of the allowed
  files with `manifest.json` (SHA-256 of each) and a detached Ed25519
  signature over the manifest. Import verifies the signature, the
  hashes, the file allowlist, and a JSON Schema per file type; rejects
  anything not UTF-8 markdown/YAML/JSON, any symlink, any path outside
  the root, and any MCP/integration definition inside `character.md`
  (tools may only name scopes).
- **Import preview**: every file shown in full, every scope the sprig
  asks for listed, all tools starting at `draft` (gallery imports don't
  get `SafeScopes`). Skills land sprig-owned (Foundations 3).
- **Gallery index**: a git repo with a static site and `index.json`
  (packages, versions, author keys, revocations), built by CI that
  re-verifies every package. Thicket reads `index.json` from a
  configured URL (`THICKET_GALLERY_URL`, off when empty) and caches it.
- **Juni suggests**: `gallery_search` tool for Juni only, returns
  untrusted, wrapped index entries; Juni can suggest and open an import
  preview, never import on her own.

### 2. Voice

- **`thicket-voice` plugin**: whisper.cpp and Piper (Kokoro optional)
  behind OpenAI-compatible endpoints. Core's transcription works with a
  local `transcribe_url` (today it needs OpenRouter). arm64 and amd64
  images; model files downloaded on first start into a volume.
- **Push-to-talk in the PWA**: hold a button (or key) to talk; audio is
  sent when released, transcribed, the reply streamed as text and
  spoken. Builds on the recorder in `internal/web/static/app.js`.
  Transcripts go to history marked `spoken`; raw audio dropped.
- **Per-sprig voice file**: `characters/<id>/voice.yaml` (engine, model,
  voice, speed, pitch), set in chat ("make Pip sound calmer") with a
  preview clip before it's saved. `voice.sounds_like` keeps working.
- **Voice samples in voice tests and drift checks**: voice tests gain
  `speak:` samples; `thicket voice-check` and the scheduled drift check
  run the check described under Decisions.
- **Discord voice (later)**: a separate issue, blocked on an Opus path
  that works with `CGO_ENABLED=0` or on running it in the voice plugin.

### 3. Sprig email addresses

- **`thicket-mail` plugin, JMAP**: one account connection per sprig
  address (credentials in the encrypted store, added to the redactor).
  Inbound with JMAP push (EventSource), polling as fallback. Tools:
  `list_mail`, `read_mail`, `draft_mail`, `send_mail`, `reply_mail`,
  `archive_mail`. Mail bodies and attachments are untrusted; HTML is
  converted to text before the model sees it. Size and attachment caps.
- **Sending rules**: `send_mail`/`reply_mail` default to `draft`
  (written to Drafts and to "needs you"). `act` is possible only for a
  recipient or domain on the sprig's mail allowlist, checked in code on
  every recipient (to, cc, bcc); a message with any recipient off the
  list falls back to draft. Only the sprig's owner edits the allowlist;
  changes are in the trail. Send rate limit per sprig.
- **Trail**: every send and receive is a trail entry (from, to,
  subject, size; bodies are not copied into the trail).
- **IMAP/SMTP fallback**: same tools over IMAP (IDLE) and SMTP with
  STARTTLS/TLS required.
- **Mailbox view in the dashboard**: per sprig you can see, list and
  read (sanitized to text, no remote images), with drafts you can send
  or discard. Private sprigs' mailboxes only for the person they belong
  to.
- **Retention**: mail older than a setting (default 90 days) is no
  longer offered to the sprig; the provider keeps it.

### 4. Browser with live view and takeover

- **`thicket-browser` plugin**: Playwright + Chromium, one persistent
  profile per sprig under a volume (cookies survive restarts), one
  egress proxy per sprig enforcing its domain allowlist, no LAN access.
  MCP tools: `open`, `click`, `type`, `read_page`, `screenshot`,
  `back`, `done`. Page content is untrusted. Idle browsers close after
  10 minutes. Profiles are encrypted at rest with a key from Thicket's
  secret store, and are left out of backups unless you choose.
- **Walls**: on each navigation and action, the plugin checks for
  CAPTCHA (known widget iframes and challenge pages), 2FA
  (`autocomplete=one-time-code`, OTP inputs), login walls (password
  fields on a page the task didn't expect), and payment/checkout
  (`cc-*` autocomplete, known payment hosts, checkout URL patterns). On
  any: the sprig is paused for that task and a "needs you" item links to
  the live view. Checkout pages set a level floor of `ask`. A test page
  set covers each kind, including misses we know about.
- **Live view and takeover**: CDP screencast relayed by Thicket over an
  authenticated WebSocket on the dashboard (no plugin port exposed).
  **Take over** pauses the sprig, forwards your mouse and keyboard, and
  **Hand back** resumes with a note of what you did. Only the sprig's
  owner can watch or take over.
- **Screenshot trail**: a screenshot per step (password and payment
  fields blacked out before saving), linked from the task's trail entry,
  pruned after `browser_screenshot_days` (default 7) by a daily job.

### 5. Teach by showing

- **Record in the live view**: **Teach a task** starts a recording of
  your takeover session: DOM events (click target, typed text per
  field, navigation) plus low-rate screenshots. Password fields and any
  field you tap "sensitive" on are replaced with `{{input}}`
  placeholders in code before the recording is saved. Desktop recording
  is a later issue.
- **Recording → draft skill**: the local model turns the redacted
  recording into a skill (steps, inputs, success check) under
  `characters/<id>/skills/`, sprig-owned and inactive. You review and
  edit it in the web app; activating it is in the trail. The raw
  recording is deleted on approval unless you tick "keep".
- **First three runs ask first**: enforced with the level floor and
  skill runs from Foundations; each of those runs shows the steps it is
  about to take.

## Not in this phase

Discord voice, automatic address creation, desktop teach recording, and
a container per sprig for the browser are separate later issues.
