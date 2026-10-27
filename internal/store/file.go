package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
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
type File struct {
	root string
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
	return &File{root: abs}, nil
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
	return filepath.Join(f.root, filepath.FromSlash(key)), nil
}

func (f *File) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fh, err
}

// writeTemp writes r to a temp file next to final and fsyncs it.
func (f *File) writeTemp(final string, r io.Reader, size int64) (string, error) {
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
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
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
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
	defer os.Remove(tmp)
	if err := os.Link(tmp, p); err != nil {
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
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (f *File) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
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
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
