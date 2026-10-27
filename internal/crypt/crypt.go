// Package crypt holds the per-repo master key and the age-format encryption
// used for every object kura stores.
//
// Files are standard age files whose single recipient stanza is a symmetric
// wrap of the age file key under a subkey of the master key K. Only holders of
// K can create or read them, so a storage provider cannot forge objects. The
// repo id, format version and the object's slot (its storage key) are bound
// into the stanza as associated data, so a ciphertext cannot be replayed into
// another repository, version or slot.
package crypt

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"
)

// FormatVersion is the on-remote format version. It is bound into every
// stanza so a provider cannot downgrade it.
const FormatVersion = 1

const stanzaType = "kura-v1"

// KeySize is the size of the master key in bytes.
const KeySize = 32

// Key is a repo master key.
type Key [KeySize]byte

// NewKey returns a fresh random master key.
func NewKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	return k, nil
}

// KeyFromBytes copies b into a Key.
func KeyFromBytes(b []byte) (Key, error) {
	var k Key
	if len(b) != KeySize {
		return k, fmt.Errorf("crypt: master key must be %d bytes, got %d", KeySize, len(b))
	}
	copy(k[:], b)
	return k, nil
}

func (k Key) subkey(label, repoID string, n int) []byte {
	info := fmt.Sprintf("kura v%d %s %s", FormatVersion, label, repoID)
	out, err := hkdf.Key(sha256.New, k[:], nil, info, n)
	if err != nil {
		panic(err) // only fails for absurd lengths
	}
	return out
}

// Fingerprint identifies K for a repo. It is pinned in git config, the clone
// URL and the recovery sheet so a substituted key is rejected on unlock.
func (k Key) Fingerprint(repoID string) string {
	return hex.EncodeToString(k.subkey("fingerprint", repoID, 16))
}

func associatedData(repoID, slot string) []byte {
	return fmt.Appendf(nil, "%s|%s|v%d|%s", stanzaType, repoID, FormatVersion, slot)
}

type stanzaKey struct {
	key []byte
	ad  []byte
}

func (k Key) stanzaKey(repoID, slot string) stanzaKey {
	return stanzaKey{key: k.subkey("wrap", repoID, chacha20poly1305.KeySize), ad: associatedData(repoID, slot)}
}

// Wrap implements age.Recipient.
func (s stanzaKey) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	aead, err := chacha20poly1305.NewX(s.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return []*age.Stanza{{
		Type: stanzaType,
		Args: []string{base64.RawStdEncoding.EncodeToString(nonce)},
		Body: aead.Seal(nil, nonce, fileKey, s.ad),
	}}, nil
}

// Unwrap implements age.Identity.
func (s stanzaKey) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(s.key)
	if err != nil {
		return nil, err
	}
	for _, st := range stanzas {
		if st.Type != stanzaType {
			continue
		}
		if len(st.Args) != 1 {
			return nil, errors.New("crypt: malformed kura stanza")
		}
		nonce, err := base64.RawStdEncoding.Strict().DecodeString(st.Args[0])
		if err != nil || len(nonce) != aead.NonceSize() {
			return nil, errors.New("crypt: malformed kura stanza nonce")
		}
		fileKey, err := aead.Open(nil, nonce, st.Body, s.ad)
		if err != nil {
			return nil, fmt.Errorf("%w: kura stanza does not open with this key", age.ErrIncorrectIdentity)
		}
		return fileKey, nil
	}
	return nil, age.ErrIncorrectIdentity
}

// Encrypt returns a writer that encrypts to dst for the object stored at
// slot. Close must be called.
func (k Key) Encrypt(dst io.Writer, repoID, slot string) (io.WriteCloser, error) {
	return age.Encrypt(dst, k.stanzaKey(repoID, slot))
}

// Decrypt returns a reader of the plaintext in src, which must have been
// encrypted for slot. Integrity is only fully established once the reader has
// returned io.EOF.
func (k Key) Decrypt(src io.Reader, repoID, slot string) (io.Reader, error) {
	return age.Decrypt(src, k.stanzaKey(repoID, slot))
}

// Seal encrypts a small in-memory value for slot.
func (k Key) Seal(plain []byte, repoID, slot string) ([]byte, error) {
	var buf bytes.Buffer
	w, err := k.Encrypt(&buf, repoID, slot)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Open decrypts a value produced by Seal for the same slot.
func (k Key) Open(ct []byte, repoID, slot string) ([]byte, error) {
	r, err := k.Decrypt(bytes.NewReader(ct), repoID, slot)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// PadmeLen returns the Padmé padded length for n bytes, which leaks at most
// O(log log n) bits of the size with at most ~12% overhead.
func PadmeLen(n int64) int64 {
	if n < 2 {
		return n
	}
	e := 63 - bits.LeadingZeros64(uint64(n))
	s := 64 - bits.LeadingZeros64(uint64(e))
	mask := int64(1)<<(e-s) - 1
	return (n + mask) &^ mask
}

// WritePadded writes an 8-byte length frame, n bytes from src, then zero
// padding up to the Padmé length of the frame.
func WritePadded(dst io.Writer, src io.Reader, n int64) error {
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(n))
	if _, err := dst.Write(hdr[:]); err != nil {
		return err
	}
	copied, err := io.CopyN(dst, src, n)
	if err != nil {
		return fmt.Errorf("crypt: padded copy: wrote %d of %d bytes: %w", copied, n, err)
	}
	pad := PadmeLen(n+8) - (n + 8)
	_, err = io.CopyN(dst, zeroReader{}, pad)
	return err
}

// ReadPadded copies the framed payload from src to dst and then drains src so
// the final age chunk is authenticated.
func ReadPadded(dst io.Writer, src io.Reader) (int64, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(src, hdr[:]); err != nil {
		return 0, fmt.Errorf("crypt: reading frame header: %w", err)
	}
	n := int64(binary.BigEndian.Uint64(hdr[:]))
	if n < 0 {
		return 0, errors.New("crypt: bad frame length")
	}
	if _, err := io.CopyN(dst, src, n); err != nil {
		return 0, fmt.Errorf("crypt: reading framed payload: %w", err)
	}
	if _, err := io.Copy(io.Discard, src); err != nil {
		return 0, fmt.Errorf("crypt: authenticating padding: %w", err)
	}
	return n, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
