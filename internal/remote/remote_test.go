package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/store"
)

type fixture struct {
	dir   string
	store *store.File
	key   crypt.Key
	id    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewFile(filepath.Join(dir, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypt.NewKey()
	f := &fixture{dir: dir, store: s, key: k, id: NewRepoID()}
	if _, err := Init(context.Background(), s, k, f.id, Recipients{Age: []string{"age1test"}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// client opens the remote with its own state directory, like a separate clone.
func (f *fixture) client(t *testing.T, name string) *Remote {
	t.Helper()
	st, err := OpenState(filepath.Join(f.dir, "clients", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Remote{Store: f.store, RepoID: f.id, Key: f.key, State: st}
}

func push(t *testing.T, r *Remote, ref, sha string) *Snapshot {
	t.Helper()
	ctx := context.Background()
	cur, err := r.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := cur.Clone()
	next.Refs[ref] = sha
	s, err := r.Commit(ctx, cur, next)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInitRefusesExisting(t *testing.T) {
	f := newFixture(t)
	if _, err := Init(context.Background(), f.store, f.key, NewRepoID(), Recipients{}); err == nil {
		t.Fatal("second Init succeeded")
	}
	cfg, err := LoadConfig(context.Background(), f.store)
	if err != nil || cfg.RepoID != f.id {
		t.Fatalf("LoadConfig = %+v, %v", cfg, err)
	}
}

func TestLatestFollowsChain(t *testing.T) {
	f := newFixture(t)
	a, b := f.client(t, "a"), f.client(t, "b")
	for i := range 9 {
		push(t, a, "refs/heads/main", string(rune('a'+i)))
	}
	got, err := b.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 9 || got.Refs["refs/heads/main"] != "i" {
		t.Fatalf("fresh client saw seq %d refs %v", got.Seq, got.Refs)
	}
	push(t, a, "refs/heads/main", "z")
	got, err = b.Latest(context.Background())
	if err != nil || got.Seq != 10 {
		t.Fatalf("after another push: seq %d err %v", got.Seq, err)
	}
}

func TestConcurrentCommitConflicts(t *testing.T) {
	f := newFixture(t)
	a, b := f.client(t, "a"), f.client(t, "b")
	ctx := context.Background()
	ca, _ := a.Latest(ctx)
	cb, _ := b.Latest(ctx)
	na, nb := ca.Clone(), cb.Clone()
	na.Refs["refs/heads/a"] = "1"
	nb.Refs["refs/heads/b"] = "2"
	if _, err := a.Commit(ctx, ca, na); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Commit(ctx, cb, nb); !errors.Is(err, ErrConflict) {
		t.Fatalf("second commit: got %v, want ErrConflict", err)
	}
}

func TestRollbackDetected(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	push(t, a, "refs/heads/main", "1")
	push(t, a, "refs/heads/main", "2")
	// Provider deletes the newest manifest, serving an older genuine state.
	if err := f.store.Delete(context.Background(), manifestKey(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Latest(context.Background()); !errors.Is(err, ErrRollback) {
		t.Fatalf("got %v, want ErrRollback", err)
	}
}

func TestForkDetected(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	push(t, a, "refs/heads/main", "1")
	push(t, a, "refs/heads/main", "2")

	// A key holder (e.g. another clone) rewrites manifest 2 after client a
	// verified it. The new one is validly encrypted but a different branch.
	writer := &Remote{Store: f.store, RepoID: f.id, Key: f.key}
	f.store.Delete(context.Background(), manifestKey(2))
	m2 := &Manifest{Version: 1, Seq: 2, Refs: map[string]string{"refs/heads/main": "other"}}
	if _, err := writer.put(context.Background(), m2); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Latest(context.Background()); !errors.Is(err, ErrRollback) {
		t.Fatalf("rewritten manifest: got %v, want ErrRollback", err)
	}

	// A successor that does not chain to what a verified is also refused.
	f2 := newFixture(t)
	c := f2.client(t, "c")
	push(t, c, "refs/heads/main", "1")
	w2 := &Remote{Store: f2.store, RepoID: f2.id, Key: f2.key}
	if _, err := w2.put(context.Background(), &Manifest{Version: 1, Seq: 2, Prev: "not-the-hash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Latest(context.Background()); !errors.Is(err, ErrRollback) {
		t.Fatalf("broken chain: got %v, want ErrRollback", err)
	}
}

func TestSeqMismatchRejected(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	push(t, a, "refs/heads/main", "1")
	// Copy manifest 1's bytes to slot 2: authentic ciphertext, wrong slot.
	b := readKey(t, f.store, manifestKey(1))
	f.store.Put(context.Background(), manifestKey(2), bytes.NewReader(b), int64(len(b)))
	if _, err := a.Latest(context.Background()); err == nil {
		t.Fatal("manifest replayed into a later slot was accepted")
	}
}

func TestForeignKeyManifestRejected(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	push(t, a, "refs/heads/main", "1")
	evilKey, _ := crypt.NewKey()
	evil := &Remote{Store: f.store, RepoID: f.id, Key: evilKey}
	if _, err := evil.put(context.Background(), &Manifest{Version: 1, Seq: 2, Prev: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Latest(context.Background()); err == nil {
		t.Fatal("manifest encrypted under another key was accepted")
	}
}

func TestPackRoundTripAndTamper(t *testing.T) {
	old := PartSize
	PartSize = 100 << 10
	defer func() { PartSize = old }()

	f := newFixture(t)
	a := f.client(t, "a")
	ctx := context.Background()
	plain := make([]byte, 350<<10)
	rand.Read(plain)
	p, err := a.UploadPack(ctx, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Parts) < 4 {
		t.Fatalf("expected multiple parts, got %d", len(p.Parts))
	}
	if err := a.VerifyPackStored(ctx, p); err != nil {
		t.Fatal(err)
	}
	rc := a.OpenPack(ctx, p)
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("pack round trip failed: %v", err)
	}

	// Flip one byte in a stored part.
	path := filepath.Join(f.dir, "remote", "packs", p.ID, "0001")
	b, _ := os.ReadFile(path)
	b[10] ^= 1
	os.WriteFile(path, b, 0o600)
	rc = a.OpenPack(ctx, p)
	_, err = io.ReadAll(rc)
	rc.Close()
	if err == nil {
		t.Fatal("tampered pack read without error")
	}
}

func TestStateNeverMovesBackwards(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Advance("r", 5, "h5")
	st.Advance("r", 3, "h3")
	st.Close()
	st, _ = OpenState(dir)
	defer st.Close()
	if st.Seq != 5 || st.Hash != "h5" {
		t.Fatalf("state = %d %s, want 5 h5", st.Seq, st.Hash)
	}
}

func readKey(t *testing.T, s store.Store, key string) []byte {
	t.Helper()
	b, err := readAll(context.Background(), s, key, maxManifestSize)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGCDeletesOnlyOldUnreferencedPacks(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	ctx := context.Background()
	realNow := now
	defer func() { now = realNow }()

	upload := func() Pack {
		p, err := a.UploadPack(ctx, bytes.NewReader([]byte("pack")), 4)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	age := func(p Pack, d time.Duration) {
		old := time.Now().Add(-d)
		for i := range p.Parts {
			os.Chtimes(filepath.Join(f.dir, "remote", partKey(p.ID, i)), old, old)
		}
	}
	commitPacks := func(packs ...Pack) {
		cur, err := a.Latest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next := cur.Clone()
		next.Packs = packs
		if _, err := a.Commit(ctx, cur, next); err != nil {
			t.Fatal(err)
		}
	}

	// Three days ago: manifest referencing p1. Two days ago: superseded by p2.
	p1, p2 := upload(), upload()
	now = func() time.Time { return realNow().Add(-72 * time.Hour) }
	commitPacks(p1)
	now = func() time.Time { return realNow().Add(-48 * time.Hour) }
	commitPacks(p2)
	// One hour ago: compaction to p3; p2 is still referenced by a recent-ish
	// manifest (the one current at the cutoff) and must survive.
	p3 := upload()
	now = func() time.Time { return realNow().Add(-time.Hour) }
	commitPacks(p3)
	now = realNow
	orphanOld, orphanNew := upload(), upload()
	for _, p := range []Pack{p1, p2, p3, orphanOld} {
		age(p, 96*time.Hour)
	}

	res, err := a.GC(ctx, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	deleted := map[string]bool{}
	for _, id := range res.Deleted {
		deleted[id] = true
	}
	if !deleted[p1.ID] || !deleted[orphanOld.ID] || len(res.Deleted) != 2 {
		t.Fatalf("deleted %v; want p1 %s and orphanOld %s", res.Deleted, p1.ID, orphanOld.ID)
	}
	if len(res.Kept) != 1 || res.Kept[0] != orphanNew.ID {
		t.Fatalf("kept %v; want young orphan %s", res.Kept, orphanNew.ID)
	}
	if err := a.VerifyPackStored(ctx, p2); err != nil {
		t.Fatalf("p2 was deleted but the manifest current at the cutoff references it: %v", err)
	}
	if _, err := a.GC(ctx, time.Hour, false); err == nil {
		t.Fatal("GC accepted a grace period shorter than twice the upload deadline")
	}
}

func TestCommitRefusesStaleOrMissingPack(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	ctx := context.Background()
	commit := func(p Pack) error {
		cur, err := a.Latest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		next := cur.Clone()
		next.Packs = append(next.Packs, p)
		_, err = a.Commit(ctx, cur, next)
		return err
	}
	upload := func() Pack {
		p, err := a.UploadPack(ctx, bytes.NewReader([]byte("pack")), 4)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The machine slept between upload and commit: gc may have run.
	slept := upload()
	slept.stored = time.Now().Add(-UploadDeadline - time.Minute)
	if err := commit(slept); !errors.Is(err, ErrStale) {
		t.Fatalf("commit of a pack stored before the deadline: %v, want ErrStale", err)
	}
	// A pack taken from an old manifest has no upload time.
	fromManifest := upload()
	fromManifest.stored = time.Time{}
	if err := commit(fromManifest); !errors.Is(err, ErrStale) {
		t.Fatalf("commit of a pack with no upload time: %v, want ErrStale", err)
	}
	// A part went missing after upload (a concurrent gc, or a broken store).
	missing := upload()
	if err := f.store.Delete(ctx, partKey(missing.ID, 0)); err != nil {
		t.Fatal(err)
	}
	if err := commit(missing); err == nil {
		t.Fatal("commit referenced a pack with a missing part")
	}
	if err := commit(upload()); err != nil {
		t.Fatalf("fresh pack: %v", err)
	}
	// Packs already in the previous manifest are not re-checked.
	push(t, a, "refs/heads/main", "0123456789012345678901234567890123456789")
}

func TestGCKeepsPackWithRecentPart(t *testing.T) {
	f := newFixture(t)
	a := f.client(t, "a")
	ctx := context.Background()
	defer func(old int64) { PartSize = old }(PartSize)
	PartSize = 64

	// A slow upload: the first part is past the grace period, the last is not.
	p, err := a.UploadPack(ctx, bytes.NewReader(make([]byte, 4096)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Parts) < 2 {
		t.Fatalf("got %d parts, want several", len(p.Parts))
	}
	old := time.Now().Add(-48 * time.Hour)
	for i := range p.Parts[:len(p.Parts)-1] {
		os.Chtimes(filepath.Join(f.dir, "remote", partKey(p.ID, i)), old, old)
	}
	res, err := a.GC(ctx, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 0 || len(res.Kept) != 1 {
		t.Fatalf("deleted %v, kept %v; want the in-progress pack kept", res.Deleted, res.Kept)
	}
	if err := a.VerifyPackStored(ctx, p); err != nil {
		t.Fatal(err)
	}
}
