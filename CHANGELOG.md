# Changelog

All notable changes are listed here. Thicket uses small releases and
versioned database migrations; upgrades never require manual steps unless
stated.

## Unreleased: v0.1.0 (day one)

First version.

- Discord channel: DMs and server channels, per-sprig name and avatar
  through webhooks, embeds in DMs.
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
- **Web app.** Every sprig in one place: a thread per sprig, an Everyone
  room, up to three chats side by side, a details panel with memories and
  routines, live typing and replies, light and dark themes. Installable
  as an app (PWA) on phones and desktops.
- One-time sign-in links from `thicket web link`, from Juni in a Discord
  DM (`web_app_link`), or from the startup log on a fresh install.
- Discord is now optional: without `DISCORD_TOKEN` Thicket runs as a web
  app only.
- Schema migration 0002 (web sign-in, sessions and transcripts).
