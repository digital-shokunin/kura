package keyring

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/digital-shokunin/kura/internal/crypt"
)

// ErrNotCached means the secret is not stored on this machine.
var ErrNotCached = errors.New("not stored on this machine")

// Secrets is a local secret store keyed by (service, account).
type Secrets interface {
	Get(service, account string) ([]byte, error)
	Set(service, account string, data []byte) error
	Delete(service, account string) error
	Name() string
}

// KeystoreDirEnv, when set, forces file-based secrets in that directory.
// Tests use it so they never touch the real Keychain.
const KeystoreDirEnv = "KURA_KEYSTORE_DIR"

// DefaultSecrets returns the login Keychain on macOS, or files under
// ~/.config/kura elsewhere.
func DefaultSecrets() (Secrets, error) {
	if dir := os.Getenv(KeystoreDirEnv); dir != "" {
		return &FileSecrets{Dir: dir}, nil
	}
	return platformSecrets()
}

const (
	repoKeyService = "kura"
	s3CredsService = "kura-s3"
)

var accountPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Cache stores unwrapped repo keys, keyed by repo id.
type Cache struct{ Secrets }

// DefaultCache returns the repo key cache on the platform secret store.
func DefaultCache() (Cache, error) {
	s, err := DefaultSecrets()
	return Cache{s}, err
}

func (c Cache) Get(repoID string) (crypt.Key, error) {
	b, err := c.Secrets.Get(repoKeyService, repoID)
	if err != nil {
		return crypt.Key{}, err
	}
	return crypt.KeyFromBytes(b)
}

func (c Cache) Set(repoID string, k crypt.Key) error {
	return c.Secrets.Set(repoKeyService, repoID, k[:])
}

func (c Cache) Delete(repoID string) error { return c.Secrets.Delete(repoKeyService, repoID) }

type s3Creds struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// LoadS3Creds returns a named static S3 credential.
func LoadS3Creds(name string) (string, string, error) {
	s, err := DefaultSecrets()
	if err != nil {
		return "", "", err
	}
	b, err := s.Get(s3CredsService, name)
	if err != nil {
		return "", "", err
	}
	var c s3Creds
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	return c.AccessKeyID, c.SecretAccessKey, nil
}

// SaveS3Creds stores a named static S3 credential.
func SaveS3Creds(name, accessKeyID, secretAccessKey string) error {
	s, err := DefaultSecrets()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(s3Creds{accessKeyID, secretAccessKey})
	return s.Set(s3CredsService, name, b)
}

// FileSecrets keeps secrets in 0600 files in a 0700 directory tree.
type FileSecrets struct{ Dir string }

func (f *FileSecrets) Name() string { return "file store " + f.Dir }

func (f *FileSecrets) path(service, account string) (string, error) {
	if !accountPattern.MatchString(service) || !accountPattern.MatchString(account) {
		return "", fmt.Errorf("keyring: invalid secret name %q/%q", service, account)
	}
	return filepath.Join(f.Dir, service, account), nil
}

func (f *FileSecrets) Get(service, account string) ([]byte, error) {
	p, err := f.path(service, account)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotCached
	}
	return b, err
}

func (f *FileSecrets) Set(service, account string, data []byte) error {
	p, err := f.path(service, account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (f *FileSecrets) Delete(service, account string) error {
	p, err := f.path(service, account)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ConfigDir is kura's configuration directory: $XDG_CONFIG_HOME/kura, or
// ~/.config/kura on every platform (including macOS).
func ConfigDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "kura"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kura"), nil
}

func defaultFileSecrets() (Secrets, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	return &FileSecrets{Dir: filepath.Join(dir, "secrets")}, nil
}
