// Package store defines the minimal object storage interface kura needs and
// opens backends by URL.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

var (
	// ErrExists is returned by PutIfAbsent when the key already exists.
	ErrExists = errors.New("store: object already exists")
	// ErrNotFound is returned when a key does not exist.
	ErrNotFound = errors.New("store: object not found")
)

// Info describes a stored object.
type Info struct {
	Size    int64
	ModTime time.Time
}

// Store is a flat key/value object store. Keys use "/" as a separator.
type Store interface {
	// Get returns the object's contents, or ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Put creates or replaces an object.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// PutIfAbsent creates an object atomically, returning ErrExists if the
	// key is already present. This is the only atomic primitive kura uses.
	PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error
	// Head returns object metadata, or ErrNotFound.
	Head(ctx context.Context, key string) (Info, error)
	// List returns keys under prefix in lexical order. It is only a hint:
	// some backends' listings lag behind writes.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes an object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// Opener constructs a Store for a parsed URL.
type Opener func(ctx context.Context, u *url.URL) (Store, error)

var openers = map[string]Opener{}

// Register makes a backend available under a URL scheme.
func Register(scheme string, o Opener) { openers[scheme] = o }

// Open returns the Store for rawURL, e.g. file:///path or s3://bucket/prefix.
// Any query string must already have been stripped.
func Open(ctx context.Context, rawURL string) (Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("store: bad url %q: %w", rawURL, err)
	}
	o, ok := openers[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("store: unsupported scheme %q", u.Scheme)
	}
	return o(ctx, u)
}
