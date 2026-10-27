package crypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"filippo.io/age"
)

func mustKey(t *testing.T) Key {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := mustKey(t)
	for _, n := range []int{0, 1, 64 << 10, 64<<10 + 1, 1 << 20} {
		plain := make([]byte, n)
		rand.Read(plain)
		ct, err := k.Seal(plain, "repo", "slot")
		if err != nil {
			t.Fatal(err)
		}
		got, err := k.Open(ct, "repo", "slot")
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: round trip mismatch", n)
		}
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	k := mustKey(t)
	plain := make([]byte, 200<<10)
	rand.Read(plain)
	ct, err := k.Seal(plain, "repo", "slot")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int{10, len(ct) / 2, len(ct) - 1} {
		bad := bytes.Clone(ct)
		bad[off] ^= 0x01
		if _, err := k.Open(bad, "repo", "slot"); err == nil {
			t.Fatalf("flipped byte at %d decrypted without error", off)
		}
	}
	if _, err := k.Open(ct[:len(ct)-100], "repo", "slot"); err == nil {
		t.Fatal("truncated ciphertext decrypted without error")
	}
}

func TestWrongKeyRepoIDOrSlotFails(t *testing.T) {
	k := mustKey(t)
	const slot = "manifests/000000000001"
	ct, err := k.Seal([]byte("hello"), "repo-a", slot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Open(ct, "repo-b", slot); !errors.Is(err, age.ErrIncorrectIdentity) {
		t.Fatalf("wrong repo id: got %v, want ErrIncorrectIdentity", err)
	}
	if _, err := k.Open(ct, "repo-a", "manifests/000000000002"); !errors.Is(err, age.ErrIncorrectIdentity) {
		t.Fatalf("wrong slot: got %v, want ErrIncorrectIdentity", err)
	}
	if _, err := mustKey(t).Open(ct, "repo-a", slot); !errors.Is(err, age.ErrIncorrectIdentity) {
		t.Fatalf("wrong key: got %v, want ErrIncorrectIdentity", err)
	}
}

func TestFingerprintBindsKeyAndRepo(t *testing.T) {
	k := mustKey(t)
	if k.Fingerprint("a") == k.Fingerprint("b") {
		t.Fatal("fingerprint does not depend on repo id")
	}
	if k.Fingerprint("a") == mustKey(t).Fingerprint("a") {
		t.Fatal("fingerprint does not depend on key")
	}
	if k.Fingerprint("a") != k.Fingerprint("a") {
		t.Fatal("fingerprint not deterministic")
	}
}

func TestPadmeLen(t *testing.T) {
	prev := int64(0)
	for n := int64(0); n < 1<<16; n++ {
		p := PadmeLen(n)
		if p < n {
			t.Fatalf("PadmeLen(%d) = %d < n", n, p)
		}
		if n > 16 && float64(p-n)/float64(n) > 0.125 {
			t.Fatalf("PadmeLen(%d) = %d: overhead too large", n, p)
		}
		if p < prev {
			t.Fatalf("PadmeLen not monotonic at %d", n)
		}
		prev = p
	}
	if PadmeLen(1000001) == 1000001 {
		t.Fatal("expected padding for 1000001")
	}
}

func TestPaddedFrameRoundTrip(t *testing.T) {
	k := mustKey(t)
	for _, n := range []int64{0, 7, 12345, 1 << 20} {
		plain := make([]byte, n)
		rand.Read(plain)
		var ct bytes.Buffer
		w, err := k.Encrypt(&ct, "repo", "slot")
		if err != nil {
			t.Fatal(err)
		}
		if err := WritePadded(w, bytes.NewReader(plain), n); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := k.Decrypt(&ct, "repo", "slot")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		got, err := ReadPadded(&out, r)
		if err != nil {
			t.Fatal(err)
		}
		if got != n || !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("n=%d: padded round trip mismatch", n)
		}
	}
}

func TestWritePaddedShortSource(t *testing.T) {
	if err := WritePadded(io.Discard, bytes.NewReader([]byte("abc")), 10); err == nil {
		t.Fatal("expected error on short source")
	}
}
