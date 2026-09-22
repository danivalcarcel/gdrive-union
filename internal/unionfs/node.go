// Package unionfs merges several Google Drive accounts into a single
// read-only directory tree: each virtual directory can be backed by more
// than one account's folder (their children get merged), while each virtual
// file always maps to exactly one account's file. Name collisions between
// two different files (or a file and a folder) are resolved by suffixing
// the account name onto whichever entry loses the plain name.
package unionfs

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"gdriveunion/internal/gcrypt"
	"gdriveunion/internal/gdrive"
)

// listTTL bounds how long a directory listing is trusted before we ask
// Drive again; short enough that other clients' changes show up reasonably
// fast, long enough that `ls -la` on a big tree doesn't refetch per-entry.
const listTTL = 20 * time.Second

// Drive has no notion of Unix ownership, so every entry is reported as
// owned by whoever is running gdunion. Leaving this at the zero value would
// make everything appear root-owned, which makes tools like `rm` treat
// perfectly writable files as "write-protected".
var (
	mountUID = uint32(os.Getuid())
	mountGID = uint32(os.Getgid())
)

// Source identifies one (account, Drive file/folder ID) pair.
type Source struct {
	Account *gdrive.Account
	FileID  string
}

type childInfo struct {
	isDir bool

	// file case
	fileSrc  Source
	mimeType string
	size     int64
	modTime  time.Time

	// dir case: every account folder that contributes children here
	dirSources []Source
}

// DirNode is a virtual directory backed by one or more accounts' folders.
type DirNode struct {
	fs.Inode

	sources []Source

	mu       sync.Mutex
	cachedAt time.Time
	children map[string]childInfo
}

var (
	_ fs.NodeLookuper  = (*DirNode)(nil)
	_ fs.NodeReaddirer = (*DirNode)(nil)
	_ fs.NodeGetattrer = (*DirNode)(nil)
	_ fs.NodeMkdirer   = (*DirNode)(nil)
	_ fs.NodeCreater   = (*DirNode)(nil)
	_ fs.NodeUnlinker  = (*DirNode)(nil)
	_ fs.NodeRmdirer   = (*DirNode)(nil)
	_ fs.NodeRenamer   = (*DirNode)(nil)
	_ fs.NodeStatfser  = (*DirNode)(nil)
)

// NewRoot builds the tree root, one virtual directory merging the given
// per-account folders (typically each account's dedicated app folder,
// resolved by the caller - see gdrive.Account.EnsureFolder - rather than
// that account's actual Drive root, so gdunion doesn't expose or write
// among a user's pre-existing, unrelated Drive content).
func NewRoot(sources []Source) *DirNode {
	return &DirNode{sources: sources}
}

func (n *DirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755
	out.Nlink = 1
	out.Uid = mountUID
	out.Gid = mountGID
	// Directories don't map to a single real mtime (they can merge folders
	// from several accounts) - report "now" rather than leaving it at the
	// zero value, which tools display as 1970-01-01 and read as "no info".
	now := time.Now()
	out.SetTimes(&now, &now, &now)
	return 0
}

// statfsBlockSize is an arbitrary, conventional block size for the
// synthetic values below; only their ratios (used vs. total) matter to
// tools like `df`.
const statfsBlockSize = 4096

// unlimitedStatfsBlocks stands in for "no meaningful ceiling" (e.g. a
// Google Workspace account with unlimited storage): a large but safely
// summable number of blocks, rather than reusing gdrive.Quota.FreeBytes'
// own sentinel (which is sized for single-account comparisons and would
// risk overflowing a uint64 total once multiple accounts are added up).
const unlimitedStatfsBlocks = 1 << 40 // ~4.5PB at 4096-byte blocks

// Statfs reports aggregate space across every account backing this
// directory, so `df` shows real numbers instead of the default zeroes.
// Best-effort: an account whose quota can't be fetched right now is
// skipped rather than failing the whole call.
func (n *DirNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	seen := make(map[*gdrive.Account]bool, len(n.sources))
	var totalBytes, freeBytes int64
	unlimited := false

	for _, src := range n.sources {
		if seen[src.Account] {
			continue
		}
		seen[src.Account] = true

		q, err := src.Account.CachedQuota(ctx)
		if err != nil {
			continue
		}
		if q.LimitBytes == 0 {
			unlimited = true
			continue
		}
		totalBytes += q.LimitBytes
		if free := q.LimitBytes - q.UsageBytes; free > 0 {
			freeBytes += free
		}
	}

	if unlimited {
		out.Blocks = unlimitedStatfsBlocks
		out.Bfree = unlimitedStatfsBlocks
	} else {
		out.Blocks = uint64(totalBytes) / statfsBlockSize
		out.Bfree = uint64(freeBytes) / statfsBlockSize
	}
	out.Bavail = out.Bfree
	out.Bsize = statfsBlockSize
	out.Frsize = statfsBlockSize
	out.NameLen = 255
	return 0
}

// refresh re-lists every contributing account folder and rebuilds the
// merged children map, unless a cached copy younger than listTTL exists.
func (n *DirNode) refresh(ctx context.Context) (map[string]childInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.children != nil && time.Since(n.cachedAt) < listTTL {
		return n.children, nil
	}

	// Each source is an independent Drive API round-trip, so fetch them all
	// concurrently rather than paying their latencies one after another - a
	// directory merging N accounts would otherwise take N times as long to
	// refresh as one backed by a single account. Results are merged
	// afterwards in n.sources order (unchanged from the sequential version),
	// since that order is what decides which account keeps a colliding name
	// (see mergeEntry/disambiguate).
	type listResult struct {
		entries []gdrive.Entry
		err     error
	}
	results := make([]listResult, len(n.sources))
	var wg sync.WaitGroup
	for i, src := range n.sources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			entries, err := src.Account.ListChildren(ctx, src.FileID)
			results[i] = listResult{entries: entries, err: err}
		}(i, src)
	}
	wg.Wait()

	children := make(map[string]childInfo)
	for i, src := range n.sources {
		if results[i].err != nil {
			return nil, results[i].err
		}
		for _, e := range results[i].entries {
			mergeEntry(children, src.Account, e)
		}
	}

	n.children = children
	n.cachedAt = time.Now()
	return children, nil
}

func mergeEntry(children map[string]childInfo, acc *gdrive.Account, e gdrive.Entry) {
	name := e.Name
	size := e.Size
	if acc.Cipher != nil {
		// Trial-decrypt: a name that doesn't decrypt (ok == false) predates
		// encryption being turned on for this account, or isn't ours - use
		// it as-is rather than hiding or erroring on it. See gcrypt's doc
		// comment for why this trial decrypt is safe, not just probabilistic.
		if plain, ok := acc.Cipher.DecryptName(e.Name); ok {
			name = plain
			// Google-native docs (Sheets/Docs/Slides) are never something
			// gdunion encrypted - their content is exported, not stored by
			// us - so their reported size is already the real one.
			if !e.IsDir && gdrive.ExportSuffix(e.MimeType) == "" {
				if plainSize, err := gcrypt.PlaintextSize(e.Size); err == nil {
					size = plainSize
				}
			}
		}
	}

	existing, collides := children[name]

	if collides && existing.isDir && e.IsDir {
		// Same-named folder from another account: merge, don't rename.
		existing.dirSources = append(existing.dirSources, Source{Account: acc, FileID: e.ID})
		children[name] = existing
		return
	}

	if collides {
		// Either two files, or a file/folder clash: the entry we're adding
		// now loses the plain name. Whoever was inserted first (accounts
		// are processed in a fixed, alphabetical order) keeps it.
		name = disambiguate(name, acc.Name, children)
	}

	if e.IsDir {
		children[name] = childInfo{isDir: true, dirSources: []Source{{Account: acc, FileID: e.ID}}}
		return
	}

	modTime, _ := time.Parse(time.RFC3339, e.ModTime)
	children[name] = childInfo{
		isDir:    false,
		fileSrc:  Source{Account: acc, FileID: e.ID},
		mimeType: e.MimeType,
		size:     size,
		modTime:  modTime,
	}
}

func disambiguate(name, accountName string, existing map[string]childInfo) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := fmt.Sprintf("%s (%s)%s", base, accountName, ext)
	for i := 2; ; i++ {
		if _, taken := existing[candidate]; !taken {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%s %d)%s", base, accountName, i, ext)
	}
}

func (n *DirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, err := n.refresh(ctx)
	if err != nil {
		return nil, errnoFor(err)
	}
	info, ok := children[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	return n.inodeFor(ctx, name, info, out), 0
}

func (n *DirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, err := n.refresh(ctx)
	if err != nil {
		return nil, errnoFor(err)
	}
	entries := make([]fuse.DirEntry, 0, len(children))
	for name, info := range children {
		mode := uint32(fuse.S_IFREG)
		if info.isDir {
			mode = fuse.S_IFDIR
		}
		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode, Ino: nameIno(name)})
	}
	return fs.NewListDirStream(entries), 0
}

// nameIno derives a stable, non-zero inode number for a directory entry from
// its name, for the entry list returned by Readdir. A zero Ino (the default
// for a DirEntry that doesn't set one) is POSIX shorthand for "deleted, treat
// as absent" - readdir() wrappers as well-established as glibc's, and Samba's
// own directory-listing code, silently drop any entry reported that way. The
// exact value doesn't need to be globally stable across calls (that's what
// NodeLookuper/inodeFor's real StableAttr is for) - it only has to be
// non-zero and cheap, since this is purely to keep those callers from
// mistaking a live entry for a deleted one.
func nameIno(name string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	sum := h.Sum64()
	if sum == 0 {
		return 1
	}
	return sum
}

// inodeFor builds the child inode for a directory entry. Its StableAttr.Ino
// must be derived from name via nameIno the same way Readdir's DirEntry.Ino
// is - go-fuse otherwise auto-assigns an unrelated internal number here,
// which won't match what Readdir already reported for that name. Samba's
// directory-listing code (openat_pathref_fsp, added as a TOCTOU hardening
// check) re-resolves each entry and rejects it outright if the inode number
// it gets back doesn't match the one from the initial listing - so a mismatch
// here silently drops every single entry from an SMB client's view, while a
// plain `ls` (which never cross-checks inode numbers this way) shows them
// fine.
func (n *DirNode) inodeFor(ctx context.Context, name string, info childInfo, out *fuse.EntryOut) *fs.Inode {
	out.SetAttrTimeout(listTTL)
	out.SetEntryTimeout(listTTL)
	out.Uid = mountUID
	out.Gid = mountGID
	// An unset Nlink defaults to 0, which POSIX conventionally reads as
	// "unlinked/deleted" - Samba's own directory-listing code checks for
	// exactly that (VALID_STAT) and silently drops any entry reporting it,
	// which is why every entry vanished over SMB despite `ls` showing them
	// fine (a plain stat(2) caller has no reason to make that check).
	out.Nlink = 1
	ino := nameIno(name)

	if info.isDir {
		out.Mode = fuse.S_IFDIR | 0o755
		now := time.Now()
		out.SetTimes(&now, &now, &now)
		child := &DirNode{sources: info.dirSources}
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR, Ino: ino})
	}

	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(info.size)
	out.SetTimes(nil, &info.modTime, nil)
	child := &FileNode{src: info.fileSrc, mimeType: info.mimeType, size: info.size, modTime: info.modTime}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG, Ino: ino})
}

// errnoFor maps a generic (Drive API) error to a FUSE errno; we don't try to
// distinguish causes here (auth failure, network, quota) beyond surfacing
// them as I/O errors, since the underlying error is still logged by the
// caller chain via go-fuse's debug logging.
func errnoFor(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	return syscall.EIO
}

// putChild records a newly created/moved entry directly in the cache
// (rather than waiting out listTTL and re-listing), since Drive gives no
// read-your-writes guarantee on Files.list.
func (n *DirNode) putChild(name string, info childInfo) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.children == nil {
		n.children = map[string]childInfo{}
	}
	n.children[name] = info
}

func (n *DirNode) deleteChild(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.children, name)
}

// parentIDFor returns the folder ID this directory maps to in acc, if acc
// is one of its contributing accounts.
func (n *DirNode) parentIDFor(acc *gdrive.Account) (string, bool) {
	for _, s := range n.sources {
		if s.Account == acc {
			return s.FileID, true
		}
	}
	return "", false
}

// pickWriteTarget chooses which contributing account should receive a new
// file or folder created in this directory: whichever has the most free
// space right now. Only accounts that already have a folder here are
// eligible, since a new entry has to land inside an existing folder.
func (n *DirNode) pickWriteTarget(ctx context.Context) (Source, syscall.Errno) {
	if len(n.sources) == 0 {
		return Source{}, syscall.EROFS
	}
	best := n.sources[0]
	bestFree, _ := best.Account.FreeSpace(ctx)
	for _, s := range n.sources[1:] {
		free, err := s.Account.FreeSpace(ctx)
		if err != nil {
			continue
		}
		if free > bestFree {
			bestFree = free
			best = s
		}
	}
	return best, 0
}

func guessMimeType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (n *DirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, err := n.refresh(ctx)
	if err != nil {
		return nil, errnoFor(err)
	}
	if _, exists := children[name]; exists {
		return nil, syscall.EEXIST
	}

	target, errno := n.pickWriteTarget(ctx)
	if errno != 0 {
		return nil, errno
	}
	remoteName := name
	if target.Account.Cipher != nil {
		remoteName = target.Account.Cipher.EncryptName(name)
	}
	created, err := target.Account.CreateFolder(ctx, target.FileID, remoteName)
	if err != nil {
		return nil, errnoFor(err)
	}

	info := childInfo{isDir: true, dirSources: []Source{{Account: target.Account, FileID: created.ID}}}
	n.putChild(name, info)
	return n.inodeFor(ctx, name, info, out), 0
}

// Create makes a new, empty file in Drive immediately (so it shows up in
// listings right away) and hands back a writable local-cache-backed handle;
// content is uploaded when the handle is flushed or released.
func (n *DirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	children, err := n.refresh(ctx)
	if err != nil {
		return nil, nil, 0, errnoFor(err)
	}
	if _, exists := children[name]; exists {
		return nil, nil, 0, syscall.EEXIST
	}

	target, errno := n.pickWriteTarget(ctx)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	mimeType := guessMimeType(name)
	remoteName := name
	if target.Account.Cipher != nil {
		remoteName = target.Account.Cipher.EncryptName(name)
	}
	created, err := target.Account.CreateFile(ctx, target.FileID, remoteName, mimeType)
	if err != nil {
		return nil, nil, 0, errnoFor(err)
	}

	src := Source{Account: target.Account, FileID: created.ID}
	modTime := time.Now()
	n.putChild(name, childInfo{isDir: false, fileSrc: src, mimeType: mimeType, size: 0, modTime: modTime})

	path, err := cachePathFor(src)
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}

	fileNode := &FileNode{src: src, mimeType: mimeType, size: 0, modTime: modTime}
	out.Mode = fuse.S_IFREG | 0o644
	out.Uid = mountUID
	out.Gid = mountGID
	out.Nlink = 1
	out.SetAttrTimeout(listTTL)
	out.SetEntryTimeout(listTTL)
	inode := n.NewInode(ctx, fileNode, fs.StableAttr{Mode: fuse.S_IFREG, Ino: nameIno(name)})
	return inode, &fileHandle{f: f, node: fileNode}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *DirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	children, err := n.refresh(ctx)
	if err != nil {
		return errnoFor(err)
	}
	info, ok := children[name]
	if !ok {
		return syscall.ENOENT
	}
	if info.isDir {
		return syscall.EISDIR
	}
	if err := info.fileSrc.Account.Trash(ctx, info.fileSrc.FileID); err != nil {
		return errnoFor(err)
	}
	n.deleteChild(name)
	return 0
}

func (n *DirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	children, err := n.refresh(ctx)
	if err != nil {
		return errnoFor(err)
	}
	info, ok := children[name]
	if !ok {
		return syscall.ENOENT
	}
	if !info.isDir {
		return syscall.ENOTDIR
	}

	// Each source's emptiness check is an independent Drive API round-trip;
	// run them concurrently rather than one after another so Rmdir on a
	// folder merging several accounts doesn't take as many round-trips'
	// worth of latency as it has sources. Cancel the rest as soon as one
	// source is found non-empty, since that already settles the call.
	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]syscall.Errno, len(info.dirSources))
	var wg sync.WaitGroup
	for i, src := range info.dirSources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			kids, err := src.Account.ListChildren(checkCtx, src.FileID)
			switch {
			case len(kids) > 0 && err == nil:
				results[i] = syscall.ENOTEMPTY
				cancel()
			case err != nil:
				results[i] = errnoFor(err)
			}
		}(i, src)
	}
	wg.Wait()

	// ENOTEMPTY (the normal reason this check fails) takes priority over any
	// other source's error, since cancelling their in-flight requests above
	// can itself surface as an error that has nothing to do with the real
	// answer.
	for _, errno := range results {
		if errno == syscall.ENOTEMPTY {
			return syscall.ENOTEMPTY
		}
	}
	for _, errno := range results {
		if errno != 0 {
			return errno
		}
	}

	for _, src := range info.dirSources {
		if err := src.Account.Trash(ctx, src.FileID); err != nil {
			return errnoFor(err)
		}
	}
	n.deleteChild(name)
	return 0
}

// moveFileSource relocates/renames a file. When the destination directory
// already has a folder in the same account, it's a native Drive move;
// otherwise (moving across two different Google accounts, which have
// disjoint file ID spaces) we copy the bytes through this machine into
// whichever destination account has the most free space, then trash the
// original.
func (n *DirNode) moveFileSource(ctx context.Context, src Source, mimeType string, destDir *DirNode, newName string) (Source, syscall.Errno) {
	oldParentID, _ := n.parentIDFor(src.Account)
	if newParentID, ok := destDir.parentIDFor(src.Account); ok {
		// Same account: stays under the same key, if any.
		remoteName := newName
		if src.Account.Cipher != nil {
			remoteName = src.Account.Cipher.EncryptName(newName)
		}
		if oldParentID != newParentID {
			if err := src.Account.Move(ctx, src.FileID, remoteName, oldParentID, newParentID); err != nil {
				return Source{}, errnoFor(err)
			}
		} else if err := src.Account.Rename(ctx, src.FileID, remoteName); err != nil {
			return Source{}, errnoFor(err)
		}
		return src, 0
	}

	// Cross-account: no server-side move between accounts, so copy the
	// bytes through this machine. Re-encrypt along the way for whichever
	// side (source, destination, both or neither) has encryption enabled,
	// rather than assuming they match.
	target, errno := destDir.pickWriteTarget(ctx)
	if errno != 0 {
		return Source{}, errno
	}
	rc, err := src.Account.Open(ctx, src.FileID, mimeType)
	if err != nil {
		return Source{}, errnoFor(err)
	}
	defer rc.Close()

	var body io.Reader = rc
	isExport := gdrive.ExportSuffix(mimeType) != ""
	if !isExport && src.Account.Cipher != nil {
		body = src.Account.Cipher.OpenReader(body)
	}
	if !isExport && target.Account.Cipher != nil {
		body = target.Account.Cipher.EncryptReader(body)
	}

	remoteName := newName
	if target.Account.Cipher != nil {
		remoteName = target.Account.Cipher.EncryptName(newName)
	}
	created, err := target.Account.CreateFile(ctx, target.FileID, remoteName, mimeType)
	if err != nil {
		return Source{}, errnoFor(err)
	}
	if _, err := target.Account.UploadContent(ctx, created.ID, body); err != nil {
		return Source{}, errnoFor(err)
	}
	if err := src.Account.Trash(ctx, src.FileID); err != nil {
		return Source{}, errnoFor(err)
	}
	return Source{Account: target.Account, FileID: created.ID}, 0
}

// moveFolderSource only supports moving within the same account: a
// cross-account folder move would need a recursive copy of its whole
// subtree, which v1 doesn't attempt.
func (n *DirNode) moveFolderSource(ctx context.Context, src Source, destDir *DirNode, newName string) (Source, syscall.Errno) {
	oldParentID, _ := n.parentIDFor(src.Account)
	newParentID, ok := destDir.parentIDFor(src.Account)
	if !ok {
		return Source{}, syscall.ENOTSUP
	}
	remoteName := newName
	if src.Account.Cipher != nil {
		remoteName = src.Account.Cipher.EncryptName(newName)
	}
	if oldParentID != newParentID {
		if err := src.Account.Move(ctx, src.FileID, remoteName, oldParentID, newParentID); err != nil {
			return Source{}, errnoFor(err)
		}
	} else if err := src.Account.Rename(ctx, src.FileID, remoteName); err != nil {
		return Source{}, errnoFor(err)
	}
	return src, 0
}

func (n *DirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	newDir, ok := newParent.(*DirNode)
	if !ok {
		return syscall.EINVAL
	}

	children, err := n.refresh(ctx)
	if err != nil {
		return errnoFor(err)
	}
	info, ok := children[name]
	if !ok {
		return syscall.ENOENT
	}

	if n == newDir && name == newName {
		return 0
	}

	destChildren, err := newDir.refresh(ctx)
	if err != nil {
		return errnoFor(err)
	}
	if existing, exists := destChildren[newName]; exists {
		switch {
		case info.isDir && !existing.isDir:
			return syscall.ENOTDIR
		case !info.isDir && existing.isDir:
			return syscall.EISDIR
		case info.isDir && existing.isDir:
			return syscall.EEXIST
		default:
			// File replacing file: the safe-save pattern most editors use
			// (write a temp file, then rename it over the original).
			if err := existing.fileSrc.Account.Trash(ctx, existing.fileSrc.FileID); err != nil {
				return errnoFor(err)
			}
			newDir.deleteChild(newName)
		}
	}

	if info.isDir {
		moved := make([]Source, 0, len(info.dirSources))
		for _, src := range info.dirSources {
			m, errno := n.moveFolderSource(ctx, src, newDir, newName)
			if errno != 0 {
				return errno
			}
			moved = append(moved, m)
		}
		n.deleteChild(name)
		newDir.putChild(newName, childInfo{isDir: true, dirSources: moved})
		return 0
	}

	moved, errno := n.moveFileSource(ctx, info.fileSrc, info.mimeType, newDir, newName)
	if errno != 0 {
		return errno
	}
	n.deleteChild(name)
	newDir.putChild(newName, childInfo{isDir: false, fileSrc: moved, mimeType: info.mimeType, size: info.size, modTime: info.modTime})
	return 0
}
