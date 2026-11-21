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

(The list is the `gallery.revoked_keys` setting. A
[shared gallery](#browsing-a-shared-gallery) brings its own list too, and
both are honoured, for packages from the gallery and for files alike.
`thicket gallery revoked` shows both.)

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

## Browsing a shared gallery

A shared gallery is a website listing signed packages: its `index.json`
names every sprig, its versions with their SHA-256, the authors' public
keys, and the keys its moderators revoked. Point Thicket at one:

```bash
THICKET_GALLERY_URL=https://example.org/thicket-gallery/index.json
```

It's off when empty. Thicket fetches the index through the same rules as
web pages (public addresses only; `THICKET_WEB_PROXY` applies), at most
4 MB, checks it against its schema, caches it in the data folder and
fetches it again once a day.

```bash
thicket gallery list                 # every sprig in the gallery
thicket gallery show fern            # author, fingerprint, scopes, versions
thicket gallery install fern [--version N] [--id ID] [--name NAME] [--yes]
thicket gallery refresh              # fetch the index now
```

`install` downloads the package and refuses it unless its size and
SHA-256 match the index, its author key isn't revoked (by the gallery or
by you) and it's signed by the key the index lists for that sprig. Then it
goes through the normal import above: every check, the full preview, and
nothing written until you say yes.

In the web app, the dashboard's **Gallery** page lists the same sprigs
and opens the same preview ([Dashboard](dashboard.md#gallery)).
`thicket doctor` checks that the index is reachable and valid, and how old
the cached copy is.

## Submitting a sprig

1. Make your author key and publish its fingerprint (above).
2. `thicket export-sprig ID --for-gallery -o ID.sprig`.
3. Open a pull request on the gallery's repository adding it as
   `packages/ID/1.sprig` (or the next number, for a new version). The
   first time, add yourself to `authors.json`: `thicket gallery verify .`
   prints the line to add.
4. Run `thicket gallery build-index .` and `thicket gallery verify .`
   before you push.

A new version must be signed with the same key as the earlier ones.

## Running your own gallery

`gallery/template/` in Thicket's source is a ready-to-copy repository:
its README (for authors and moderators), `authors.json`, `revoked.json`,
an empty `index.json`, a pull request template and two GitHub workflows.
It needs only the `thicket` binary.

| Path | What |
|---|---|
| `packages/ID/N.sprig` | version N of sprig ID |
| `authors.json` | accepted author keys: name, public key, optional link |
| `revoked.json` | revoked author keys, with a reason and date |
| `index.json` | made by `thicket gallery build-index`, never by hand |

```bash
thicket gallery verify DIR           # what CI runs on every pull request
thicket gallery build-index DIR      # write DIR/index.json
thicket gallery build-site DIR [-o OUT]   # the static site, default DIR/site
```

`verify` re-reads every package with import's checks (signature, every
hash, the file allowlist, every schema), and refuses a package signed by
a key that isn't in `authors.json` or is revoked, versions of one sprig
signed by different keys, stray files, and an `index.json` that isn't
exactly what the packages make. The `verify` workflow runs it on every
pull request; make it a required check so a failing change can't be
merged.

`build-site` writes one folder to publish: `index.html` (each sprig with
its drawn avatar, description, author, key fingerprint, the scopes it
asks for, and every version with its SHA-256), `index.json` and the
packages. The `pages` workflow publishes it on GitHub Pages; any static
host works. People then set `THICKET_GALLERY_URL` to its `index.json`.

The verify check proves a package is well formed and signed, not that
it's good. The template's README lists the review steps for moderators:
check the fingerprint of a new author, read the sprig's full preview,
look for descriptions or skills that try to instruct an assistant, and
for scopes it doesn't need.

To revoke a key, add it to `revoked.json`, remove its packages and
rebuild the index. Thicket installs pick it up within a day.

## Schemas

The schemas are in the source, `internal/sprigpkg/schemas/`:
`manifest`, `signature`, `character` (the front matter of
`character.md`), `avatar`, `voicetest` and `skill` (the front matter of a
skill file), and for a shared gallery `index`, `authors` and `revoked`.
Every object is closed (`additionalProperties: false`), so a field the
schema doesn't name is refused rather than ignored.
