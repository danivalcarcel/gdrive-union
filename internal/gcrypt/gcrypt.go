// Package gcrypt provides optional, per-account encryption for gdunion:
// file content and filenames are encrypted before ever leaving this
// machine, so neither Google nor anyone with access to the Drive UI can
// read them - only whoever holds the local key file can. It knows nothing
// about Drive or the union tree, just byte streams and strings in, byte
// streams and strings out; internal/unionfs decides when to use it.
//
// Two independent subkeys are derived (via HKDF-SHA256) from one 32-byte
// master key: one for filenames, one for file content, so a weakness in
// one doesn't directly expose the other's key material.
//
// Both names and file content are self-describing: each starts with a
// fixed magic marker, so DecryptName/OpenReader can tell "this is
// something gdunion encrypted" apart from a plaintext name/file left over
// from before encryption was turned on (or created by something else
// entirely) and fall back to using it as-is, rather than failing outright.
// A trial decrypt is safe here (no meaningful false-positive risk) because
// secretbox authenticates every chunk - garbage input fails verification
// long before it could be mistaken for real plaintext.
//
// File content is encrypted in fixed-size chunks, each sealed
// independently with NaCl secretbox (XSalsa20-Poly1305) and a random
// 24-byte nonce - large enough that nonce reuse across chunks/files isn't a
// practical concern, so no counter-nonce bookkeeping is needed. Chunking
// (rather than one seal over the whole file) keeps memory use bounded
// regardless of file size, and each chunk is self-framed with a 4-byte
// big-endian length prefix, so encrypting/decrypting is a straight
// streaming io.Reader wrap with no separate index or footer.
package gcrypt

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	KeySize   = 32
	nonceSize = 24

	// chunkPlain is the amount of plaintext sealed per chunk; chunkOverhead
	// is the fixed bytes added per chunk (4-byte length prefix + nonce +
	// secretbox's Poly1305 tag).
	chunkPlain    = 64 * 1024
	chunkOverhead = 4 + nonceSize + secretbox.Overhead
	chunkCipher   = chunkPlain + chunkOverhead

	nameMagic    = "GDN1"
	contentMagic = "GDX1" // prefixed once at the very start of an encrypted stream
)

// Cipher holds the derived filename and content subkeys for one account.
type Cipher struct {
	nameKey    [KeySize]byte
	contentKey [KeySize]byte
}

// GenerateKey returns a new random 32-byte master key, meant to be written
// once to the account's key file and never regenerated (regenerating it
// makes every file already encrypted with the old key unrecoverable).
func GenerateKey() ([KeySize]byte, error) {
	var k [KeySize]byte
	_, err := rand.Read(k[:])
	return k, err
}

// New derives a Cipher from a master key (see GenerateKey).
func New(masterKey [KeySize]byte) (*Cipher, error) {
	c := &Cipher{}
	if err := derive(masterKey, "gdunion-filename-v1", c.nameKey[:]); err != nil {
		return nil, err
	}
	if err := derive(masterKey, "gdunion-content-v1", c.contentKey[:]); err != nil {
		return nil, err
	}
	return c, nil
}

func derive(master [KeySize]byte, info string, out []byte) error {
	_, err := io.ReadFull(hkdf.New(sha256.New, master[:], nil, []byte(info)), out)
	return err
}

func randomNonce() [nonceSize]byte {
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// crypto/rand failing means the OS RNG is broken; nothing downstream
		// of that can be trusted to behave safely, so fail loudly rather
		// than silently produce a predictable/zero nonce.
		panic("gcrypt: reading random bytes: " + err.Error())
	}
	return nonce
}

// EncryptName obfuscates a single path segment (not a full path - gdunion
// encrypts one directory/file name at a time). Each call produces a
// different ciphertext for the same input (random nonce); that's fine
// since gdunion is the only reader and never needs the mapping to be
// deterministic.
func (c *Cipher) EncryptName(name string) string {
	nonce := randomNonce()
	sealed := secretbox.Seal(nil, []byte(name), &nonce, &c.nameKey)
	out := append([]byte(nameMagic), nonce[:]...)
	out = append(out, sealed...)
	return base64.RawURLEncoding.EncodeToString(out)
}

// DecryptName reverses EncryptName. ok is false whenever raw doesn't look
// like something EncryptName produced - see the package doc for why a
// trial decrypt here is safe rather than just probabilistic.
func (c *Cipher) DecryptName(raw string) (name string, ok bool) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) < len(nameMagic)+nonceSize {
		return "", false
	}
	if string(data[:len(nameMagic)]) != nameMagic {
		return "", false
	}
	data = data[len(nameMagic):]
	var nonce [nonceSize]byte
	copy(nonce[:], data[:nonceSize])
	opened, ok := secretbox.Open(nil, data[nonceSize:], &nonce, &c.nameKey)
	if !ok {
		return "", false
	}
	return string(opened), true
}

// EncryptReader wraps a plaintext reader, yielding a magic-prefixed,
// chunk-framed ciphertext stream on Read. Intended to be handed straight to
// an upload call as the request body, so encryption happens while
// streaming rather than needing the whole file in memory.
func (c *Cipher) EncryptReader(r io.Reader) io.Reader {
	er := &encryptReader{c: c, src: r}
	er.buf.WriteString(contentMagic)
	return er
}

type encryptReader struct {
	c   *Cipher
	src io.Reader
	buf bytes.Buffer
	eof bool

	// chunk and sealBuf are reused across chunks (rather than allocated
	// fresh each time) so encrypting a large file doesn't cost one heap
	// allocation of each per 64KiB chunk. Both are sized to exactly fit the
	// largest a single chunk can ever be, so secretbox.Seal never needs to
	// grow sealBuf's backing array.
	chunk   [chunkPlain]byte
	sealBuf [chunkPlain + secretbox.Overhead]byte
}

func (er *encryptReader) Read(p []byte) (int, error) {
	for er.buf.Len() == 0 && !er.eof {
		n, err := io.ReadFull(er.src, er.chunk[:])
		if n > 0 {
			er.seal(er.chunk[:n])
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			er.eof = true
		case err != nil:
			return 0, err
		}
	}
	if er.buf.Len() == 0 {
		return 0, io.EOF
	}
	return er.buf.Read(p)
}

func (er *encryptReader) seal(plain []byte) {
	nonce := randomNonce()
	sealed := secretbox.Seal(er.sealBuf[:0], plain, &nonce, &er.c.contentKey)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(nonceSize+len(sealed)))
	er.buf.Write(lenBuf[:])
	er.buf.Write(nonce[:])
	er.buf.Write(sealed)
}

// OpenReader wraps r, which may or may not be something EncryptReader
// produced (see the package doc): if it starts with the content magic
// marker, the rest is decrypted; otherwise every byte of r (including the
// ones already peeked at to make that call) is passed through unchanged.
// Errors only for a stream that starts with the magic but is then corrupt.
func (c *Cipher) OpenReader(r io.Reader) io.Reader {
	peek := make([]byte, len(contentMagic))
	n, err := io.ReadFull(r, peek)
	if err != nil || string(peek[:n]) != contentMagic {
		// Not (recognizably) ours: replay whatever we already consumed,
		// then the rest of r, completely unchanged.
		return io.MultiReader(bytes.NewReader(peek[:n]), r)
	}
	return &decryptReader{c: c, src: r}
}

// maxFrame is the largest a single chunk's (nonce + sealed ciphertext) can
// ever legitimately be, i.e. exactly what EncryptReader produces for a full
// chunkPlain-sized chunk. decryptReader rejects anything claiming to be
// bigger outright as corrupt, rather than trusting an attacker- or
// corruption-controlled length prefix enough to attempt an allocation of
// that size.
const maxFrame = nonceSize + chunkPlain + secretbox.Overhead

type decryptReader struct {
	c   *Cipher
	src io.Reader
	buf bytes.Buffer
	eof bool

	// frameBuf and openBuf are reused across chunks (rather than allocated
	// fresh each time) so decrypting a large file doesn't cost two heap
	// allocations per chunk. Both are sized to exactly fit the largest a
	// single chunk can ever be.
	frameBuf [maxFrame]byte
	openBuf  [chunkPlain]byte
}

func (dr *decryptReader) Read(p []byte) (int, error) {
	for dr.buf.Len() == 0 && !dr.eof {
		var lenBuf [4]byte
		if _, err := io.ReadFull(dr.src, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				dr.eof = true
				break
			}
			return 0, fmt.Errorf("gcrypt: reading chunk length: %w", err)
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen < nonceSize+secretbox.Overhead || frameLen > maxFrame {
			return 0, errors.New("gcrypt: corrupt stream (invalid chunk length)")
		}
		frame := dr.frameBuf[:frameLen]
		if _, err := io.ReadFull(dr.src, frame); err != nil {
			return 0, fmt.Errorf("gcrypt: reading chunk: %w", err)
		}
		var nonce [nonceSize]byte
		copy(nonce[:], frame[:nonceSize])
		opened, ok := secretbox.Open(dr.openBuf[:0], frame[nonceSize:], &nonce, &dr.c.contentKey)
		if !ok {
			return 0, errors.New("gcrypt: decryption failed (wrong key, or data is corrupted)")
		}
		dr.buf.Write(opened)
	}
	if dr.buf.Len() == 0 {
		return 0, io.EOF
	}
	return dr.buf.Read(p)
}

// PlaintextSize computes the plaintext size corresponding to an encrypted
// stream of cipherSize bytes, purely from the fixed chunk-size arithmetic
// (no key or decryption needed) - used to report the size a user expects
// to see (e.g. `ls -la`) instead of the larger on-disk encrypted size.
// Only meaningful for a size that actually came from an EncryptReader
// stream; callers are responsible for knowing that (gdunion assumes
// everything under an account with encryption enabled is encrypted).
func PlaintextSize(cipherSize int64) (int64, error) {
	if cipherSize == 0 {
		return 0, nil
	}
	body := cipherSize - int64(len(contentMagic))
	if body < 0 {
		return 0, fmt.Errorf("gcrypt: implausible encrypted size %d", cipherSize)
	}
	if body == 0 {
		return 0, nil
	}
	fullChunks := body / chunkCipher
	rem := body % chunkCipher
	if rem == 0 {
		return fullChunks * chunkPlain, nil
	}
	if rem <= chunkOverhead {
		return 0, fmt.Errorf("gcrypt: implausible encrypted size %d", cipherSize)
	}
	return fullChunks*chunkPlain + (rem - chunkOverhead), nil
}
