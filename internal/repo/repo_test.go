package repo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/remote"
	"github.com/digital-shokunin/kura/internal/store"
)

func TestParseURL(t *testing.T) {
	for _, tc := range []struct {
		in, store, fp string
	}{
		{"kura::s3://b/p?region=us-east-2&fp=abc", "s3://b/p?region=us-east-2", "abc"},
		{"s3://b/p?fp=abc&creds=x", "s3://b/p?creds=x", "abc"},
		{"kura::file:///tmp/r", "file:///tmp/r", ""},
	} {
		got, err := ParseURL(tc.in)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", tc.in, err)
		}
		if got.StoreURL != tc.store || got.Fingerprint != tc.fp {
			t.Errorf("ParseURL(%q) = %+v, want store %q fp %q", tc.in, got, tc.store, tc.fp)
		}
	}
	for _, bad := range []string{"", "kura::", "/no/scheme", "kura::%zz"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) succeeded", bad)
		}
	}
}

func TestURLRoundTrip(t *testing.T) {
	in := "kura::s3://b/p?creds=x&fp=abc&region=us-east-2"
	tg, err := ParseURL(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := tg.URL(); got != in {
		t.Errorf("URL() = %q, want %q", got, in)
	}
	if hint := tg.UnlockHint(); hint != "kura unlock '"+in+"'" {
		t.Errorf("UnlockHint() = %q", hint)
	}
}

func TestCheckFingerprint(t *testing.T) {
	k, _ := crypt.NewKey()
	id := remote.NewRepoID()
	fp := k.Fingerprint(id)
	if err := CheckFingerprint(k, id, fp, "", fp); err != nil {
		t.Fatalf("matching fingerprint rejected: %v", err)
	}
	if err := CheckFingerprint(k, id); err != nil {
		t.Fatalf("no pinned fingerprint rejected: %v", err)
	}
	if err := CheckFingerprint(k, id, fp, "00"+fp[2:]); err == nil {
		t.Fatal("mismatched fingerprint accepted")
	}
	// The fingerprint binds the repo id: the same key in another repo differs.
	if err := CheckFingerprint(k, remote.NewRepoID(), fp); err == nil {
		t.Fatal("fingerprint accepted for a different repo id")
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "remote")
	s, err := store.NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypt.NewKey()
	id := remote.NewRepoID()
	if _, err := remote.Init(ctx, s, k, id, remote.Recipients{Age: []string{"age1test"}}); err != nil {
		t.Fatal(err)
	}
	fp := k.Fingerprint(id)
	cache := keyring.Cache{Secrets: &keyring.FileSecrets{Dir: t.TempDir()}}
	tg, err := ParseURL("kura::file://" + dir + "?fp=" + fp)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Open(ctx, tg, cache)
	if err == nil || !strings.Contains(err.Error(), "kura unlock '"+tg.URL()+"'") {
		t.Fatalf("Open without a cached key: %v; want the unlock command", err)
	}
	if err := cache.Set(id, k); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, tg, cache)
	if err != nil {
		t.Fatal(err)
	}
	if r.RepoID != id || r.Key != k {
		t.Fatal("Open returned the wrong repo or key")
	}

	// A substituted key in the cache, or a different pinned fingerprint
	// (remote.<name>.kurafp), is refused.
	if _, err := Open(ctx, tg, cache, "00"+fp[2:]); err == nil {
		t.Fatal("Open ignored a conflicting pinned fingerprint")
	}
	other, _ := crypt.NewKey()
	cache.Set(id, other)
	if _, err := Open(ctx, tg, cache); err == nil {
		t.Fatal("Open accepted a cached key that does not match the URL's fingerprint")
	}
}
