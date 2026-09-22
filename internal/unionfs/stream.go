package unionfs

import (
	"context"
	"io"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"gdriveunion/internal/gdrive"
)

// streamThreshold is the file size above which FileNode.Open serves reads
// directly from Drive via HTTP Range requests (see streamHandle) instead of
// downloading the whole file to the local cache first (see cache.go). Below
// it, the whole-file cache is simpler and already fast enough - the
// overhead of a Range request per block isn't worth it for anything that
// downloads quickly anyway.
const streamThreshold = 32 * 1024 * 1024 // 32MiB

// streamBlockSize is how much of the file streamHandle fetches per Range
// request, and the most it ever holds in memory at once. Bounds the number
// of Drive API calls a large sequential read costs (a naive per-syscall
// Range request could easily run into Drive's per-user rate limit on a
// multi-GB file) while keeping memory use independent of file size.
const streamBlockSize = 4 * 1024 * 1024 // 4MiB

// shouldStream reports whether FileNode.Open should hand back a streamHandle
// for this file rather than the usual local-cache-backed one.
func shouldStream(src Source, mimeType string, size int64) bool {
	if size < streamThreshold {
		return false
	}
	if gdrive.ExportSuffix(mimeType) != "" {
		// Google-native docs are generated on export, not read from stored
		// bytes - no reliable Range support, and they're capped well under
		// streamThreshold in practice anyway.
		return false
	}
	if src.Account.Cipher != nil {
		// gcrypt's fixed-size chunking (see internal/gcrypt) would in
		// principle let a plaintext byte range map to a precise ciphertext
		// range, but that translation isn't implemented yet - encrypted
		// files still go through the whole-file cache for now.
		return false
	}
	return true
}

// streamHandle serves reads for a large, unencrypted, non-exported file
// directly from Drive via ranged HTTP requests, instead of gdunion's usual
// whole-file-to-local-cache approach. It never touches disk and holds at
// most one streamBlockSize window of the file in memory at a time, so
// opening (or seeking around in) a huge file doesn't cost a full download
// up front or unbounded memory.
//
// This is read-only, and FileNode.Open only ever hands one out for a
// read-only open (see shouldStream/Open): gdunion has no way to push a
// partial byte range to Drive (there's no such API - any content update has
// to supply the complete new file, see gdrive.Account.UploadContent), so
// writes always go through the existing local-cache-buffer-then-upload-whole
// path regardless of file size.
//
// Each open handle keeps its own independent block window - concurrent
// opens of the same file don't share one, so two readers scanning the same
// huge file at once each pay for their own Range requests. Fine for a first
// cut; worth revisiting if that turns out to matter in practice.
type streamHandle struct {
	src      Source
	mimeType string
	size     int64 // file size in bytes; reads at or beyond this are EOF.

	mu       sync.Mutex
	blockOff int64
	block    []byte // currently buffered window; nil until the first Read.
}

var _ fs.FileReader = (*streamHandle)(nil)

func (h *streamHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	if h.block == nil || off < h.blockOff || off >= h.blockOff+int64(len(h.block)) {
		if err := h.fetchBlock(ctx, off); err != nil {
			return nil, syscall.EIO
		}
	}

	avail := h.block[off-h.blockOff:]
	n := len(dest)
	if n > len(avail) {
		n = len(avail)
	}
	copy(dest[:n], avail[:n])
	return fuse.ReadResultData(dest[:n]), 0
}

// fetchBlock replaces h.block with the streamBlockSize-sized window of the
// file starting at off (clamped to h.size). Called with h.mu held.
func (h *streamHandle) fetchBlock(ctx context.Context, off int64) error {
	end := off + streamBlockSize
	if end > h.size {
		end = h.size
	}

	rc, err := h.src.Account.OpenRange(ctx, h.src.FileID, off, end-1)
	if err != nil {
		return err
	}
	defer rc.Close()

	buf := make([]byte, end-off)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return err
	}
	h.block = buf
	h.blockOff = off
	return nil
}
