# Gallery packages

A gallery package is a sprig package meant for strangers: signed by its
author, and carrying only what describes the sprig. Ordinary packages
(`thicket export-sprig ID`) are for moving a sprig between Thickets you
trust; see [Sharing a sprig](characters.md#sharing-a-sprig).

## Making one

```bash
thicket gallery keygen                     # once
thicket gallery key                        # your key id and fingerprint, to publish
thicket export-sprig fern --for-gallery [-o FILE] [--skill ID]...
```

`keygen` makes an Ed25519 key pair with Go's standard library and keeps
the private key in the encrypted secret store (`gallery_signing_key`). It
won't replace an existing key unless you pass `--replace`: a new key can't
sign updates of sprigs you've already shared.

The package holds exactly these files:

| Path | What |
|---|---|
| `manifest.json` | kind `thicket-gallery-sprig`, format, sprig id and name, your key id (`author_key`), and every other file's SHA-256 |
| `signature` | a detached Ed25519 signature over `manifest.json`, with your public key and key id (JSON) |
| `character.md` | the character, without anything tied to your install |
| `sprig/avatar.json` | the drawn avatar's settings |
| `sprig/voice_tests/*.yaml` | its voice tests |
| `skills/*.md` | its own skills, and the shared skills it mentions (or `--skill ID`) |

Left behind: routines, memories, the picture, the model setting, who it
belongs to, kids, parents, host and household settings. The export is
refused if:

- the sprig is a kid sprig;
- its `tools` name anything but whole scopes: `schedule`, `web`,
  `calendar`, `characters`, `admin` or `mcp:<server>/*`, never a single
  tool like `mcp:sandbox/run`;
- anything looks like a secret;
- the result wouldn't pass import's checks below.

## Importing one

`thicket import-sprig FILE`, or send the file to Juni in chat or the web
app. Thicket tells a gallery package from an ordinary one by its
`signature` (or the manifest's kind) and reads it with these checks, in
this order, refusing with the reason at the first that fails:

1. Every entry is a plain file (no symlinks or hard links) at a clean,
   relative path (no `..`, no absolute path) on the allowlist above, is
   Markdown, YAML or JSON, and within the limits: 256 KB a file, 2 MB in
   all, 100 files, 20 skills.
2. The signature verifies over `manifest.json` with the public key it
   carries, the key id is that key's, and the key isn't revoked.
3. The manifest passes its schema and names the same key; every file is
   listed with a matching SHA-256 and every listed file is there. One
   changed byte anywhere is refused.
4. Every file is UTF-8 text without control characters, holds no MCP
   server or integration definition (`mcpServers`, `mcp_servers:`,
   `integrations:` blocks, `"command":`/`"transport":` settings), and passes
   its JSON Schema and Thicket's own checks (a valid character that isn't
   a kid sprig or the host, a valid avatar, voice tests with a prompt,
   skills whose id matches their file name).

### Trust

An author's key is **trusted on first use**: the preview shows its key id
and fingerprint (compare it with the one the author publishes) and says
whether it's new or known since an earlier import. Once you import, the
key is stored. A later package for the same sprig is an **update** and
must be signed by the key it first came with; a package for that sprig
signed by any other key is refused. A key id stored with a different
public key is refused too.

Revoked keys are refused at import:

```bash
thicket gallery revoke 0123456789abcdef   # the key id a preview shows
thicket gallery revoked
```

(The list is the `gallery.revoked_keys` setting; the gallery index will
bring its own.)

### What an imported sprig gets

- **No scopes.** The preview lists every scope the sprig asks for; grant
  any you want in chat. (Ordinary packages keep `schedule` and `web`;
  gallery ones don't.)
- **Every tool at draft**, with any levels left by an earlier sprig with
  the same id cleared.
- **Skills inactive until you approve them.** They're written as the
  sprig's own, as `characters/<id>/skills/<skill>.md.pending`, which the
  skill loader skips. Approve one with `thicket skill approve SPRIG ID`
  (`thicket skill pending SPRIG` shows them in full), or ask the sprig: it
  shows you the skill and asks. A skill whose id your shared library or
  another sprig already uses is left out (the preview says so).
- It's a family sprig, never a kid sprig or the host, and the trail
  records the import with the author's key.

An update replaces the character's description, its avatar settings,
voice tests and skills (pending approval again), and keeps what is yours:
its scopes, levels, who it belongs to, its picture and model.

## Schemas

The schemas are in the source, `internal/sprigpkg/schemas/`:
`manifest`, `signature`, `character` (the front matter of
`character.md`), `avatar`, `voicetest` and `skill` (the front matter of a
skill file). Every object is closed (`additionalProperties: false`), so a
field the schema doesn't name is refused rather than ignored.
