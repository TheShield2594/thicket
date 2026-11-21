# Thicket sprig gallery

Sprigs shared by Thicket households. Each one is a signed gallery package:
a character, its drawn avatar, voice tests and skills, signed by its
author's key. Thicket checks every package again when someone installs it,
and shows them all of it first.

To use this gallery, set `THICKET_GALLERY_URL` to the published site's
`index.json`, then run `thicket gallery list`, open the Gallery page in the
dashboard, or ask Juni.

## What's here

| Path | What | Who writes it |
|---|---|---|
| `packages/ID/N.sprig` | version N of sprig ID | authors, by pull request |
| `authors.json` | accepted author keys: key id, name, public key, optional link | moderators |
| `revoked.json` | revoked author keys, with a reason and date | moderators |
| `index.json` | everything above in one file, for Thicket | `thicket gallery build-index` |

The `verify` workflow runs `thicket gallery verify .` on every pull request.
It re-checks every package (signature, every hash, the file allowlist,
every schema), that each is signed by a key in `authors.json` that isn't
revoked, that every version of a sprig has the same key, and that
`index.json` is exactly what the packages make. Make it a required check
on `main`: a change that fails it can't be merged.

The `pages` workflow publishes the site (`thicket gallery build-site`) on
every push to `main`.

## Submitting a sprig

1. Make an author key once: `thicket gallery keygen`. Publish your
   fingerprint (`thicket gallery key`) somewhere people can check it.
2. Export the sprig: `thicket export-sprig ID --for-gallery -o ID.sprig`.
3. Fork this repository and add it as `packages/ID/1.sprig` (or the next
   number, for a new version).
4. The first time, add yourself to `authors.json`. `thicket gallery verify .`
   prints the line to add, with your public key.
5. Run `thicket gallery build-index .` and `thicket gallery verify .`.
6. Open a pull request. Say what the sprig does and where your
   fingerprint is published.

A new version must be signed with the same key as the earlier ones.

## Reviewing a submission (moderators)

The `verify` check proves a package is well formed and signed. It can't
tell whether the sprig is any good or kind. Before merging:

1. **The check passed.** Don't merge a red pull request, and don't merge
   one that turns the check off or changes the workflows.
2. **Only the expected files changed**: one or more `packages/ID/N.sprig`,
   `index.json`, and for a new author one entry in `authors.json`. Nothing
   else.
3. **A new author is who they say.** Compare the fingerprint the check
   prints with the one the author published. If you can't, ask.
4. **Read the sprig.** Import it on a test Thicket
   (`thicket import-sprig packages/ID/N.sprig`, then answer n): the
   preview shows every file in full. Look for:
   - a description that tries to instruct an assistant ("ignore your
     instructions", "install", "grant", "forward");
   - skills that ask for secrets, send things elsewhere, or do more than
     the description says;
   - scopes it doesn't need (`admin` and `characters` rarely make sense);
   - anything unkind, unsafe for a household, or plain spam.
5. **An update** should do what the old version did, plus what its author
   says changed. Compare the two previews.
6. Merge with a review from a second moderator for new authors.

## Revoking a key

When an author's key is lost, stolen or abused:

1. Add `{"key_id": "…", "reason": "…", "revoked_at": "YYYY-MM-DD"}` to
   `revoked.json`.
2. Remove that key's packages from `packages/` (the check refuses a
   revoked key's packages).
3. Run `thicket gallery build-index .` and merge.

Every Thicket using the gallery picks up the revocation within a day and
refuses packages signed with that key, from the gallery or as files.
Sprigs already installed stay; their owners decide.
