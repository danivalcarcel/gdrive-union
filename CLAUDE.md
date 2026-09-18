# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`gdunion` mounts several Google Drive accounts as a single FUSE filesystem: a
union filesystem where each virtual directory can merge folders from
multiple accounts, but every file lives in exactly one account (no
replication). It supports both read and write. See README.md for the
end-user setup flow (OAuth credential creation, adding accounts, mounting)
and the exact semantics of writes, renames, and known limitations - don't
duplicate that here, read it when those details matter.

## Commands

FUSE only builds/runs on Linux (and Darwin), so on a non-Linux dev machine
always cross-compile with `GOOS=linux`:

```bash
GOOS=linux GOARCH=amd64 go build -o gdunion-linux-amd64 ./cmd/gdunion
GOOS=linux GOARCH=arm64 go build -o gdunion-linux-arm64 ./cmd/gdunion
GOOS=linux go vet ./...
```

There are no automated tests in this repo yet. Verify changes by building
and `go vet`-ing for `GOOS=linux`; actually exercising a mount requires a
real Linux/WSL host with `fuse3` installed and at least one authorized
Google account (`./gdunion auth add <name>`, then `./gdunion mount <dir>`).
`docker compose up` (see `Dockerfile`/`docker-compose.yml`, README's "Try
it with Docker") is another way to get that Linux+FUSE environment,
including from a non-Linux dev machine with Docker installed - the
container needs `SYS_ADMIN` + `/dev/fuse` access, which the compose file
already requests. Note `auth add` needs `--bind 0.0.0.0` in that case:
Docker's published-port forwarding doesn't arrive on loopback, unlike a
plain `ssh -L` tunnel (see `internal/auth`'s `AddAccount` doc comment).

`go mod tidy` needs network access to resolve/fetch dependency versions.

There are unit tests for `internal/gcrypt` (pure Go, no FUSE dependency, so
it also builds and runs natively on a non-Linux dev machine without
cross-compiling): `go test ./internal/gcrypt/...`.

## Architecture

Five packages, each with a single responsibility, composed in
`cmd/gdunion/main.go`:

- **`internal/gdrive`** - thin wrapper around the Drive v3 API for one
  authenticated account (`Account`). Knows nothing about other accounts or
  the union tree: list children, download/export content, create/update/
  move/trash files, a 60s-cached `FreeSpace()` used for write placement, and
  `EnsureFolder` (find-or-create a folder by name under a parent).
  `Account.Cipher` (nil unless encryption is enabled) is just carried here
  for `internal/unionfs` to use - `gdrive` never calls into it itself, so
  e.g. `EnsureFolder`'s calls for the (always-plaintext) app root folder
  don't need to care that a cipher exists.
- **`internal/auth`** - the OAuth2 flow per account. `AddAccount` runs a
  loopback HTTP server (fixed port by default, `--port` flag, so it can be
  reached through an `ssh -L` tunnel when the browser isn't on the same
  machine as gdunion) and persists the token. `TokenSource` wraps
  `oauth2.TokenSource` to write refreshed tokens back to disk so re-auth is
  never needed unless the OAuth scope changes.
- **`internal/config`** - just filesystem paths: `~/.config/gdunion/` for
  the shared `client_secret.json` (one OAuth client for every account),
  `accounts/<name>.json` per-account tokens, and `accounts/<name>.key`
  per-account encryption master keys (presence of the file is what turns
  encryption on for that account - see `internal/gcrypt`).
- **`internal/gcrypt`** - optional per-account encryption, policy-free (it
  just seals/opens byte streams and strings; deciding *when* to use it is
  `internal/unionfs`'s job). `Cipher` derives independent filename and
  content subkeys via HKDF from one master key. Names and file content are
  both self-describing (a fixed magic marker up front), so `DecryptName`/
  `OpenReader` can tell "this is something gdunion encrypted" apart from a
  plaintext leftover from before encryption was enabled and fall back to
  using it as-is - a trial decrypt is safe here (not just probabilistic)
  because secretbox authenticates every chunk. Content is chunked (64KiB
  plaintext per chunk, NaCl secretbox with a random nonce per chunk) so
  memory use stays bounded regardless of file size; `PlaintextSize` computes
  the original size from an encrypted size via the fixed per-chunk overhead,
  without needing to decrypt (used to report the size a user expects to see
  instead of the larger on-disk encrypted one).
- **`internal/unionfs`** - the actual filesystem, built on
  `github.com/hanwen/go-fuse/v2/fs`. `NewRoot(sources []Source)` builds the
  tree root directly from caller-supplied sources; it does not know about
  "Drive root" at all - `cmd/gdunion/main.go` resolves each account's
  dedicated `gdrive-union-<account-name>` app folder via `EnsureFolder` first
  (mount never touches a user's pre-existing Drive content). This is where
  most of the interesting logic lives:
  - `node.go`: `DirNode` is a virtual directory backed by `[]Source`
    (account + Drive folder ID pairs) - more than one when folders from
    different accounts share a name and get merged. `refresh()` lists every
    contributing account's folder and rebuilds a `map[string]childInfo`
    cache (`listTTL` = 20s); `mergeEntry` is where name-collision handling
    happens (same-named folders merge, same-named files/type-mismatches get
    an ` (accountname)` suffix via `disambiguate`). Write operations
    (`Mkdir`, `Create`, `Unlink`, `Rmdir`, `Rename`) mutate this cache
    in-place via `putChild`/`deleteChild` immediately after the Drive API
    call succeeds, rather than waiting for the next `refresh()`, since Drive
    gives no read-your-writes guarantee on `Files.list`. `pickWriteTarget`
    is the "most free space" placement policy, restricted to accounts that
    already have a folder at that path (you can only create inside a folder
    that exists in that account's graph).
  - `file.go`: `FileNode` (one file, one `Source`) and `fileHandle` (the
    open file). Reads and writes both go through a local cache file at a
    deterministic path; writes are buffered locally and uploaded whole on
    `Flush`/`Release`/`Fsync`, not streamed byte-range. `Open` handles
    `O_TRUNC` by skipping the download and creating a fresh empty cache file
    instead.
  - `cache.go`: the on-disk cache under `$TMPDIR/gdunion-cache/<account>/
    <fileID>`. `ensureCached` dedupes concurrent downloads of the same file
    via a `sync.Once` per key and treats an existing local file as fresh if
    its size matches Drive's reported size (not a hash - see README's Known
    Limitations). `cachePathFor` just computes the deterministic path
    without downloading, used when about to overwrite content anyway.
  - Cross-account renames/moves (`node.go`'s `moveFileSource`): Drive has no
    API to move a file between two different Google accounts, since their
    file ID spaces are disjoint. When the destination directory doesn't
    already have a folder in the source file's account, gdunion falls back
    to streaming the content through the local machine (download from the
    source account, create+upload to the destination account, trash the
    original). This does not exist for whole-folder moves (`moveFolderSource`
    returns `ENOTSUP` for a cross-account folder move) - only files.
  - Encryption integration points, all in `node.go`/`file.go`/`cache.go`,
    gated on `src.Account.Cipher != nil`: `mergeEntry` decrypts names coming
    back from `ListChildren` (and rescales `childInfo.size` from ciphertext
    to plaintext via `gcrypt.PlaintextSize`, skipped for Google-native
    export types); `Mkdir`/`Create` encrypt the name handed to
    `CreateFolder`/`CreateFile` but keep the plaintext name as the local map
    key (that's what the user sees); `cache.go`'s `ensureCached` wraps the
    download in `Cipher.OpenReader` before writing the local (always
    plaintext) cache file; `file.go`'s `fileHandle.upload`/`truncateRemote`
    wrap the upload body in `Cipher.EncryptReader` and then use the local
    file's own stat'd size (not whatever `UploadContent` reports back,
    which would be the larger ciphertext size) to update `FileNode.size`.
    The local cache is always plaintext - only the two Drive API boundaries
    (`Account.Open`/`Account.UploadContent`) ever see ciphertext - which is
    why none of the FUSE read/write offset math had to change for this.

When adding a new `DirNode`/`FileNode` FUSE operation, check
`fs.Node*`/`fs.File*` interface signatures directly against the vendored
source in the Go module cache
(`$(go env GOPATH)/pkg/mod/github.com/hanwen/go-fuse/v2@<version>/fs/api.go`)
rather than relying on memory - several method signatures (e.g.
`SetAttrIn.GetSize()`, `Attr.SetTimes()`, embedded `Owner` fields for
uid/gid) are easy to get subtly wrong.
