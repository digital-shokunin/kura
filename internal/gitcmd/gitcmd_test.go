package gitcmd

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo creates a repository and returns a Git for it.
func repo(t *testing.T, format string) (*Git, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main", "--object-format="+format)
	return &Git{Dir: filepath.Join(dir, ".git"), Stderr: io.Discard}, dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644)
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func TestObjectFormat(t *testing.T) {
	for _, f := range []string{"sha1", "sha256"} {
		g, _ := repo(t, f)
		if got, err := g.ObjectFormat(); err != nil || got != f {
			t.Errorf("ObjectFormat = %q, %v; want %q", got, err, f)
		}
	}
}

func TestResolveAndAncestry(t *testing.T) {
	g, dir := repo(t, "sha1")
	a := commit(t, dir, "a")
	b := commit(t, dir, "b")
	run(t, dir, "checkout", "-q", "-b", "side", a)
	c := commit(t, dir, "c")

	if got, err := g.ResolveObject("main"); err != nil || got != b {
		t.Fatalf("ResolveObject(main) = %q, %v; want %s", got, err, b)
	}
	if _, err := g.ResolveObject("--output=/tmp/x"); err == nil {
		t.Fatal("ResolveObject treated an option-like name as valid")
	}
	if got := g.SymbolicHead(); got != "refs/heads/side" {
		t.Fatalf("SymbolicHead = %q", got)
	}
	missing := strings.Repeat("1", 40)
	for _, tc := range []struct {
		old, new string
		want     Ancestry
	}{
		{a, b, IsAncestor},
		{b, a, NotAncestor},
		{b, c, NotAncestor},
		{missing, b, UnknownObject},
	} {
		if got, err := g.CheckAncestor(tc.old, tc.new); err != nil || got != tc.want {
			t.Errorf("CheckAncestor(%.7s, %.7s) = %v, %v; want %v", tc.old, tc.new, got, err, tc.want)
		}
	}
	run(t, dir, "checkout", "-q", "--detach")
	if got := g.SymbolicHead(); got != "" {
		t.Errorf("SymbolicHead on a detached HEAD = %q", got)
	}
}

func TestPackObjectsIndexPackRoundTrip(t *testing.T) {
	src, srcDir := repo(t, "sha1")
	a := commit(t, srcDir, "a")
	b := commit(t, srcDir, "b")
	dst, _ := repo(t, "sha1")

	// The first pack holds a; the second only what b adds, and is not thin.
	var full, incr bytes.Buffer
	if n, err := src.PackObjects(&full, []string{a}, nil); err != nil || n != 3 {
		t.Fatalf("PackObjects(a) = %d objects, %v; want 3 (commit, tree, blob)", n, err)
	}
	if n, err := src.PackObjects(&incr, []string{b}, []string{a}); err != nil || n != 3 {
		t.Fatalf("PackObjects(b ^a) = %d objects, %v; want 3", n, err)
	}
	if dst.HasObject(b) || dst.ObjectsConnected([]string{b}) {
		t.Fatal("destination already has b")
	}
	for _, p := range []*bytes.Buffer{&full, &incr} {
		keep, err := dst.IndexPack(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("IndexPack keep file: %v", err)
		}
	}
	if !dst.HasObject(b) {
		t.Fatal("b missing after IndexPack")
	}
	if _, err := dst.IndexPack(strings.NewReader("PACK garbage")); err == nil {
		t.Fatal("IndexPack accepted a corrupt pack")
	}
}

func TestCrossesGraft(t *testing.T) {
	g, dir := repo(t, "sha1")
	a := commit(t, dir, "a")
	b := commit(t, dir, "b")
	c := commit(t, dir, "c")
	crosses := func(include, exclude []string) bool {
		t.Helper()
		got, err := g.CrossesGraft(include, exclude)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if crosses([]string{c}, nil) {
		t.Fatal("complete history reported as cut")
	}
	// A shallow boundary at b: walking from c stops there.
	os.WriteFile(filepath.Join(g.Dir, "shallow"), []byte(b+"\n"), 0o644)
	if !crosses([]string{c}, []string{a}) {
		t.Error("range through the shallow boundary not detected")
	}
	if crosses([]string{c}, []string{b}) {
		t.Error("range above the shallow boundary reported as cut")
	}
	os.Remove(filepath.Join(g.Dir, "shallow"))
	os.WriteFile(filepath.Join(g.Dir, "info", "grafts"), []byte(c+" "+a+"\n"), 0o644)
	if !crosses([]string{c}, []string{b}) {
		t.Error("grafted commit not detected")
	}
	if crosses([]string{c}, []string{c}) {
		t.Error("empty range reported as cut")
	}
}

func TestConfigGet(t *testing.T) {
	g, dir := repo(t, "sha1")
	if v, err := g.ConfigGet("remote.origin.kurafp"); err != nil || v != "" {
		t.Fatalf("unset key = %q, %v", v, err)
	}
	run(t, dir, "config", "remote.origin.kurafp", "abc")
	if v, err := g.ConfigGet("remote.origin.kurafp"); err != nil || v != "abc" {
		t.Fatalf("ConfigGet = %q, %v", v, err)
	}
	// An unparsable config must be an error, never a silently empty pin.
	os.WriteFile(filepath.Join(g.Dir, "config"), []byte("[broken\n"), 0o644)
	if _, err := g.ConfigGet("remote.origin.kurafp"); err == nil {
		t.Fatal("ConfigGet ignored a broken config")
	}
}

func TestRequireRepo(t *testing.T) {
	if err := (&Git{}).RequireRepo(); err == nil {
		t.Fatal("RequireRepo passed without a repository")
	}
	if _, err := (&Git{}).IndexPack(strings.NewReader("")); err == nil {
		t.Fatal("IndexPack ran without a repository")
	}
}
