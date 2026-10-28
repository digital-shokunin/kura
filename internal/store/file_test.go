package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digital-shokunin/kura/internal/store"
	"github.com/digital-shokunin/kura/internal/store/storetest"
)

func TestFileConformance(t *testing.T) {
	s, err := store.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range storetest.Checks() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Run(context.Background(), s, "t/"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileRejectsBadKeys(t *testing.T) {
	s, err := store.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"", "/abs", "../up", "a/../b", "a//b", "a/.tmp-x"} {
		if _, err := s.Head(context.Background(), k); err == nil || err == store.ErrNotFound {
			t.Errorf("key %q: expected invalid key error, got %v", k, err)
		}
	}
}

// Whoever else can write the directory (a shared NAS) must not be able to aim
// reads, writes or gc deletes outside it with a planted symlink.
func TestFileConfinedToRoot(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root, outside := filepath.Join(tmp, "remote"), filepath.Join(tmp, "outside")
	s, err := store.NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(outside, "docs"), 0o700)
	victim := filepath.Join(outside, "docs", "0000")
	os.WriteFile(victim, []byte("precious"), 0o600)
	os.WriteFile(filepath.Join(outside, "age.age"), []byte("precious"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "packs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "keys")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}

	keys, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("List followed symlinks out of the root: %v", keys)
	}
	if _, err := s.Get(ctx, "packs/docs/0000"); err == nil {
		t.Error("Get read through a symlinked directory")
	}
	if _, err := s.Get(ctx, "config.json"); err == nil {
		t.Error("Get followed a symlink out of the root")
	}
	if _, err := s.Head(ctx, "packs/docs/0000"); err == nil {
		t.Error("Head stat'ed through a symlinked directory")
	}
	if err := s.Put(ctx, "keys/age.age", strings.NewReader("evil"), 4); err == nil {
		t.Error("Put wrote through a symlinked directory")
	}
	if err := s.PutIfAbsent(ctx, "packs/new/0000", strings.NewReader("evil"), 4); err == nil {
		t.Error("PutIfAbsent wrote through a symlinked directory")
	}
	if err := s.Delete(ctx, "packs/docs/0000"); err == nil {
		t.Error("Delete removed a file through a symlinked directory")
	}
	for _, p := range []string{victim, filepath.Join(outside, "age.age")} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "precious" {
			t.Errorf("%s outside the root was changed: %q, %v", p, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("a directory was created outside the root")
	}
}
