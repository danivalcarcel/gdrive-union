package unionfs

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"gdriveunion/internal/gcrypt"
	"gdriveunion/internal/gdrive"
)

// newFakeDriveAccount stands up a tiny HTTP server that serves content out
// of an in-memory buffer, honoring Range headers the same way Drive's own
// media download endpoint does, and wraps it in a real *gdrive.Account -
// so streamHandle/OpenRange are exercised through their actual code path,
// not a mock of them.
func newFakeDriveAccount(t *testing.T, fileID string, content []byte) *gdrive.Account {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/files/"+fileID, func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), int64(len(content)-1)
		if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
			if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		if start < 0 || end >= int64(len(content)) || start > end {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	svc, err := drive.NewService(ctx,
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
	)
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}
	return &gdrive.Account{Name: "test", Service: svc}
}

// TestStreamHandleReadMatchesSource exercises streamHandle end-to-end
// (through the real gdrive.Account.OpenRange, against a fake Drive server)
// with a file spanning several streamBlockSize-sized blocks, reading it back
// in varying, non-block-aligned chunk sizes and at both forward and backward
// (re-seeking) offsets, and checks every byte against the source content.
func TestStreamHandleReadMatchesSource(t *testing.T) {
	size := int64(streamBlockSize*2 + 12345) // spans 3 blocks, last one partial
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	acc := newFakeDriveAccount(t, "file1", content)
	h := &streamHandle{
		src:      Source{Account: acc, FileID: "file1"},
		mimeType: "application/octet-stream",
		size:     size,
	}

	readAt := func(off int64, n int) []byte {
		dest := make([]byte, n)
		res, errno := h.Read(context.Background(), dest, off)
		if errno != 0 {
			t.Fatalf("Read(off=%d, n=%d): errno %v", off, n, errno)
		}
		buf, status := res.Bytes(nil)
		if status != 0 {
			t.Fatalf("Read(off=%d, n=%d): ReadResult.Bytes status %v", off, n, status)
		}
		return buf
	}

	checkAt := func(off int64, n int) {
		got := readAt(off, n)
		end := off + int64(len(got))
		want := content[off:end]
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("byte mismatch at offset %d (read starting %d): got %02x want %02x", off+int64(i), off, got[i], want[i])
			}
		}
	}

	// Odd, non-block-aligned read size, walking sequentially across block
	// boundaries.
	const step = 70000
	for off := int64(0); off < size; off += step {
		n := step
		if off+int64(n) > size {
			n = int(size - off)
		}
		checkAt(off, n)
	}

	// Re-seek backward into an already-fetched block, and into an earlier
	// one, to make sure fetchBlock correctly refetches instead of trusting
	// stale bounds.
	checkAt(streamBlockSize-100, 200)   // straddles the first block boundary
	checkAt(0, 100)                     // back to the very start
	checkAt(streamBlockSize*2+100, 500) // into the final, partial block

	// Reading exactly at EOF returns nothing, not an error.
	if got := readAt(size, 10); len(got) != 0 {
		t.Fatalf("Read at EOF: got %d bytes, want 0", len(got))
	}

	// A read whose length would run past EOF is truncated, not padded/erred.
	got := readAt(size-5, 10)
	if len(got) != 5 {
		t.Fatalf("Read straddling EOF: got %d bytes, want 5", len(got))
	}
	want := content[size-5:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte mismatch at offset %d: got %02x want %02x", size-5+int64(i), got[i], want[i])
		}
	}
}

func TestShouldStream(t *testing.T) {
	plainAcc := &gdrive.Account{Name: "plain"}
	cipherAcc := &gdrive.Account{Name: "encrypted", Cipher: mustCipher(t)}

	cases := []struct {
		name     string
		src      Source
		mimeType string
		size     int64
		want     bool
	}{
		{"small plain file", Source{Account: plainAcc}, "application/octet-stream", streamThreshold - 1, false},
		{"large plain file", Source{Account: plainAcc}, "application/octet-stream", streamThreshold, true},
		{"large exported doc", Source{Account: plainAcc}, "application/vnd.google-apps.document", streamThreshold * 10, false},
		{"large encrypted file", Source{Account: cipherAcc}, "application/octet-stream", streamThreshold, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldStream(c.src, c.mimeType, c.size); got != c.want {
				t.Errorf("shouldStream(%q, size=%d) = %v, want %v", c.mimeType, c.size, got, c.want)
			}
		})
	}
}

func mustCipher(t *testing.T) *gcrypt.Cipher {
	t.Helper()
	key, err := gcrypt.GenerateKey()
	if err != nil {
		t.Fatalf("gcrypt.GenerateKey: %v", err)
	}
	c, err := gcrypt.New(key)
	if err != nil {
		t.Fatalf("gcrypt.New: %v", err)
	}
	return c
}
