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
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

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
)

// NewRoot builds the tree root, one virtual directory merging every
// account's Drive root.
func NewRoot(accounts []*gdrive.Account) *DirNode {
	sources := make([]Source, len(accounts))
	for i, a := range accounts {
		sources[i] = Source{Account: a, FileID: "root"}
	}
	return &DirNode{sources: sources}
}

func (n *DirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755
	out.Nlink = 1
	out.Uid = mountUID
	out.Gid = mountGID
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

	children := make(map[string]childInfo)
	for _, src := range n.sources {
		entries, err := src.Account.ListChildren(ctx, src.FileID)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			mergeEntry(children, src.Account, e)
		}
	}

	n.children = children
	n.cachedAt = time.Now()
	return children, nil
}

func mergeEntry(children map[string]childInfo, acc *gdrive.Account, e gdrive.Entry) {
	name := e.Name
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
		size:     e.Size,
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
	return n.inodeFor(ctx, info, out), 0
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
		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *DirNode) inodeFor(ctx context.Context, info childInfo, out *fuse.EntryOut) *fs.Inode {
	out.SetAttrTimeout(listTTL)
	out.SetEntryTimeout(listTTL)
	out.Uid = mountUID
	out.Gid = mountGID

	if info.isDir {
		out.Mode = fuse.S_IFDIR | 0o755
		child := &DirNode{sources: info.dirSources}
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR})
	}

	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(info.size)
	out.SetTimes(nil, &info.modTime, nil)
	child := &FileNode{src: info.fileSrc, mimeType: info.mimeType, size: info.size, modTime: info.modTime}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG})
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
	created, err := target.Account.CreateFolder(ctx, target.FileID, name)
	if err != nil {
		return nil, errnoFor(err)
	}

	info := childInfo{isDir: true, dirSources: []Source{{Account: target.Account, FileID: created.ID}}}
	n.putChild(name, info)
	return n.inodeFor(ctx, info, out), 0
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
	created, err := target.Account.CreateFile(ctx, target.FileID, name, mimeType)
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
	out.SetAttrTimeout(listTTL)
	out.SetEntryTimeout(listTTL)
	inode := n.NewInode(ctx, fileNode, fs.StableAttr{Mode: fuse.S_IFREG})
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
	for _, src := range info.dirSources {
		kids, err := src.Account.ListChildren(ctx, src.FileID)
		if err != nil {
			return errnoFor(err)
		}
		if len(kids) > 0 {
			return syscall.ENOTEMPTY
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
		if oldParentID != newParentID {
			if err := src.Account.Move(ctx, src.FileID, newName, oldParentID, newParentID); err != nil {
				return Source{}, errnoFor(err)
			}
		} else if err := src.Account.Rename(ctx, src.FileID, newName); err != nil {
			return Source{}, errnoFor(err)
		}
		return src, 0
	}

	target, errno := destDir.pickWriteTarget(ctx)
	if errno != 0 {
		return Source{}, errno
	}
	rc, err := src.Account.Open(ctx, src.FileID, mimeType)
	if err != nil {
		return Source{}, errnoFor(err)
	}
	defer rc.Close()
	created, err := target.Account.CreateFile(ctx, target.FileID, newName, mimeType)
	if err != nil {
		return Source{}, errnoFor(err)
	}
	if _, err := target.Account.UploadContent(ctx, created.ID, rc); err != nil {
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
	if oldParentID != newParentID {
		if err := src.Account.Move(ctx, src.FileID, newName, oldParentID, newParentID); err != nil {
			return Source{}, errnoFor(err)
		}
	} else if err := src.Account.Rename(ctx, src.FileID, newName); err != nil {
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
