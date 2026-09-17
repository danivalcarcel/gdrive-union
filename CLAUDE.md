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

`go mod tidy` needs network access to resolve/fetch dependency versions.

## Architecture

Four packages, each with a single responsibility, composed in
`cmd/gdunion/main.go`:

- **`internal/gdrive`** - thin wrapper around the Drive v3 API for one
  authenticated account (`Account`). Knows nothing about other accounts or
  the union tree: list children, download/export content, create/update/
  move/trash files, a 60s-cached `FreeSpace()` used for write placement, and
  `EnsureFolder` (find-or-create a folder by name under a parent).
- **`internal/auth`** - the OAuth2 flow per account. `AddAccount` runs a
  loopback HTTP server (fixed port by default, `--port` flag, so it can be
  reached through an `ssh -L` tunnel when the browser isn't on the same
  machine as gdunion) and persists the token. `TokenSource` wraps
  `oauth2.TokenSource` to write refreshed tokens back to disk so re-auth is
  never needed unless the OAuth scope changes.
- **`internal/config`** - just filesystem paths: `~/.config/gdunion/` for
  the shared `client_secret.json` (one OAuth client for every account) and
  `accounts/<name>.json` per-account tokens.
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

When adding a new `DirNode`/`FileNode` FUSE operation, check
`fs.Node*`/`fs.File*` interface signatures directly against the vendored
source in the Go module cache
(`$(go env GOPATH)/pkg/mod/github.com/hanwen/go-fuse/v2@<version>/fs/api.go`)
rather than relying on memory - several method signatures (e.g.
`SetAttrIn.GetSize()`, `Attr.SetTimes()`, embedded `Owner` fields for
uid/gid) are easy to get subtly wrong.
