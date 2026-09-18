package unionfs

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FileNode is a single virtual file, backed by exactly one account's Drive
// file (no cross-account duplication: each file lives in one place). src,
// size and modTime are mutated in place after a write uploads new content,
// or after a rename moves the file to another Source.
type FileNode struct {
	fs.Inode

	mu       sync.Mutex
	src      Source
	mimeType string
	size     int64
	modTime  time.Time
}

var (
	_ fs.NodeOpener    = (*FileNode)(nil)
	_ fs.NodeGetattrer = (*FileNode)(nil)
	_ fs.NodeSetattrer = (*FileNode)(nil)
)

func (n *FileNode) snapshot() (Source, string, int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.src, n.mimeType, n.size
}

func (n *FileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(n.size)
	out.SetTimes(nil, &n.modTime, nil)
	out.Uid = mountUID
	out.Gid = mountGID
	return 0
}

// Setattr handles truncate (the only attribute change we support changing
// remotely; permission/time changes are accepted but not persisted to
// Drive, which has no such concept).
func (n *FileNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		if fh, ok := f.(*fileHandle); ok {
			if err := fh.truncate(int64(sz)); err != nil {
				return syscall.EIO
			}
		} else {
			// No open handle (e.g. `truncate -s 0 file` on an unopened
			// file): apply directly against Drive.
			if err := n.truncateRemote(ctx, int64(sz)); err != nil {
				return syscall.EIO
			}
		}
	}
	return n.Getattr(ctx, f, out)
}

func (n *FileNode) truncateRemote(ctx context.Context, size int64) error {
	src, mimeType, curSize := n.snapshot()
	path, err := ensureCached(ctx, src, mimeType, curSize)
	if err != nil {
		return err
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var body io.Reader = f
	if src.Account.Cipher != nil {
		body = src.Account.Cipher.EncryptReader(f)
	}
	if _, err := src.Account.UploadContent(ctx, src.FileID, body); err != nil {
		return err
	}
	n.mu.Lock()
	// size, not whatever UploadContent reports back: with encryption on,
	// that's the larger ciphertext size, not the size a user expects to see.
	n.size = size
	n.mu.Unlock()
	return nil
}

// Open downloads the file to a local cache on first access (or reuses a
// previous download) and serves reads from there; writes go to that same
// local copy and are uploaded whole on close. This trades memory/disk for
// simplicity over streaming HTTP range requests / byte-range patches,
// matching rclone's vfs-cache-mode=full behavior.
func (n *FileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	src, mimeType, size := n.snapshot()

	truncating := flags&uint32(os.O_TRUNC) != 0

	var path string
	var err error
	var f *os.File
	if truncating {
		// About to discard whatever's there; no need to download it first.
		path, err = cachePathFor(src)
		if err == nil {
			f, err = os.Create(path)
		}
	} else {
		path, err = ensureCached(ctx, src, mimeType, size)
		if err == nil {
			f, err = os.OpenFile(path, os.O_RDWR, 0o600)
		}
	}
	if err != nil {
		return nil, 0, syscall.EIO
	}
	return &fileHandle{f: f, node: n, dirty: truncating}, fuse.FOPEN_KEEP_CACHE, 0
}

type fileHandle struct {
	f     *os.File
	node  *FileNode
	mu    sync.Mutex
	dirty bool
}

var (
	_ fs.FileReader   = (*fileHandle)(nil)
	_ fs.FileWriter   = (*fileHandle)(nil)
	_ fs.FileFlusher  = (*fileHandle)(nil)
	_ fs.FileFsyncer  = (*fileHandle)(nil)
	_ fs.FileReleaser = (*fileHandle)(nil)
)

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.f.ReadAt(dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := h.f.WriteAt(data, off)
	if err != nil {
		return uint32(n), syscall.EIO
	}
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
	return uint32(n), 0
}

func (h *fileHandle) truncate(size int64) error {
	if err := h.f.Truncate(size); err != nil {
		return err
	}
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
	return nil
}

// upload pushes the local buffer to Drive if it was modified since the last
// upload, and updates the FileNode's metadata to match.
func (h *fileHandle) upload(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return 0
	}
	h.dirty = false
	h.mu.Unlock()

	fi, err := h.f.Stat()
	if err != nil {
		return syscall.EIO
	}
	plainSize := fi.Size()

	if _, err := h.f.Seek(0, io.SeekStart); err != nil {
		return syscall.EIO
	}
	src, _, _ := h.node.snapshot()

	var body io.Reader = h.f
	if src.Account.Cipher != nil {
		body = src.Account.Cipher.EncryptReader(h.f)
	}
	if _, err := src.Account.UploadContent(ctx, src.FileID, body); err != nil {
		return syscall.EIO
	}
	h.node.mu.Lock()
	// The local file's own size, not what UploadContent reports back: with
	// encryption on, that's the larger ciphertext size.
	h.node.size = plainSize
	h.node.mu.Unlock()
	return 0
}

func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	return h.upload(ctx)
}

func (h *fileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return h.upload(ctx)
}

func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	errno := h.upload(ctx)
	h.f.Close()
	return errno
}
