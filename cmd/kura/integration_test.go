package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

var kuraBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kura-it-bin-*")
	if err != nil {
		panic(err)
	}
	kuraBin = filepath.Join(dir, "kura")
	if out, err := exec.Command("go", "build", "-o", kuraBin, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	if err := os.Symlink("kura", filepath.Join(dir, "git-remote-kura")); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is an isolated world: its own HOME, keystore, git config and PATH.
type env struct {
	t         *testing.T
	root      string
	vars      []string
	url       string // kura::file://...?fp=...
	identity  string // age identity file
	recipient string // its public recipient
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root}
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o700)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || strings.HasPrefix(k, "GIT_") || k == "KURA_KEYSTORE_DIR" || k == "XDG_CONFIG_HOME" {
			continue
		}
		e.vars = append(e.vars, kv)
	}
	e.vars = append(e.vars,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"PATH="+filepath.Dir(kuraBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KURA_KEYSTORE_DIR="+filepath.Join(root, "keystore"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	e.recipient = id.Recipient().String()
	e.identity = filepath.Join(root, "identity.txt")
	os.WriteFile(e.identity, []byte(id.String()+"\n"), 0o600)

	out := e.kura("init", "file://"+filepath.Join(root, "remote"), "--age", id.Recipient().String())
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  git remote add origin '") {
			e.url = strings.TrimSuffix(strings.TrimPrefix(line, "  git remote add origin '"), "'")
		}
	}
	if !strings.Contains(e.url, "?fp=") {
		t.Fatalf("init did not print a pinned url:\n%s", out)
	}
	return e
}

func (e *env) cmd(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.vars
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (e *env) kura(args ...string) string {
	e.t.Helper()
	out, err := e.cmd(e.root, kuraBin, args...)
	if err != nil {
		e.t.Fatalf("kura %v: %v\n%s", args, err, out)
	}
	return out
}

func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.cmd(dir, "git", args...)
	if err != nil {
		e.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(out)
}

func (e *env) gitFail(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.cmd(dir, "git", args...)
	if err == nil {
		e.t.Fatalf("git %v in %s unexpectedly succeeded:\n%s", args, dir, out)
	}
	return out
}

func (e *env) clone(name string) string {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	e.git(e.root, "clone", "-q", e.url, dir)
	return dir
}

func (e *env) newRepo(name, branch string) string {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	e.git(e.root, "init", "-q", "-b", branch, dir)
	e.git(dir, "remote", "add", "origin", e.url)
	return dir
}

func (e *env) commit(dir, file, content string) string {
	e.t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "update "+file)
	return e.git(dir, "rev-parse", "HEAD")
}

func (e *env) remoteRef(dir, ref string) string {
	e.t.Helper()
	out := e.git(dir, "ls-remote", "origin", ref)
	sha, _, _ := strings.Cut(out, "\t")
	return sha
}

func TestCloneEmptyPushFetch(t *testing.T) {
	e := newEnv(t)
	a := e.clone("a")
	e.git(a, "checkout", "-q", "-b", "main")
	c1 := e.commit(a, "notes/hello.md", "# hello\n")
	e.git(a, "push", "-q", "-u", "origin", "main")

	b := e.clone("b")
	if got := e.git(b, "rev-parse", "HEAD"); got != c1 {
		t.Fatalf("clone HEAD %s, want %s", got, c1)
	}
	if got := e.git(b, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Fatalf("clone HEAD points at %s", got)
	}
	c2 := e.commit(a, "notes/two.md", "two\n")
	e.git(a, "push", "-q")
	e.git(b, "pull", "-q", "--ff-only")
	if got := e.git(b, "rev-parse", "HEAD"); got != c2 {
		t.Fatalf("after pull HEAD %s, want %s", got, c2)
	}
	if out := e.git(b, "fsck", "--full", "--no-dangling"); strings.Contains(out, "missing") {
		t.Fatalf("fsck: %s", out)
	}
}

func TestNonMainHead(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "trunk")
	e.commit(a, "f", "x")
	e.git(a, "push", "-q", "origin", "trunk")
	b := e.clone("b")
	if got := e.git(b, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Fatalf("clone HEAD = %s, want refs/heads/trunk", got)
	}
}

func TestDivergedPushRejectedThenForce(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", "base")
	e.git(a, "push", "-q", "-u", "origin", "main")
	b := e.clone("b")

	ca := e.commit(a, "f", "from a")
	e.git(a, "push", "-q")
	cb := e.commit(b, "f", "from b")
	out := e.gitFail(b, "push", "origin", "main")
	if !strings.Contains(out, "rejected") {
		t.Fatalf("expected rejection, got:\n%s", out)
	}
	if got := e.remoteRef(a, "refs/heads/main"); got != ca {
		t.Fatalf("remote main = %s, want %s", got, ca)
	}
	// Stale lease is refused, correct lease succeeds.
	out = e.gitFail(b, "push", "--force-with-lease=main:"+cb, "origin", "main")
	if !strings.Contains(out, "stale info") && !strings.Contains(out, "rejected") {
		t.Fatalf("expected stale lease rejection, got:\n%s", out)
	}
	e.git(b, "push", "-q", "--force-with-lease=main:"+ca, "origin", "main")
	if got := e.remoteRef(a, "refs/heads/main"); got != cb {
		t.Fatalf("after leased force push remote main = %s, want %s", got, cb)
	}
	// a can recover with fetch + reset.
	e.git(a, "fetch", "-q")
	if got := e.git(a, "rev-parse", "origin/main"); got != cb {
		t.Fatalf("a sees origin/main %s, want %s", got, cb)
	}
}

func TestTagsDeletesDryRunAtomic(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	c1 := e.commit(a, "f", "1")
	e.git(a, "tag", "-a", "v1", "-m", "release v1")
	e.git(a, "branch", "topic")
	e.git(a, "push", "-q", "origin", "main", "topic", "v1")
	tagObj := e.git(a, "rev-parse", "v1")
	if got := e.remoteRef(a, "refs/tags/v1"); got != tagObj {
		t.Fatalf("remote tag = %s, want tag object %s", got, tagObj)
	}
	b := e.clone("b")
	if got := e.git(b, "rev-parse", "v1^{commit}"); got != c1 {
		t.Fatalf("cloned tag peels to %s", got)
	}

	// Dry run changes nothing.
	e.commit(a, "f", "2")
	e.git(a, "push", "-q", "--dry-run", "origin", "main")
	if got := e.remoteRef(a, "refs/heads/main"); got != c1 {
		t.Fatalf("dry run moved main to %s", got)
	}

	// Atomic push with one rejected ref applies nothing.
	e.git(b, "checkout", "-q", "-b", "topic", "origin/topic")
	e.commit(b, "g", "topic work")
	e.git(b, "push", "-q", "origin", "topic")
	e.commit(a, "h", "diverged topic")
	e.git(a, "branch", "-f", "topic", "HEAD")
	e.gitFail(a, "push", "--atomic", "origin", "main", "topic")
	if got := e.remoteRef(a, "refs/heads/main"); got != c1 {
		t.Fatalf("atomic push partially applied: main = %s", got)
	}

	// Delete a branch.
	e.git(b, "push", "-q", "origin", ":topic")
	if got := e.remoteRef(a, "refs/heads/topic"); got != "" {
		t.Fatalf("topic still exists: %s", got)
	}
}

func TestParallelPushesToDifferentBranches(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", "base")
	e.git(a, "push", "-q", "-u", "origin", "main")
	clones := []string{e.clone("c0"), e.clone("c1"), e.clone("c2")}
	for round := range 3 {
		errs := make(chan error, len(clones))
		for i, dir := range clones {
			branch := fmt.Sprintf("b%d", i)
			if round == 0 {
				e.git(dir, "checkout", "-q", "-b", branch)
			}
			e.commit(dir, branch+".md", fmt.Sprintf("round %d", round))
			go func() {
				out, err := e.cmd(dir, "git", "push", "-q", "origin", branch)
				if err != nil {
					err = fmt.Errorf("%s: %v\n%s", branch, err, out)
				}
				errs <- err
			}()
		}
		for range clones {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, dir := range clones {
		branch := fmt.Sprintf("refs/heads/b%d", i)
		if got, want := e.remoteRef(a, branch), e.git(dir, "rev-parse", "HEAD"); got != want {
			t.Fatalf("%s = %s, want %s", branch, got, want)
		}
	}
	fresh := e.clone("fresh")
	e.git(fresh, "fsck", "--full", "--no-dangling")
}

func TestCompactThenCloneAndIncrementalFetch(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	for i := range 5 {
		e.commit(a, fmt.Sprintf("n%d.md", i), strings.Repeat("x", i*1000))
		e.git(a, "push", "-q", "-u", "origin", "main")
	}
	old := e.clone("old")
	e.commit(a, "after.md", "y")
	e.git(a, "push", "-q")

	out := e.kura("compact", e.url)
	if !strings.Contains(out, "Compacted 6 packs") {
		t.Fatalf("compact output:\n%s", out)
	}
	tip := e.commit(a, "post.md", "z")
	e.git(a, "push", "-q")

	fresh := e.clone("fresh")
	if got := e.git(fresh, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("fresh clone HEAD %s, want %s", got, tip)
	}
	e.git(old, "pull", "-q", "--ff-only")
	if got := e.git(old, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("old clone HEAD %s, want %s", got, tip)
	}
	out = e.kura("gc", e.url, "--dry-run")
	if !strings.Contains(out, "Would delete 0") {
		t.Fatalf("gc should keep young packs:\n%s", out)
	}
	e.kura("verify", e.url, "--repo", a)
}

func TestTamperedPackFailsLoudly(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", strings.Repeat("secret ", 5000))
	e.git(a, "push", "-q", "origin", "main")
	parts, _ := filepath.Glob(filepath.Join(e.root, "remote", "packs", "*", "0000"))
	if len(parts) != 1 {
		t.Fatalf("expected one pack part, got %v", parts)
	}
	b, _ := os.ReadFile(parts[0])
	b[len(b)/2] ^= 0xff
	os.WriteFile(parts[0], b, 0o600)
	out := e.gitFail(e.root, "clone", e.url, filepath.Join(e.root, "b"))
	if !strings.Contains(out, "does not match manifest") {
		t.Fatalf("expected integrity error, got:\n%s", out)
	}
}

func TestRollbackRefused(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", "1")
	e.git(a, "push", "-q", "-u", "origin", "main")
	e.commit(a, "f", "2")
	e.git(a, "push", "-q")
	// Provider deletes the newest manifest to serve the older state.
	m, _ := filepath.Glob(filepath.Join(e.root, "remote", "manifests", "*"))
	os.Remove(m[len(m)-1])
	out := e.gitFail(a, "fetch")
	if !strings.Contains(out, "rollback") {
		t.Fatalf("expected rollback error, got:\n%s", out)
	}
}

func TestNoKeyFailsCleanlyAndUnlockWorks(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", "1")
	e.git(a, "push", "-q", "-u", "origin", "main")

	e.kura("lock", e.url)
	out := e.gitFail(a, "push", "origin", "main")
	if !strings.Contains(out, "kura unlock") {
		t.Fatalf("expected unlock hint, got:\n%s", out)
	}
	// Wrong pinned fingerprint is refused even with the right identity.
	bad := strings.Replace(e.url, "fp=", "fp=00", 1)
	if out, err := e.cmd(e.root, kuraBin, "unlock", bad, "-i", e.identity); err == nil || !strings.Contains(out, "mismatch") {
		t.Fatalf("unlock with wrong fp: err=%v\n%s", err, out)
	}
	e.kura("unlock", e.url, "-i", e.identity)
	e.commit(a, "f", "2")
	e.git(a, "push", "-q")
}

func TestSubstitutedKeyFileRefused(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "f", "1")
	e.git(a, "push", "-q", "-u", "origin", "main")

	// The provider knows the public recipient, so it can wrap its own key K'
	// to it. Build such a key file and swap it in.
	e.kura("init", "file://"+filepath.Join(e.root, "evil"), "--age", e.recipient)
	evil, err := os.ReadFile(filepath.Join(e.root, "evil", "keys", "age.age"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, "remote", "keys", "age.age"), evil, 0o600)
	e.kura("lock", e.url)

	// With the fingerprint pinned, unlock refuses K'.
	out, err := e.cmd(e.root, kuraBin, "unlock", e.url, "-i", e.identity)
	if err == nil || !strings.Contains(out, "mismatch") {
		t.Fatalf("pinned unlock accepted substituted key: err=%v\n%s", err, out)
	}
	// Even unpinned, K' cannot authenticate the existing manifests.
	unpinned := strings.Split(e.url, "?")[0]
	out, err = e.cmd(e.root, kuraBin, "unlock", unpinned, "-i", e.identity)
	if err == nil || !strings.Contains(out, "does not open this repository") {
		t.Fatalf("unpinned unlock accepted substituted key: err=%v\n%s", err, out)
	}
}

func TestProbeFileBackend(t *testing.T) {
	e := newEnv(t)
	out := e.kura("probe", "file://"+filepath.Join(e.root, "probe"))
	if !strings.Contains(out, "All checks passed") {
		t.Fatalf("probe:\n%s", out)
	}
}

func TestSHA256Repository(t *testing.T) {
	e := newEnv(t)
	a := filepath.Join(e.root, "a")
	e.git(e.root, "init", "-q", "-b", "main", "--object-format=sha256", a)
	e.git(a, "remote", "add", "origin", e.url)
	c1 := e.commit(a, "f", "sha256 content")
	if len(c1) != 64 {
		t.Fatalf("expected a sha256 commit id, got %s", c1)
	}
	e.git(a, "push", "-q", "-u", "origin", "main")
	b := e.clone("b")
	if got := e.git(b, "rev-parse", "--show-object-format"); got != "sha256" {
		t.Fatalf("clone object format = %s, want sha256", got)
	}
	if got := e.git(b, "rev-parse", "HEAD"); got != c1 {
		t.Fatalf("clone HEAD %s, want %s", got, c1)
	}
	// A sha1 repository cannot push into it.
	s1 := e.newRepo("s1", "main")
	e.commit(s1, "g", "sha1 content")
	if out := e.gitFail(s1, "push", "origin", "main:other"); !strings.Contains(out, "sha256") {
		t.Fatalf("expected object format mismatch, got:\n%s", out)
	}
	if out := e.kura("info", e.url); !strings.Contains(out, "Object format: sha256") {
		t.Fatalf("info:\n%s", out)
	}
	e.kura("compact", e.url)
	e.kura("verify", e.url, "--repo", a)
}

func TestIncrementalPacksAreMerged(t *testing.T) {
	e := newEnv(t)
	a := e.newRepo("a", "main")
	e.commit(a, "big.md", strings.Repeat("base content\n", 2000))
	e.git(a, "push", "-q", "-u", "origin", "main")
	old := e.clone("old")
	for i := range 26 {
		e.commit(a, fmt.Sprintf("n%02d.md", i), fmt.Sprintf("note %d\n", i))
		e.git(a, "push", "-q")
	}
	// 27 pushes; the merge at 25 packs leaves 2, plus the 2 pushes after it.
	out := e.kura("info", e.url)
	if !strings.Contains(out, "Packs:         4 ") {
		t.Fatalf("expected incremental packs to be merged down to 4:\n%s", out)
	}
	tip := e.git(a, "rev-parse", "HEAD")
	e.git(old, "pull", "-q", "--ff-only")
	if got := e.git(old, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("old clone HEAD %s, want %s", got, tip)
	}
	fresh := e.clone("fresh")
	if got := e.git(fresh, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("fresh clone HEAD %s, want %s", got, tip)
	}
	e.kura("verify", e.url, "--repo", a)
}

func TestRecipientsLiveInManifest(t *testing.T) {
	e := newEnv(t)
	id2, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	id2File := filepath.Join(e.root, "id2.txt")
	os.WriteFile(id2File, []byte(id2.String()+"\n"), 0o600)

	out := e.kura("recipients", e.url, "add", "--age", id2.Recipient().String())
	if !strings.Contains(out, id2.Recipient().String()) || !strings.Contains(out, e.recipient) {
		t.Fatalf("recipients add output:\n%s", out)
	}
	e.kura("lock", e.url)
	e.kura("unlock", e.url, "-i", id2File)

	e.kura("recipients", e.url, "remove", "--age", e.recipient)
	out = e.kura("recipients", e.url, "list")
	if strings.Contains(out, e.recipient) || !strings.Contains(out, id2.Recipient().String()) {
		t.Fatalf("recipients list after remove:\n%s", out)
	}
	e.kura("lock", e.url)
	if out, err := e.cmd(e.root, kuraBin, "unlock", e.url, "-i", e.identity); err == nil {
		t.Fatalf("removed identity still unwraps the key file:\n%s", out)
	}
	if out, err := e.cmd(e.root, kuraBin, "recipients", e.url, "remove", "--age", id2.Recipient().String()); err == nil {
		t.Fatalf("removing the last recipient was allowed:\n%s", out)
	}
}

func TestKeyCacheDirOverride(t *testing.T) {
	e := newEnv(t)
	cache := filepath.Join(e.root, "drill")
	e.vars = append(e.vars, "KURA_KEYCACHE_DIR="+cache)
	e.kura("unlock", e.url, "-i", e.identity)
	keys, _ := filepath.Glob(filepath.Join(cache, "kura", "*"))
	if len(keys) != 1 {
		t.Fatalf("expected the unwrapped key in the override dir, found %v", keys)
	}
	// The main store still holds the key from init, untouched.
	main, _ := filepath.Glob(filepath.Join(e.root, "keystore", "kura", "*"))
	if len(main) != 1 {
		t.Fatalf("main keystore changed: %v", main)
	}
	e.kura("lock", e.url)
	if keys, _ := filepath.Glob(filepath.Join(cache, "kura", "*")); len(keys) != 0 {
		t.Fatalf("lock did not clear the override dir: %v", keys)
	}
	if main, _ := filepath.Glob(filepath.Join(e.root, "keystore", "kura", "*")); len(main) != 1 {
		t.Fatalf("lock with override touched the main keystore: %v", main)
	}
}

// An interrupted or losing `recipients` writer can leave key files that match
// no manifest. `recipients sync` rewrites them from the manifest.
func TestRecipientsSyncRepairsKeyFiles(t *testing.T) {
	e := newEnv(t)
	id2, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	id2File := filepath.Join(e.root, "id2.txt")
	os.WriteFile(id2File, []byte(id2.String()+"\n"), 0o600)
	keyFile := filepath.Join(e.root, "remote", "keys", "age.age")
	oldKeys, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	e.kura("recipients", e.url, "add", "--age", id2.Recipient().String())
	// A concurrent writer that read the old list overwrites the key files,
	// then dies before committing.
	if err := os.WriteFile(keyFile, oldKeys, 0o644); err != nil {
		t.Fatal(err)
	}
	e.kura("lock", e.url)
	out, err := e.cmd(e.root, kuraBin, "unlock", e.url, "-i", id2File)
	if err == nil || !strings.Contains(out, "recipients <url> sync") {
		t.Fatalf("unlock with key files missing a recipient: err=%v\n%s", err, out)
	}

	e.kura("unlock", e.url, "-i", e.identity)
	out = e.kura("recipients", e.url, "sync")
	if !strings.Contains(out, id2.Recipient().String()) {
		t.Fatalf("recipients sync output:\n%s", out)
	}
	e.kura("lock", e.url)
	e.kura("unlock", e.url, "-i", id2File)
}
