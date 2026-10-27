// Package repo parses kura remote URLs and opens repositories with a cached
// key, enforcing the pinned key fingerprint.
package repo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/remote"
	"github.com/digital-shokunin/kura/internal/store"
	"github.com/digital-shokunin/kura/internal/store/s3"
)

func init() { s3.CredentialLookup = keyring.LoadS3Creds }

// Target is a parsed kura URL: the backing store plus the pinned fingerprint.
type Target struct {
	// StoreURL is the backend URL with kura-specific query parameters removed.
	StoreURL string
	// Fingerprint is the expected key fingerprint (?fp=), or "".
	Fingerprint string
}

// ParseURL accepts "kura::<store-url>[?fp=...]" or a bare store URL.
func ParseURL(raw string) (Target, error) {
	raw = strings.TrimPrefix(raw, "kura::")
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("bad url %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return Target{}, fmt.Errorf("url %q needs a scheme (file://, s3://)", raw)
	}
	q := u.Query()
	t := Target{Fingerprint: q.Get("fp")}
	q.Del("fp")
	u.RawQuery = q.Encode()
	t.StoreURL = u.String()
	return t, nil
}

// URL returns the full kura URL for git, including the fingerprint.
func (t Target) URL() string {
	u, _ := url.Parse(t.StoreURL)
	if t.Fingerprint != "" {
		q := u.Query()
		q.Set("fp", t.Fingerprint)
		u.RawQuery = q.Encode()
	}
	return "kura::" + u.String()
}

// UnlockHint is the command that unlocks a repo on this machine.
func (t Target) UnlockHint() string {
	return fmt.Sprintf("kura unlock '%s'", t.URL())
}

// OpenStore opens the target's backend and reads its config.
func OpenStore(ctx context.Context, t Target) (store.Store, remote.Config, error) {
	s, err := store.Open(ctx, t.StoreURL)
	if err != nil {
		return nil, remote.Config{}, err
	}
	cfg, err := remote.LoadConfig(ctx, s)
	return s, cfg, err
}

// CheckFingerprint verifies k against every non-empty expected fingerprint.
func CheckFingerprint(k crypt.Key, repoID string, expected ...string) error {
	got := k.Fingerprint(repoID)
	for _, want := range expected {
		if want != "" && want != got {
			return fmt.Errorf("key fingerprint mismatch: remote key is %s but %s is pinned; "+
				"the remote's key files may have been substituted", got, want)
		}
	}
	return nil
}

// Open returns the remote with its key from the local cache. It never
// prompts: if the key is not cached it explains how to unlock.
func Open(ctx context.Context, t Target, cache keyring.Cache, pinned ...string) (*remote.Remote, error) {
	s, cfg, err := OpenStore(ctx, t)
	if err != nil {
		return nil, err
	}
	k, err := cache.Get(cfg.RepoID)
	if errors.Is(err, keyring.ErrNotCached) {
		return nil, fmt.Errorf("repo key not unlocked on this machine; run in a terminal:\n  %s", t.UnlockHint())
	}
	if err != nil {
		return nil, fmt.Errorf("reading key from %s: %w", cache.Name(), err)
	}
	if err := CheckFingerprint(k, cfg.RepoID, append(pinned, t.Fingerprint)...); err != nil {
		return nil, err
	}
	return &remote.Remote{Store: s, RepoID: cfg.RepoID, Key: k}, nil
}
