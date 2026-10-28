package keyring

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/digital-shokunin/kura/internal/crypt"
)

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWrapAgeAnyRecipientUnwraps(t *testing.T) {
	k, _ := crypt.NewKey()
	a, b, stranger := newIdentity(t), newIdentity(t), newIdentity(t)
	ct, err := WrapAge(k, []string{a.Recipient().String(), b.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []*age.X25519Identity{a, b} {
		got, err := UnwrapAge(ct, []age.Identity{id})
		if err != nil {
			t.Fatal(err)
		}
		if got != k {
			t.Fatal("unwrapped a different key")
		}
	}
	if _, err := UnwrapAge(ct, []age.Identity{stranger}); err == nil {
		t.Fatal("a non-recipient unwrapped the key")
	}
}

func TestUnwrapAgeRejectsWrongLength(t *testing.T) {
	id := newIdentity(t)
	for _, n := range []int{crypt.KeySize - 1, crypt.KeySize + 1} {
		var buf strings.Builder
		w, err := age.Encrypt(&buf, id.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		w.Write(make([]byte, n))
		w.Close()
		if _, err := UnwrapAge([]byte(buf.String()), []age.Identity{id}); err == nil {
			t.Fatalf("accepted a %d-byte key", n)
		}
	}
}

func TestWrapAgeRejectsBadRecipient(t *testing.T) {
	k, _ := crypt.NewKey()
	for _, r := range []string{"", "not-a-key", "ssh-ed25519 AAAA"} {
		if _, err := WrapAge(k, []string{r}); err == nil {
			t.Errorf("WrapAge accepted recipient %q", r)
		}
	}
}

func TestLoadIdentities(t *testing.T) {
	dir := t.TempDir()
	a, b := newIdentity(t), newIdentity(t)
	good := filepath.Join(dir, "ids")
	os.WriteFile(good, []byte("# created: today\n"+a.String()+"\n\n  "+b.String()+"  \n"), 0o600)
	ids, err := LoadIdentities(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d identities, want 2", len(ids))
	}

	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("# nothing here\n"), 0o600)
	if _, err := LoadIdentities(empty); err == nil {
		t.Error("an identity file without identities was accepted")
	}
	junk := filepath.Join(dir, "junk")
	os.WriteFile(junk, []byte("AGE-SECRET-KEY-1NOTVALID\n"), 0o600)
	if _, err := LoadIdentities(junk); err == nil {
		t.Error("a malformed identity was accepted")
	}
	if _, err := LoadIdentities(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestFileSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	f := &FileSecrets{Dir: dir}
	if _, err := f.Get("kura", "abc"); !errors.Is(err, ErrNotCached) {
		t.Fatalf("Get of a missing secret: %v, want ErrNotCached", err)
	}
	if err := f.Set("kura", "abc", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("kura", "abc", []byte("two")); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get("kura", "abc")
	if err != nil || string(got) != "two" {
		t.Fatalf("Get = %q, %v; want the latest value", got, err)
	}
	for path, want := range map[string]os.FileMode{
		dir:                               0o700,
		filepath.Join(dir, "kura"):        0o700,
		filepath.Join(dir, "kura", "abc"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != want {
			t.Errorf("%s has mode %o, want %o", path, perm, want)
		}
	}
	if err := f.Delete("kura", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("kura", "abc"); err != nil {
		t.Fatalf("deleting a missing secret: %v", err)
	}
	if _, err := f.Get("kura", "abc"); !errors.Is(err, ErrNotCached) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

func TestFileSecretsRejectsUnsafeNames(t *testing.T) {
	f := &FileSecrets{Dir: t.TempDir()}
	for _, name := range []string{"", "..", "../escape", "a/b", ".hidden", "-flag", strings.Repeat("a", 129)} {
		if err := f.Set("kura", name, []byte("x")); err == nil {
			t.Errorf("Set accepted account %q", name)
		}
		if err := f.Set(name, "abc", []byte("x")); err == nil {
			t.Errorf("Set accepted service %q", name)
		}
	}
}

func TestCacheRoundTripAndEnvOverrides(t *testing.T) {
	keystore, keycache := t.TempDir(), t.TempDir()
	t.Setenv(KeystoreDirEnv, keystore)
	t.Setenv(KeyCacheDirEnv, keycache)

	c, err := DefaultCache()
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypt.NewKey()
	if err := c.Set("0123456789abcdef0123456789abcdef", k); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get("0123456789abcdef0123456789abcdef"); err != nil || got != k {
		t.Fatalf("Get = %v, %v", got, err)
	}
	// The repo key goes to the key cache; S3 credentials to the keystore.
	if _, err := os.Stat(filepath.Join(keycache, repoKeyService, "0123456789abcdef0123456789abcdef")); err != nil {
		t.Errorf("repo key not in KURA_KEYCACHE_DIR: %v", err)
	}
	if err := SaveS3Creds("bucket", "AKIDEXAMPLE", "secret"); err != nil {
		t.Fatal(err)
	}
	id, secret, err := LoadS3Creds("bucket")
	if err != nil || id != "AKIDEXAMPLE" || secret != "secret" {
		t.Fatalf("LoadS3Creds = %q, %q, %v", id, secret, err)
	}
	if _, err := os.Stat(filepath.Join(keystore, s3CredsService, "bucket")); err != nil {
		t.Errorf("S3 credentials not in KURA_KEYSTORE_DIR: %v", err)
	}
	if entries, _ := os.ReadDir(keycache); len(entries) != 1 {
		t.Errorf("KURA_KEYCACHE_DIR holds %d entries, want only the repo key service", len(entries))
	}
}

func TestConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/config")
	if d, _ := ConfigDir(); d != "/x/config/kura" {
		t.Errorf("ConfigDir with XDG_CONFIG_HOME = %q", d)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/x/home")
	if d, _ := ConfigDir(); d != "/x/home/.config/kura" {
		t.Errorf("ConfigDir without XDG_CONFIG_HOME = %q", d)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: crypt.KeySize}
	if _, err := b.Write(make([]byte, crypt.KeySize-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte{2}); err == nil {
		t.Fatal("write past the cap succeeded")
	}
	if n := len(b.Bytes()); n != crypt.KeySize {
		t.Fatalf("buffer holds %d bytes", n)
	}
	// io.Copy must go through Write, not a promoted ReadFrom.
	if _, ok := any(b).(io.ReaderFrom); ok {
		t.Fatal("cappedBuffer implements io.ReaderFrom, which bypasses the cap")
	}
}

// A decompression bomb from the store must stop gpg, not exhaust memory.
func TestUnwrapGPGStopsOversizedOutput(t *testing.T) {
	if _, err := gpgPath(); err != nil {
		t.Skip("gpg not installed")
	}
	// gpg-agent's socket path must be short; macOS temp dirs are too long.
	home, err := os.MkdirTemp("/tmp", "kura-gpg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("gpgconf", "--kill", "gpg-agent").Run()
		os.RemoveAll(home)
	})
	t.Setenv("GNUPGHOME", home)
	// A symmetric message needs no keys; with --passphrase it decrypts
	// without prompting, which is all this test needs from gpg.
	enc := exec.Command("gpg", "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", "x",
		"--compress-level", "9", "--symmetric", "--armor")
	enc.Stdin = strings.NewReader(strings.Repeat("A", 64<<20))
	ct, err := enc.Output()
	if err != nil {
		t.Skipf("gpg cannot encrypt here: %v", err)
	}
	if len(ct) > 1<<20 {
		t.Fatalf("test message is %d bytes; expected heavy compression", len(ct))
	}
	os.WriteFile(filepath.Join(home, "gpg.conf"), []byte("batch\npinentry-mode loopback\npassphrase x\n"), 0o600)
	if _, err := UnwrapGPG(ct); err == nil || !strings.Contains(err.Error(), "gpg decrypt") {
		t.Fatalf("UnwrapGPG of a 64 MiB message: %v, want an error", err)
	}
}
