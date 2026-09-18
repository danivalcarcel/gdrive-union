# gdunion

Mounts several Google Drive accounts as a single filesystem (`rclone
mount`-style, but as a "union filesystem"): each virtual folder can merge
content from multiple accounts, but every file lives in exactly one account
(no copy of the same file in more than one place). If an account fails or
its token stops working, you only lose what was stored there, not the whole
mount.

Supports read and write: create, edit, delete, rename and move
files/folders. New files are placed automatically on whichever account has
the most free space among those present in that folder.

gdunion never touches your existing Drive content: each account is scoped
to its own dedicated `gdrive-union-<account-name>` folder (created automatically
if it doesn't exist yet). Everything gdunion reads, merges or writes lives
inside that one folder per account; the rest of that Drive is left alone.

Optionally, per account, file content and filenames can be end-to-end
encrypted (see [Encryption](#encryption)), so neither Google nor anyone
with access to the Drive web UI can read them - only whoever holds the
local key file can.

## Requirements

- Linux or macOS (or WSL2 with FUSE support on Windows) with `libfuse3`
  installed:
  ```bash
  sudo apt install fuse3
  ```
- A Google Cloud account to create OAuth credentials (free).
- Go 1.22+, only if building from source instead of using the install
  script below.

## 1. Create OAuth credentials (one time)

1. Go to [Google Cloud Console](https://console.cloud.google.com/) and
   create a new project (or use an existing one).
2. Enable the **Google Drive API** ("APIs & Services" > "Library").
3. Go to "APIs & Services" > "OAuth consent screen", type **External**, and
   add yourself (and any other accounts you'll use) as a "Test user" while
   the app is in testing mode.
4. Go to "APIs & Services" > "Credentials" > "Create Credentials" > "OAuth
   client ID", type **Desktop app**.
5. Download that credential's JSON and save it to:
   - Linux/Mac: `~/.config/gdunion/client_secret.json`
   - (create the folder if it doesn't exist: `mkdir -p ~/.config/gdunion`)

These credentials are reused for every account you add afterwards; each
account only contributes its own login/token.

## 2. Install

Quickest way, downloads the right prebuilt binary (Linux/macOS,
amd64/arm64) from the [latest release](https://github.com/danivalcarcel/gdrive-union/releases/latest)
into `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/danivalcarcel/gdrive-union/master/install.sh | bash
```

Pass a specific release tag as an argument to pin a version instead of
latest: `... | bash -s v0.2.0`. Set `GDUNION_INSTALL_DIR` to install
somewhere other than `/usr/local/bin`.

Or build from source:

```bash
go mod tidy   # fetches dependencies (needs network)
go build -o gdunion ./cmd/gdunion
```

Or, to try it (or develop/test it) without installing anything on your
machine beyond Docker, see [Try it with Docker](#try-it-with-docker) below
and skip straight to step 3 - Docker handles the rest of this page's steps
for you.

## 3. Add accounts

```bash
gdunion auth add personal
gdunion auth add work
gdunion auth add another-account
```

Each one opens (or prints) a Google URL to sign in with that account and
authorize full (read/write) access to Drive. The token is saved to
`~/.config/gdunion/accounts/<name>.json` and refreshes itself. Right after
authorizing, gdunion also creates (or reuses) that account's dedicated
`gdrive-union-<name>` folder and confirms it in the output.

If you had already used an earlier version of gdunion (one that mounted the
whole Drive root, or used a different app-folder prefix like `gdrive-<name>`),
any files from back then are sitting outside the current
`gdrive-union-<name>` folder - move them in manually (via the Drive web UI,
or `mv` once mounted, since it's just a regular Drive folder) if you want
them included.

If you authorized an account with an older, read-only version of gdunion,
you need to re-run `auth add <name>` for that account: Google won't widen
the scope of a token that's already been issued.

If the browser completing the login isn't on the same machine as `gdunion`
(for example, you're on a headless server over SSH), open a local tunnel
first, using the same port as `--port` (53682 by default):

```bash
ssh -L 53682:localhost:53682 user@server
```

and run `gdunion auth add <name>` inside that same SSH session.

Check available quota for each account:

```bash
gdunion auth list
```

## 4. Mount

```bash
mkdir -p ~/gdrive
gdunion mount ~/gdrive
```

Ctrl+C unmounts cleanly. If the process dies without unmounting, free the
mountpoint with:

```bash
fusermount3 -u ~/gdrive
```

## Try it with Docker

An alternative to steps 1-4 above: build and run gdunion in a container,
so nothing touches your host beyond Docker itself. Useful for trying the
tool, or for development/testing without a Linux machine handy.

Requirements: Docker with Compose v2 (`docker compose version` should
work). The container needs `SYS_ADMIN` and access to `/dev/fuse` to mount
FUSE filesystems at all - [`docker-compose.yml`](docker-compose.yml)
already requests both, so plain `docker compose` commands are enough; you
don't need to pass extra flags yourself.

1. Get OAuth credentials the same way as step 1 above (Google Cloud
   Console), and copy the downloaded JSON into the container's config
   volume (a one-off container just for this, since it's a named volume
   rather than a bind mount):

   ```bash
   docker compose run --rm -T gdunion sh -c 'cat > /root/.config/gdunion/client_secret.json' < /path/to/client_secret.json
   ```

2. Add an account. Note the `--bind 0.0.0.0`: unlike running natively,
   Docker's published-port forwarding doesn't arrive on the loopback
   interface, so the OAuth callback server needs to listen on all
   interfaces *inside* the container (see `gdunion auth add`'s `--bind`
   flag) - the URL you actually open is still `http://127.0.0.1:53682/...`,
   since that's resolved on your machine, not inside the container.

   ```bash
   docker compose run --rm gdunion auth add personal --bind 0.0.0.0
   ```

3. Mount (runs in the foreground; Ctrl+C stops it):

   ```bash
   docker compose up
   ```

4. Poke around from another terminal:

   ```bash
   docker compose exec gdunion sh
   ls /mnt/gdrive
   ```

Any other `gdunion` subcommand works the same way, e.g.
`docker compose run --rm gdunion crypt enable personal` or
`docker compose run --rm gdunion auth list`.

The FUSE mount only exists inside the container's own mount namespace - it
isn't visible in your host's file manager or editor, only inside the
container (step 4 above, or similar). That's enough to try the tool or run
it as a semi-isolated background service; if you want a mount you can
browse normally from your host, install natively instead (step 2, above).

## Encryption

Off by default. Turn it on per account:

```bash
gdunion crypt enable personal
```

This generates a random key and saves it to
`~/.config/gdunion/accounts/personal.key` (readable only by you). From then
on, everything gdunion creates or edits in that account is encrypted before
it ever leaves this machine: file content, and filenames too (so the Drive
web UI shows unreadable names, not just unreadable content).

Check which accounts are encrypted:

```bash
gdunion crypt status
```

**Back up that key file.** There is no password reset, no recovery option,
no way to ask Google for help: lose the key and everything gdunion
encrypted with it is permanently unreadable. Treat it like an SSH private
key.

Notes:

- Only *new* files/folders (created after `crypt enable`) are encrypted.
  Anything already in that account's app folder stays exactly as it was;
  gdunion keeps reading and writing it as plaintext, no migration happens
  automatically.
- Google Docs/Sheets/Slides are never encrypted (see Known limitations -
  their content isn't something gdunion stores, so there's nothing to
  encrypt).
- The app folder's own name (`gdrive-union-<account-name>`) is never
  encrypted, so you can still find it in the Drive UI - only what's *inside*
  it is.
- Moving a file between two accounts that both have encryption on
  re-encrypts it with the destination's key along the way (see
  Writing: how it works); it's never re-uploaded under one account's key
  while readable with another's.
- Overhead is small: about 44 bytes per 64KiB chunk (~0.07%), plus a 4-byte
  header per file.

## How name collisions are resolved

- If two accounts have a folder with the same name (e.g. "Documents"), they
  get **merged**: listing that folder shows files from both accounts
  together.
- If two accounts have a **file** with the same name (or a file and a
  folder collide), the first account in alphabetical order keeps the plain
  name, and the others get renamed with the account appended:
  `report.pdf` and `report (work).pdf`.

## Writing: how it works

- **New files**: when creating a file in a folder, gdunion checks which
  accounts have a presence in that folder and picks whichever has the most
  free space (with a 60s quota cache, so it isn't checked on every single
  operation).
- **New folders**: same placement policy.
- **Editing**: changes are written to the local cache and uploaded whole to
  Drive when the file is closed (or on an explicit `fsync`). There's no
  incremental byte-range upload.
- **Deleting** (`rm`/`rmdir`): moves to Drive's trash rather than deleting
  permanently. `rmdir` only works if the folder is empty across every
  account it's made of.
- **Renaming/moving** (`mv`): if source and destination are in the same
  account, it's a native Drive move (fast). If you move a file between
  folders that live in different accounts, gdunion streams the content
  through this machine into the destination account and then trashes the
  original (slower, and briefly uses quota on both accounts). Moving an
  **entire folder** between different accounts isn't supported (only
  renaming/moving folders within the same account).
- Overwriting an existing file via `mv` (the "safe save" pattern many
  editors use: write to a temp file, then rename it over the original) is
  supported: the old file gets trashed and the new one takes its place.

## Known limitations

- Google Docs/Sheets/Slides get exported to PDF/XLSX on read (they have no
  downloadable native binary); they're also read-only (you can't "edit" a
  Doc by uploading new bytes this way).
- Files are downloaded/uploaded whole through a local cache
  (`$TMPDIR/gdunion-cache/<account>/<id>`), rather than streamed via HTTP
  byte ranges. Very large files take longer as a result.
- Each folder's listing is cached for 20s before hitting the API again;
  very recent changes made in Drive from outside this mount can take up to
  that long to show up. Changes made from this mount itself show up
  immediately (they don't wait on that cache).
- The download cache is only invalidated by a file-size check; an external
  edit that doesn't change the size (rare, but possible) won't be noticed
  until the cache expires or you clear it manually.
- No concurrency control across processes/machines: if two places edit the
  same file at once, whoever uploads last wins (no merge, no conflict
  warning).
- Encryption assumes uniformity: once `crypt enable`d, gdunion treats
  everything under that account as encrypted. It can still show and read
  older plaintext files left over from before (best-effort fallback), but
  their reported size may be off in that mixed scenario, since it's
  normally computed from the encrypted size without downloading. Simplest
  to enable encryption before putting real files in an account.
