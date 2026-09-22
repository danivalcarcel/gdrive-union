// Package gdrive is a thin wrapper around the Drive v3 API: listing
// children, reading file bytes, and reporting quota. It knows nothing about
// the union tree; internal/unionfs composes multiple accounts of this.
package gdrive

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"gdriveunion/internal/gcrypt"
)

const FolderMimeType = "application/vnd.google-apps.folder"

// exportMimeTypes maps Google-native document types (Docs/Sheets/Slides,
// which have no downloadable binary) to a reasonable exported format.
var exportMimeTypes = map[string]struct {
	mime string
	ext  string
}{
	"application/vnd.google-apps.document":     {"application/pdf", ".pdf"},
	"application/vnd.google-apps.spreadsheet":  {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
	"application/vnd.google-apps.presentation": {"application/pdf", ".pdf"},
}

// Account is one authenticated Google Drive account.
type Account struct {
	Name    string
	Service *drive.Service

	// Cipher is non-nil when this account has encryption enabled (see
	// internal/gcrypt and `gdunion crypt enable`). gdrive itself never
	// looks at it - it's just carried here so internal/unionfs, which
	// already threads *Account everywhere, has it in reach without extra
	// plumbing. Everything under an account with a non-nil Cipher is
	// assumed to be encrypted.
	Cipher *gcrypt.Cipher

	quotaMu    sync.Mutex
	quotaAt    time.Time
	quotaCache Quota
}

func NewAccount(ctx context.Context, name string, ts oauth2.TokenSource) (*Account, error) {
	svc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("creating drive client for %s: %w", name, err)
	}
	return &Account{Name: name, Service: svc}, nil
}

// Quota reports bytes used/available for this account ("" limit means
// unlimited, e.g. Workspace accounts).
type Quota struct {
	UsageBytes int64
	LimitBytes int64 // 0 == unlimited
}

func (a *Account) Quota(ctx context.Context) (Quota, error) {
	about, err := a.Service.About.Get().Fields("storageQuota").Context(ctx).Do()
	if err != nil {
		return Quota{}, fmt.Errorf("%s: fetching quota: %w", a.Name, err)
	}
	q := Quota{UsageBytes: about.StorageQuota.Usage}
	if about.StorageQuota.Limit > 0 {
		q.LimitBytes = about.StorageQuota.Limit
	}
	return q, nil
}

// FreeBytes returns available space, or a very large number for unlimited
// accounts so they sort last in "least full" but still win "most free".
func (q Quota) FreeBytes() int64 {
	if q.LimitBytes == 0 {
		return 1 << 62
	}
	free := q.LimitBytes - q.UsageBytes
	if free < 0 {
		return 0
	}
	return free
}

// freeSpaceTTL bounds how long a quota reading is trusted before writes
// re-check it; short enough to react to a account filling up mid-session,
// long enough that creating many small files doesn't cost an API call each.
const freeSpaceTTL = 60 * time.Second

// CachedQuota is like Quota but reuses a reading for up to freeSpaceTTL,
// for use on hot paths: deciding where to place a new file/folder, and
// aggregating space for `df` (see internal/unionfs's Statfs).
func (a *Account) CachedQuota(ctx context.Context) (Quota, error) {
	a.quotaMu.Lock()
	defer a.quotaMu.Unlock()
	if time.Since(a.quotaAt) >= freeSpaceTTL {
		q, err := a.Quota(ctx)
		if err != nil {
			return Quota{}, err
		}
		a.quotaCache = q
		a.quotaAt = time.Now()
	}
	return a.quotaCache, nil
}

// FreeSpace is CachedQuota(ctx).FreeBytes(), for callers that only care
// about the free-space number.
func (a *Account) FreeSpace(ctx context.Context) (int64, error) {
	q, err := a.CachedQuota(ctx)
	if err != nil {
		return 0, err
	}
	return q.FreeBytes(), nil
}

// Entry is one child returned by ListChildren.
type Entry struct {
	ID       string
	Name     string
	IsDir    bool
	MimeType string
	Size     int64
	ModTime  string // RFC3339, as returned by the API
}

const listFields = "nextPageToken, files(id, name, mimeType, size, modifiedTime)"

// ListChildren lists the direct children of parentID ("root" for the
// account's root folder), following pagination.
func (a *Account) ListChildren(ctx context.Context, parentID string) ([]Entry, error) {
	var entries []Entry
	q := a.Service.Files.List().
		Q(fmt.Sprintf("'%s' in parents and trashed = false", parentID)).
		Fields(listFields).
		PageSize(1000).
		Context(ctx)

	pageToken := ""
	for {
		if pageToken != "" {
			q = q.PageToken(pageToken)
		}
		res, err := q.Do()
		if err != nil {
			return nil, fmt.Errorf("%s: listing %s: %w", a.Name, parentID, err)
		}
		for _, f := range res.Files {
			entries = append(entries, Entry{
				ID:       f.Id,
				Name:     f.Name,
				IsDir:    f.MimeType == FolderMimeType,
				MimeType: f.MimeType,
				Size:     f.Size,
				ModTime:  f.ModifiedTime,
			})
		}
		pageToken = res.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return entries, nil
}

// Open returns a reader for the file's content, transparently exporting
// Google-native documents (which have no raw binary) to a fixed format.
// The caller must Close() the reader.
func (a *Account) Open(ctx context.Context, id, mimeType string) (io.ReadCloser, error) {
	if export, ok := exportMimeTypes[mimeType]; ok {
		res, err := a.Service.Files.Export(id, export.mime).Context(ctx).Download()
		if err != nil {
			return nil, fmt.Errorf("%s: exporting %s: %w", a.Name, id, err)
		}
		return res.Body, nil
	}
	res, err := a.Service.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("%s: downloading %s: %w", a.Name, id, err)
	}
	return res.Body, nil
}

// OpenRange returns a reader for just the byte range [start, end] (inclusive)
// of id's raw content, via an HTTP Range request - Drive replies with 206
// Partial Content, which the generated client treats like any other 2xx
// response, so this needs no special-casing beyond setting the header.
// Callers must not use this for a Google-native export's mimeType: the
// exported bytes are generated on the fly rather than read from storage, and
// don't reliably support partial requests the way a stored file's raw bytes
// do. The caller must Close() the reader.
func (a *Account) OpenRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	call := a.Service.Files.Get(id).Context(ctx)
	call.Header().Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	res, err := call.Download()
	if err != nil {
		return nil, fmt.Errorf("%s: downloading %s bytes %d-%d: %w", a.Name, id, start, end, err)
	}
	return res.Body, nil
}

// ExportSuffix returns the filename suffix change needed for a Google-native
// doc (e.g. a Sheet becomes "<name>.xlsx"), or "" if the file downloads as-is.
func ExportSuffix(mimeType string) string {
	if export, ok := exportMimeTypes[mimeType]; ok {
		return export.ext
	}
	return ""
}

const writeFields = "id, name, mimeType, size, modifiedTime, parents"

// CreateFolder creates a new, empty folder under parentID.
func (a *Account) CreateFolder(ctx context.Context, parentID, name string) (Entry, error) {
	f := &drive.File{Name: name, MimeType: FolderMimeType, Parents: []string{parentID}}
	created, err := a.Service.Files.Create(f).Fields(writeFields).Context(ctx).Do()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: creating folder %q: %w", a.Name, name, err)
	}
	return entryFromFile(created), nil
}

// CreateFile creates a new, empty file under parentID; content is uploaded
// separately via UploadContent once the caller has bytes to write.
func (a *Account) CreateFile(ctx context.Context, parentID, name, mimeType string) (Entry, error) {
	f := &drive.File{Name: name, MimeType: mimeType, Parents: []string{parentID}}
	created, err := a.Service.Files.Create(f).Fields(writeFields).Context(ctx).Do()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: creating file %q: %w", a.Name, name, err)
	}
	return entryFromFile(created), nil
}

// UploadContent replaces id's content with r, read from the start.
func (a *Account) UploadContent(ctx context.Context, id string, r io.Reader) (Entry, error) {
	updated, err := a.Service.Files.Update(id, &drive.File{}).Media(r).Fields(writeFields).Context(ctx).Do()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: uploading content for %s: %w", a.Name, id, err)
	}
	return entryFromFile(updated), nil
}

// Rename changes id's name in place (no parent change).
func (a *Account) Rename(ctx context.Context, id, newName string) error {
	_, err := a.Service.Files.Update(id, &drive.File{Name: newName}).Fields(writeFields).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("%s: renaming %s to %q: %w", a.Name, id, newName, err)
	}
	return nil
}

// Move renames id and/or moves it from oldParent to newParent in one call
// (both parents are within this same account; Drive has no notion of moving
// a file between different accounts).
func (a *Account) Move(ctx context.Context, id, newName, oldParent, newParent string) error {
	_, err := a.Service.Files.Update(id, &drive.File{Name: newName}).
		AddParents(newParent).
		RemoveParents(oldParent).
		Fields(writeFields).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("%s: moving %s: %w", a.Name, id, err)
	}
	return nil
}

// Trash moves id to the account's trash (recoverable), rather than
// permanently deleting it.
func (a *Account) Trash(ctx context.Context, id string) error {
	_, err := a.Service.Files.Update(id, &drive.File{Trashed: true}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("%s: trashing %s: %w", a.Name, id, err)
	}
	return nil
}

// EnsureFolder finds a folder named `name` directly under parentID,
// creating it if it doesn't exist yet. Used to scope gdunion to one
// dedicated app folder per account instead of the account's entire Drive.
func (a *Account) EnsureFolder(ctx context.Context, parentID, name string) (Entry, error) {
	escaped := strings.ReplaceAll(name, `'`, `\'`)
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false", escaped, parentID, FolderMimeType)
	res, err := a.Service.Files.List().Q(q).Fields("files(id, name, mimeType, size, modifiedTime)").PageSize(1).Context(ctx).Do()
	if err != nil {
		return Entry{}, fmt.Errorf("%s: looking up folder %q: %w", a.Name, name, err)
	}
	if len(res.Files) > 0 {
		return entryFromFile(res.Files[0]), nil
	}
	return a.CreateFolder(ctx, parentID, name)
}

func entryFromFile(f *drive.File) Entry {
	return Entry{
		ID:       f.Id,
		Name:     f.Name,
		IsDir:    f.MimeType == FolderMimeType,
		MimeType: f.MimeType,
		Size:     f.Size,
		ModTime:  f.ModifiedTime,
	}
}
