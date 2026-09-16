package unionfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Downloaded files are cached to disk under the OS temp dir, keyed by
// account + Drive file ID, so repeated reads (and re-mounts) don't
// re-download unchanged files. This is a size check, not a hash: good
// enough to avoid serving a stale file after most edits, not a guarantee.
var (
	downloadMu    sync.Mutex
	downloadState = map[string]*cacheEntry{}
)

type cacheEntry struct {
	once sync.Once
	path string
	err  error
}

func cacheRoot() (string, error) {
	dir := filepath.Join(os.TempDir(), "gdunion-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// cachePathFor returns the deterministic local path for src's content,
// creating the containing directory if needed, without touching the file
// itself. Used when we're about to write fresh content (create, truncate)
// and there's nothing worth downloading first.
func cachePathFor(src Source) (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	accDir := filepath.Join(root, src.Account.Name)
	if err := os.MkdirAll(accDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(accDir, src.FileID), nil
}

// ensureCached returns the local path to src's content, downloading it if
// necessary. expectedSize <= 0 (Google-native docs report no size) skips the
// staleness check and always trusts an existing cached copy.
func ensureCached(ctx context.Context, src Source, mimeType string, expectedSize int64) (string, error) {
	key := src.Account.Name + "/" + src.FileID

	downloadMu.Lock()
	entry, ok := downloadState[key]
	if !ok {
		entry = &cacheEntry{}
		downloadState[key] = entry
	}
	downloadMu.Unlock()

	entry.once.Do(func() {
		path, err := cachePathFor(src)
		if err != nil {
			entry.err = err
			return
		}

		if fi, err := os.Stat(path); err == nil {
			if expectedSize <= 0 || fi.Size() == expectedSize {
				entry.path = path
				return
			}
		}

		rc, err := src.Account.Open(ctx, src.FileID, mimeType)
		if err != nil {
			entry.err = err
			return
		}
		defer rc.Close()

		tmp := path + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			entry.err = err
			return
		}
		_, copyErr := io.Copy(f, rc)
		closeErr := f.Close()
		if copyErr != nil {
			os.Remove(tmp)
			entry.err = copyErr
			return
		}
		if closeErr != nil {
			os.Remove(tmp)
			entry.err = closeErr
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			entry.err = err
			return
		}
		entry.path = path
	})

	return entry.path, entry.err
}
