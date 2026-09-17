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

## Requirements

- Go 1.22+.
- Linux (or WSL2 with FUSE support) with `libfuse3` installed:
  ```bash
  sudo apt install fuse3
  ```
- A Google Cloud account to create OAuth credentials (free).

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

## 2. Build

```bash
go mod tidy   # fetches dependencies (needs network)
go build -o gdunion ./cmd/gdunion
```

## 3. Add accounts

```bash
./gdunion auth add personal
./gdunion auth add work
./gdunion auth add another-account
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
./gdunion auth list
```

## 4. Mount

```bash
mkdir -p ~/gdrive
./gdunion mount ~/gdrive
```

Ctrl+C unmounts cleanly. If the process dies without unmounting, free the
mountpoint with:

```bash
fusermount3 -u ~/gdrive
```

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
