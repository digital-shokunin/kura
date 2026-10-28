package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func init() {
	Register("file", func(_ context.Context, u *url.URL) (Store, error) {
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("store: file url must not have a host: %q", u.String())
		}
		return NewFile(u.Path)
	})
}

// File stores objects as files under a directory. PutIfAbsent uses link(2),
// which fails atomically if the target exists, so it is safe across processes
// and on most network filesystems that support hard links.
//
// Every operation goes through an os.Root, so a symlink planted in the
// directory by whoever else can write to it (a shared NAS, say) cannot point
// kura's reads, writes or gc deletes at files outside it.
type File struct {
	root *os.Root
}

// NewFile returns a File store rooted at dir, creating it if needed.
func NewFile(dir string) (*File, error) {
	if dir == "" {
		return nil, errors.New("store: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &File{root: root}, nil
}

func (f *File) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("store: invalid key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".tmp-") {
			return "", fmt.Errorf("store: invalid key %q", key)
		}
	}
	return filepath.FromSlash(key), nil
}

func (f *File) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := f.root.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fh, err
}

// writeTemp writes r to a temp file next to final and fsyncs it.
func (f *File) writeTemp(final string, r io.Reader, size int64) (string, error) {
	dir := filepath.Dir(final)
	if err := f.root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, ".tmp-"+rand.Text())
	tmp, err := f.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, r)
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("store: short write: %d of %d bytes", n, size)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		f.root.Remove(name)
		return "", err
	}
	return name, nil
}

func (f *File) Put(_ context.Context, key string, r io.Reader, size int64) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	tmp, err := f.writeTemp(p, r, size)
	if err != nil {
		return err
	}
	if err := f.root.Rename(tmp, p); err != nil {
		f.root.Remove(tmp)
		return err
	}
	return nil
}

func (f *File) PutIfAbsent(_ context.Context, key string, r io.Reader, size int64) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	tmp, err := f.writeTemp(p, r, size)
	if err != nil {
		return err
	}
	defer f.root.Remove(tmp)
	if err := f.root.Link(tmp, p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	return nil
}

func (f *File) Head(_ context.Context, key string) (Info, error) {
	p, err := f.path(key)
	if err != nil {
		return Info{}, err
	}
	st, err := f.root.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// List returns regular files only. WalkDir does not descend into symlinked
// directories, and symlinks themselves are skipped.
func (f *File) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := fs.WalkDir(f.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		if key := path.Clean(p); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

func (f *File) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := f.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
