package gcrypt

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNameRoundTrip(t *testing.T) {
	c := testCipher(t)
	for _, name := range []string{"", "a", "report.pdf", "informe (trabajo).pdf", "日本語のファイル名.txt"} {
		enc := c.EncryptName(name)
		if enc == name {
			t.Fatalf("EncryptName(%q) did not change the name", name)
		}
		dec, ok := c.DecryptName(enc)
		if !ok {
			t.Fatalf("DecryptName(%q) (from EncryptName(%q)) returned ok=false", enc, name)
		}
		if dec != name {
			t.Fatalf("round trip mismatch: got %q, want %q", dec, name)
		}
	}
}

func TestDecryptNamePlaintextFallback(t *testing.T) {
	c := testCipher(t)
	for _, raw := range []string{"", "plain.txt", "not-base64!!", "gdrive-union-personal"} {
		if _, ok := c.DecryptName(raw); ok {
			t.Fatalf("DecryptName(%q) unexpectedly returned ok=true for a plaintext-looking name", raw)
		}
	}
}

func TestContentRoundTrip(t *testing.T) {
	c := testCipher(t)
	sizes := []int{0, 1, 100, chunkPlain - 1, chunkPlain, chunkPlain + 1, chunkPlain*2 + 500}
	for _, size := range sizes {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}

		ciphertext, err := io.ReadAll(c.EncryptReader(bytes.NewReader(plain)))
		if err != nil {
			t.Fatalf("size %d: encrypting: %v", size, err)
		}

		gotSize, err := PlaintextSize(int64(len(ciphertext)))
		if err != nil {
			t.Fatalf("size %d: PlaintextSize: %v", size, err)
		}
		if gotSize != int64(size) {
			t.Fatalf("size %d: PlaintextSize(%d) = %d, want %d", size, len(ciphertext), gotSize, size)
		}

		decrypted, err := io.ReadAll(c.OpenReader(bytes.NewReader(ciphertext)))
		if err != nil {
			t.Fatalf("size %d: decrypting: %v", size, err)
		}
		if !bytes.Equal(decrypted, plain) {
			t.Fatalf("size %d: round trip mismatch (got %d bytes, want %d)", size, len(decrypted), len(plain))
		}
	}
}

func TestOpenReaderPassthroughForPlaintext(t *testing.T) {
	c := testCipher(t)
	for _, plain := range [][]byte{{}, []byte("a"), []byte("abc"), []byte("hello, this is a plain file"), bytes.Repeat([]byte("x"), 200000)} {
		got, err := io.ReadAll(c.OpenReader(bytes.NewReader(plain)))
		if err != nil {
			t.Fatalf("OpenReader passthrough: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("passthrough mismatch: got %d bytes, want %d", len(got), len(plain))
		}
	}
}

func TestWrongKeyFailsClosed(t *testing.T) {
	c1 := testCipher(t)
	c2 := testCipher(t)

	if _, ok := c2.DecryptName(c1.EncryptName("secret.txt")); ok {
		t.Fatal("DecryptName succeeded with the wrong key")
	}

	plain := []byte("some file content")
	ciphertext, err := io.ReadAll(c1.EncryptReader(bytes.NewReader(plain)))
	if err != nil {
		t.Fatalf("encrypting: %v", err)
	}
	if _, err := io.ReadAll(c2.OpenReader(bytes.NewReader(ciphertext))); err == nil {
		t.Fatal("decrypting with the wrong key unexpectedly succeeded")
	}
}
