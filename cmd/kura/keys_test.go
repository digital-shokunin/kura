package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/store"
)

// failingPuts is a store whose Put always fails.
type failingPuts struct{ store.Store }

func (failingPuts) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("injected put failure")
}

// Dropping every gpg recipient writes keys/age.age and deletes keys/gpg.asc.
// If the write fails, the gpg key file must still be there.
func TestStoreKeysWritesBeforeDeleting(t *testing.T) {
	ctx := context.Background()
	for range 10 { // map order is random; repeat so an unordered loop fails
		fs, err := store.NewFile(filepath.Join(t.TempDir(), "remote"))
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{keyring.AgeKeyPath, keyring.GPGKeyPath} {
			if err := fs.Put(ctx, key, strings.NewReader("old"), 3); err != nil {
				t.Fatal(err)
			}
		}
		objs := map[string][]byte{keyring.AgeKeyPath: []byte("new"), keyring.GPGKeyPath: nil}
		if err := storeKeys(ctx, failingPuts{fs}, objs); err == nil {
			t.Fatal("storeKeys ignored a failed write")
		}
		if _, err := fs.Head(ctx, keyring.GPGKeyPath); err != nil {
			t.Fatalf("gpg key file deleted although the age key file was not written: %v", err)
		}
	}
}
