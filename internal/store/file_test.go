package store_test

import (
	"context"
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
