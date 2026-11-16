# Changelog

All notable changes are listed here. Thicket uses small releases and
versioned database migrations; upgrades never require manual steps unless
stated.

## Unreleased: v0.1.0 (day one)

First version.

- Discord channel: DMs and server channels, per-sprig name and avatar
  through webhooks, embeds in DMs.
- Attachments: sprigs see images (PNG, JPEG, GIF, WebP; up to 4 per
  message, 5 MB each) and read text files (up to 256 KB, first 12,000
  characters). Images go to the model with that message only and are
  never stored. Text files stay in the conversation with secrets redacted,
  marked as untrusted content. If the model refuses images, the sprig
  still replies and says it couldn't see them. Other files are passed on
  by name only.
- PDFs: up to 2 per message (10 MB, 50 pages each), sent to the model with
  that message only. A new `pdf_engine` setting (auto, native,
  cloudflare-ai, mistral-ocr) picks how OpenRouter reads them; within a
  turn, parsed PDFs are reused instead of parsed again.
- Character system: one Markdown file per sprig with YAML frontmatter;
  validation, hot reload, never overwrites hand edits. Juniper ("Juni")
  ships as the host.
- Conversational onboarding: scripted key setup, then Juni learns your
  name, timezone and needs.
- New sprigs by interview → in-voice preview → tweak → create.
- Self-updating memory: shared and per-sprig scopes, background review by
  a cheap model with code validation, locked user memories, "what do you
  remember" and "forget that", rollback per review run.
- Light initiative: follow-ups, quiet-spell check-ins, quiet hours.
- Handoffs between sprigs.
- MCP integrations over Streamable HTTP and stdio, granted per sprig.
- Approvals for risky actions, configurable per tool.
- Scheduled jobs with run history (ok / nothing found / error / missed).
- Spend tracking per sprig and job, daily and monthly caps with alerts.
- Retries and fallback model.
- Encrypted secret store; secrets never reach the model or logs.
- `/health`, structured JSON logs, activity log, failures reported in chat.
- Export / restore, daily snapshots, character import and export.
- Schema migration 0001.
