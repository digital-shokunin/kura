// Package keyring wraps a repo's master key to the user's long-term keys
// (age recipients, including plugins like age-plugin-se, and gpg) and caches
// the unwrapped key locally (macOS Keychain, or a 0600 file elsewhere).
package keyring

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/plugin"

	"github.com/digital-shokunin/kura/internal/crypt"
)

// Remote object keys for wrapped copies of K.
const (
	AgeKeyPath = "keys/age.age"
	GPGKeyPath = "keys/gpg.asc"
)

func init() {
	// Git hosts and GUI apps (Obsidian) often run with a minimal PATH. Make
	// sure Homebrew's gpg and age plugins are found.
	path := os.Getenv("PATH")
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(":"+path+":", ":"+d+":") {
			path += ":" + d
		}
	}
	os.Setenv("PATH", path)
}

func terminalUI() *plugin.ClientUI {
	logf := func(format string, v ...any) { fmt.Fprintf(os.Stderr, "kura: "+format+"\n", v...) }
	return plugin.NewTerminalUI(logf, logf)
}

func parseRecipient(s string) (age.Recipient, error) {
	if rs, err := age.ParseRecipients(strings.NewReader(s)); err == nil {
		return rs[0], nil
	}
	if strings.HasPrefix(s, "age1") {
		return plugin.NewRecipient(s, terminalUI())
	}
	return nil, fmt.Errorf("unsupported age recipient %q", s)
}

// WrapAge encrypts K to every age recipient (X25519 or plugin) in one file;
// any single matching identity can unwrap it.
func WrapAge(k crypt.Key, recipients []string) ([]byte, error) {
	var rs []age.Recipient
	for _, s := range recipients {
		r, err := parseRecipient(s)
		if err != nil {
			return nil, err
		}
		rs = append(rs, r)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(k[:]); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// LoadIdentities parses an age identity file, which may mix native secret
// keys (AGE-SECRET-KEY-1...) and plugin identities (AGE-PLUGIN-SE-1...).
func LoadIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []age.Identity
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "AGE-PLUGIN-") {
			id, err := plugin.NewIdentity(line, terminalUI())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			ids = append(ids, id)
			continue
		}
		native, err := age.ParseIdentities(strings.NewReader(line))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ids = append(ids, native...)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no identities found", path)
	}
	return ids, nil
}

// UnwrapAge decrypts K from an age-wrapped key file.
func UnwrapAge(ct []byte, ids []age.Identity) (crypt.Key, error) {
	r, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		return crypt.Key{}, err
	}
	b, err := io.ReadAll(io.LimitReader(r, crypt.KeySize+1))
	if err != nil {
		return crypt.Key{}, err
	}
	return crypt.KeyFromBytes(b)
}

func gpgPath() (string, error) {
	p, err := exec.LookPath("gpg")
	if err != nil {
		return "", errors.New("gpg not found (brew install gnupg)")
	}
	return filepath.Abs(p)
}

// WrapGPG encrypts K to the given gpg key fingerprints with hidden recipient
// key ids, so the provider cannot see which key unlocks the repo.
func WrapGPG(k crypt.Key, fingerprints []string) ([]byte, error) {
	gpg, err := gpgPath()
	if err != nil {
		return nil, err
	}
	args := []string{"--batch", "--yes", "--armor", "--encrypt", "--throw-keyids", "--trust-model", "always"}
	for _, fp := range fingerprints {
		args = append(args, "--recipient", fp)
	}
	var out bytes.Buffer
	cmd := exec.Command(gpg, args...)
	cmd.Stdin = bytes.NewReader(k[:])
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gpg encrypt: %w", err)
	}
	return out.Bytes(), nil
}

// UnwrapGPG decrypts K with gpg, which may prompt for the smartcard PIN via
// pinentry and require a touch. Because recipients are hidden, gpg otherwise
// tries every secret encryption key in turn (asking for each smartcard);
// tryKeys names the key(s) to try instead.
func UnwrapGPG(ct []byte, tryKeys ...string) (crypt.Key, error) {
	gpg, err := gpgPath()
	if err != nil {
		return crypt.Key{}, err
	}
	args := []string{"--quiet"}
	for _, k := range tryKeys {
		args = append(args, "--try-secret-key", strings.TrimSuffix(k, "!"))
	}
	out := &cappedBuffer{max: crypt.KeySize}
	cmd := exec.Command(gpg, append(args, "--decrypt")...)
	cmd.Stdin = bytes.NewReader(ct)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return crypt.Key{}, fmt.Errorf("gpg decrypt: %w", err)
	}
	return crypt.KeyFromBytes(out.Bytes())
}

// cappedBuffer fails writes past max bytes. keys/gpg.asc comes from the
// untrusted store and anyone can encrypt to a public key, so a compressed
// message could otherwise expand without bound; the failed write closes
// gpg's stdout and stops it. The buffer is a field, not embedded, so that
// io.Copy cannot bypass Write through bytes.Buffer's ReadFrom.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, fmt.Errorf("gpg output exceeds %d bytes; %s is not a kura key", b.max, GPGKeyPath)
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
