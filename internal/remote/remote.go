// Package remote implements kura's on-remote format: the plaintext config,
// the append-only chain of encrypted manifests, and encrypted pack storage.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"time"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/store"
)

const (
	configKey       = "config.json"
	maxManifestSize = 16 << 20
	formatName      = "kura"
	// maxSeq bounds the probe for the newest manifest, so a provider that
	// claims every key exists cannot make it loop forever.
	maxSeq = 1 << 40
)

var repoIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	// ErrConflict means another writer committed the next manifest first.
	ErrConflict = errors.New("remote: concurrent update, manifest sequence taken")
	// ErrRollback means the remote no longer extends the chain we last saw.
	ErrRollback = errors.New("remote: manifest chain does not extend local high-water mark (rollback or fork)")
	// ErrUnknownOutcome means a manifest write may or may not have landed.
	// Callers must not delete anything that write referenced.
	ErrUnknownOutcome = errors.New("remote: manifest write outcome unknown")
	// ErrStale means a manifest would reference a pack stored longer than
	// UploadDeadline ago, which a concurrent gc may already have deleted.
	ErrStale = errors.New("remote: uploaded pack is too old to commit")
)

// Config is the only plaintext metadata on the remote. RepoID is bound into
// every ciphertext and the key fingerprint, so altering it breaks decryption.
type Config struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	RepoID  string `json:"repo_id"`
}

// LoadConfig reads and validates config.json.
func LoadConfig(ctx context.Context, s store.Store) (Config, error) {
	var c Config
	b, err := readAll(ctx, s, configKey, 1<<16)
	if errors.Is(err, store.ErrNotFound) {
		return c, fmt.Errorf("remote: no kura repository here (missing %s)", configKey)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("remote: bad %s: %w", configKey, err)
	}
	if c.Format != formatName || c.Version != crypt.FormatVersion {
		return c, fmt.Errorf("remote: unsupported repository format %q v%d", c.Format, c.Version)
	}
	if !repoIDPattern.MatchString(c.RepoID) {
		return c, fmt.Errorf("remote: malformed repo id in %s", configKey)
	}
	return c, nil
}

// NewRepoID returns a random repository id.
func NewRepoID() string { return randHex(16) }

// Remote is an opened kura repository.
type Remote struct {
	Store  store.Store
	RepoID string
	Key    crypt.Key
	// State, if set, is the local high-water mark used for rollback detection.
	State *State
	// Logf reports progress and trust decisions to the user (stderr).
	Logf func(format string, args ...any)
}

func (r *Remote) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Init writes config.json and the empty genesis manifest, which records who
// the key is wrapped to. It fails if a repository already exists there.
func Init(ctx context.Context, s store.Store, key crypt.Key, repoID string, rc Recipients) (*Remote, error) {
	cfg, _ := json.Marshal(Config{Format: formatName, Version: crypt.FormatVersion, RepoID: repoID})
	if err := s.PutIfAbsent(ctx, configKey, bytes.NewReader(cfg), int64(len(cfg))); err != nil {
		if errors.Is(err, store.ErrExists) {
			return nil, errors.New("remote: a repository already exists at this location")
		}
		return nil, err
	}
	r := &Remote{Store: s, RepoID: repoID, Key: key}
	genesis := &Manifest{Version: crypt.FormatVersion, Seq: 0, Refs: map[string]string{},
		Recipients: rc, Writer: writerName(), Time: now()}
	if _, err := r.put(ctx, genesis); err != nil {
		return nil, err
	}
	return r, nil
}

// Manifest is the authenticated description of the remote's refs and packs.
type Manifest struct {
	Version int               `json:"v"`
	Seq     uint64            `json:"seq"`
	Prev    string            `json:"prev,omitempty"`
	Refs    map[string]string `json:"refs"`
	Head    string            `json:"head,omitempty"`
	Packs   []Pack            `json:"packs"`
	// Base is the ref state whose objects Packs[0] contains in full. Merging
	// the later, incremental packs only needs objects not reachable from it.
	Base map[string]string `json:"base,omitempty"`
	// Recipients lists who K is wrapped to. Keeping it in the authenticated
	// chain stops a provider from replaying an older list.
	Recipients Recipients `json:"recipients"`
	// ObjectFormat is the git hash algorithm of the repository ("sha1" if
	// empty). It is recorded by the first push.
	ObjectFormat string    `json:"object_format,omitempty"`
	Writer       string    `json:"writer"`
	Time         time.Time `json:"time"`
}

// Format returns the repository's git object format.
func (m *Manifest) Format() string {
	if m.ObjectFormat == "" {
		return "sha1"
	}
	return m.ObjectFormat
}

// Recipients are the public keys K is wrapped to.
type Recipients struct {
	Age []string `json:"age,omitempty"`
	GPG []string `json:"gpg,omitempty"`
}

// Empty reports whether there are no recipients at all.
func (r Recipients) Empty() bool { return len(r.Age) == 0 && len(r.GPG) == 0 }

// Pack is one encrypted git packfile, stored in parts.
type Pack struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Size    int64     `json:"size"`
	Parts   []Part    `json:"parts"`

	stored time.Time // wall clock when UploadPack stored the last part; zero if read from a manifest
}

// Part is one stored chunk of an encrypted pack.
type Part struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Snapshot is a verified manifest plus the hash of its stored bytes, which
// the next manifest must reference as Prev.
type Snapshot struct {
	*Manifest
	Hash string
}

// Clone returns a deep copy of m suitable for building the next manifest.
func (m *Manifest) Clone() *Manifest {
	c := *m
	c.Refs = maps.Clone(m.Refs)
	if c.Refs == nil {
		c.Refs = map[string]string{}
	}
	c.Base = maps.Clone(m.Base)
	c.Packs = append([]Pack(nil), m.Packs...)
	c.Recipients.Age = append([]string(nil), m.Recipients.Age...)
	c.Recipients.GPG = append([]string(nil), m.Recipients.GPG...)
	return &c
}

// HasPack reports whether the manifest references pack id.
func (m *Manifest) HasPack(id string) bool {
	for _, p := range m.Packs {
		if p.ID == id {
			return true
		}
	}
	return false
}

func manifestKey(seq uint64) string { return fmt.Sprintf("manifests/%012d", seq) }

func (r *Remote) readManifest(ctx context.Context, seq uint64) (*Snapshot, error) {
	key := manifestKey(seq)
	ct, err := readAll(ctx, r.Store, key, maxManifestSize)
	if err != nil {
		return nil, err
	}
	plain, err := r.Key.Open(ct, r.RepoID, key)
	if err != nil {
		return nil, fmt.Errorf("remote: manifest %d failed authentication: %w", seq, err)
	}
	var m Manifest
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("remote: manifest %d: %w", seq, err)
	}
	if m.Seq != seq {
		return nil, fmt.Errorf("remote: manifest stored at %d claims seq %d", seq, m.Seq)
	}
	if m.Version != crypt.FormatVersion {
		return nil, fmt.Errorf("remote: manifest %d has unsupported version %d", seq, m.Version)
	}
	if m.Refs == nil {
		m.Refs = map[string]string{}
	}
	return &Snapshot{Manifest: &m, Hash: hashHex(ct)}, nil
}

// ReadManifest returns the verified manifest at seq.
func (r *Remote) ReadManifest(ctx context.Context, seq uint64) (*Snapshot, error) {
	return r.readManifest(ctx, seq)
}

func (r *Remote) exists(ctx context.Context, seq uint64) (bool, error) {
	_, err := r.Store.Head(ctx, manifestKey(seq))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// findLatestSeq locates the highest manifest by probing, never trusting
// listings: exponential search for a missing seq, then bisection.
func (r *Remote) findLatestSeq(ctx context.Context) (uint64, error) {
	ok, err := r.exists(ctx, 0)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("remote: genesis manifest missing")
	}
	lo, hi := uint64(0), uint64(1)
	for {
		if hi > maxSeq {
			return 0, errors.New("remote: implausible manifest count; refusing to probe further")
		}
		ok, err := r.exists(ctx, hi)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := r.exists(ctx, mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// Latest returns the newest verified manifest. With a local high-water mark
// it walks the chain forward from it, refusing rollback or forks; without one
// it trusts the newest manifest it finds and says so.
func (r *Remote) Latest(ctx context.Context) (*Snapshot, error) {
	st := r.State
	if st != nil && st.Hash != "" {
		if st.RepoID != r.RepoID {
			return nil, fmt.Errorf("remote: local state is for repo %s but remote is %s", st.RepoID, r.RepoID)
		}
		cur, err := r.readManifest(ctx, st.Seq)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: manifest %d is gone", ErrRollback, st.Seq)
		}
		if err != nil {
			return nil, err
		}
		if cur.Hash != st.Hash {
			return nil, fmt.Errorf("%w: manifest %d changed since last seen", ErrRollback, st.Seq)
		}
		for {
			next, err := r.readManifest(ctx, cur.Seq+1)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return nil, err
			}
			if next.Prev != cur.Hash {
				return nil, fmt.Errorf("%w: manifest %d does not chain to %d", ErrRollback, next.Seq, cur.Seq)
			}
			cur = next
		}
		return cur, r.advance(cur)
	}
	seq, err := r.findLatestSeq(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := r.readManifest(ctx, seq)
	if err != nil {
		return nil, err
	}
	if st != nil {
		r.logf("kura: first contact with this remote; trusting manifest seq=%d written by %q at %s",
			cur.Seq, cur.Writer, cur.Time.Format(time.RFC3339))
	}
	return cur, r.advance(cur)
}

func (r *Remote) advance(s *Snapshot) error {
	if r.State == nil {
		return nil
	}
	return r.State.Advance(r.RepoID, s.Seq, s.Hash)
}

// Commit writes next as the successor of prev. It returns ErrConflict if
// another writer took that sequence number.
func (r *Remote) Commit(ctx context.Context, prev *Snapshot, next *Manifest) (*Snapshot, error) {
	if err := r.checkNewPacks(ctx, prev, next); err != nil {
		return nil, err
	}
	next.Version = crypt.FormatVersion
	next.Seq = prev.Seq + 1
	next.Prev = prev.Hash
	next.Writer = writerName()
	next.Time = now()
	if next.Refs == nil {
		next.Refs = map[string]string{}
	}
	s, err := r.put(ctx, next)
	if err != nil {
		return nil, err
	}
	return s, r.advance(s)
}

// checkNewPacks makes sure every pack next adds is fully stored and fresh
// enough that gc cannot have deleted it as an orphan. It returns ErrStale for a
// pack stored more than UploadDeadline ago.
func (r *Remote) checkNewPacks(ctx context.Context, prev *Snapshot, next *Manifest) error {
	for _, p := range next.Packs {
		if prev.HasPack(p.ID) {
			continue
		}
		if err := r.VerifyPackStored(ctx, p); err != nil {
			return err
		}
		if p.Stale() {
			return fmt.Errorf("%w: pack %s", ErrStale, p.ID)
		}
	}
	return nil
}

func (r *Remote) put(ctx context.Context, m *Manifest) (*Snapshot, error) {
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	key := manifestKey(m.Seq)
	ct, err := r.Key.Seal(plain, r.RepoID, key)
	if err != nil {
		return nil, err
	}
	err = r.Store.PutIfAbsent(ctx, key, bytes.NewReader(ct), int64(len(ct)))
	if errors.Is(err, store.ErrExists) {
		// A retried request may have succeeded the first time, so only a
		// different stored manifest is a conflict.
		var stored []byte
		var rerr error
		for attempt := range 3 {
			if stored, rerr = readAll(ctx, r.Store, key, maxManifestSize); rerr == nil {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
		switch {
		case rerr != nil:
			return nil, fmt.Errorf("%w: cannot re-read %s: %v", ErrUnknownOutcome, key, rerr)
		case !bytes.Equal(stored, ct):
			return nil, ErrConflict
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return &Snapshot{Manifest: m, Hash: hashHex(ct)}, nil
}

func readAll(ctx context.Context, s store.Store, key string, limit int64) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("remote: %s exceeds %d bytes", key, limit)
	}
	return b, nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writerName() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

var now = func() time.Time { return time.Now().UTC().Round(time.Second) }
