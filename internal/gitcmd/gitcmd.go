// Package gitcmd runs git subcommands for the remote helper. The helper's own
// stdin and stdout carry git's remote-helper protocol, so children never
// inherit them: stdin is /dev/null or an explicit reader, stdout is captured,
// and only stderr is passed through.
package gitcmd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs commands against one repository.
type Git struct {
	// Dir is the absolute GIT_DIR, or "" when there is no repository.
	Dir string
	// CommonDir holds the object store and shared state; it differs from
	// Dir in linked worktrees.
	CommonDir string
	// Stderr receives git's diagnostics. Defaults to os.Stderr.
	Stderr io.Writer
}

// FromEnv returns a Git for the repository git invoked the helper in.
// GIT_DIR may be relative or unset (e.g. ls-remote outside a repository).
func FromEnv() (*Git, error) {
	g := &Git{}
	if d := os.Getenv("GIT_DIR"); d != "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		g.Dir = abs
	} else if out, err := g.output(nil, "rev-parse", "--absolute-git-dir"); err == nil {
		g.Dir = strings.TrimSpace(out)
	}
	if g.Dir != "" {
		g.CommonDir = g.Dir
		if out, err := g.output(nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			g.CommonDir = strings.TrimSpace(out)
		}
	}
	return g, nil
}

func (g *Git) command(stdin io.Reader, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = os.Environ()
	if g.Dir != "" {
		cmd.Env = append(cmd.Env, "GIT_DIR="+g.Dir)
	}
	cmd.Stdin = stdin // nil means /dev/null
	cmd.Stderr = g.stderr()
	return cmd
}

func (g *Git) stderr() io.Writer {
	if g.Stderr != nil {
		return g.Stderr
	}
	return os.Stderr
}

func (g *Git) output(stdin io.Reader, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := g.command(stdin, args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w", args[0], err)
	}
	return out.String(), nil
}

// quiet runs git with stderr discarded and reports only the exit status.
func (g *Git) quiet(args ...string) (int, error) {
	cmd := g.command(nil, args...)
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// RequireRepo errors if there is no repository.
func (g *Git) RequireRepo() error {
	if g.Dir == "" {
		return errors.New("kura: not inside a git repository")
	}
	return nil
}

// ObjectFormat returns "sha1" or "sha256".
func (g *Git) ObjectFormat() (string, error) {
	out, err := g.output(nil, "rev-parse", "--show-object-format")
	return strings.TrimSpace(out), err
}

// ResolveObject returns the object id that rev names, without peeling tags.
func (g *Git) ResolveObject(rev string) (string, error) {
	out, err := g.output(nil, "rev-parse", "--verify", "--end-of-options", rev)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// HasObject reports whether the object exists locally.
func (g *Git) HasObject(sha string) bool {
	code, err := g.quiet("cat-file", "-e", sha)
	return err == nil && code == 0
}

// Ancestry is the outcome of an ancestry check.
type Ancestry int

const (
	IsAncestor Ancestry = iota
	NotAncestor
	UnknownObject
)

// CheckAncestor reports whether old is an ancestor of new. UnknownObject
// means old is not present locally (the pusher must fetch first).
func (g *Git) CheckAncestor(old, new string) (Ancestry, error) {
	if !g.HasObject(old) {
		return UnknownObject, nil
	}
	code, err := g.quiet("merge-base", "--is-ancestor", old, new)
	switch {
	case err != nil:
		return 0, err
	case code == 0:
		return IsAncestor, nil
	case code == 1:
		return NotAncestor, nil
	default:
		// Not commits (e.g. a tag of a tree): not a fast-forward.
		return NotAncestor, nil
	}
}

// SymbolicHead returns the ref HEAD points to, or "" if detached.
func (g *Git) SymbolicHead() string {
	out, err := g.output(nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ObjectsConnected reports whether every object reachable from wants is
// present locally.
func (g *Git) ObjectsConnected(wants []string) bool {
	if len(wants) == 0 {
		return true
	}
	args := append([]string{"rev-list", "--objects", "--quiet"}, wants...)
	code, err := g.quiet(append(args, "--not", "--all")...)
	return err == nil && code == 0
}

// PackObjects writes a pack of objects reachable from include but not from
// exclude to w and returns the number of objects in it. Packs are not thin,
// so each one is self-contained given the packs before it.
func (g *Git) PackObjects(w io.Writer, include, exclude []string) (uint32, error) {
	cmd := g.command(revs(include, exclude),
		"pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q")
	hw := &headerWriter{w: w}
	cmd.Stdout = hw
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("git pack-objects: %w", err)
	}
	if len(hw.hdr) < 12 || string(hw.hdr[:4]) != "PACK" {
		return 0, errors.New("git pack-objects: missing pack header")
	}
	return binary.BigEndian.Uint32(hw.hdr[8:12]), nil
}

// revs is the stdin for a --revs or --stdin walk of include minus exclude.
func revs(include, exclude []string) io.Reader {
	var b strings.Builder
	for _, s := range include {
		fmt.Fprintln(&b, s)
	}
	for _, s := range exclude {
		fmt.Fprintln(&b, "^"+s)
	}
	return strings.NewReader(b.String())
}

// CrossesGraft reports whether the commits reachable from include but not
// from exclude contain a shallow boundary (.git/shallow) or a grafted commit
// (.git/info/grafts). Git walks such a commit as if it had no parents, or
// different ones, so a pack built from the range can silently lack objects
// that its commits need.
func (g *Git) CrossesGraft(include, exclude []string) (bool, error) {
	cut := map[string]bool{}
	for _, name := range []string{"shallow", filepath.Join("info", "grafts")} {
		b, err := os.ReadFile(filepath.Join(g.commonDir(), name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) > 0 && !strings.HasPrefix(f[0], "#") {
				cut[f[0]] = true
			}
		}
	}
	if len(cut) == 0 || len(include) == 0 {
		return false, nil
	}
	out, err := g.output(revs(include, exclude), "rev-list", "--stdin")
	if err != nil {
		return false, err
	}
	for _, sha := range strings.Fields(out) {
		if cut[sha] {
			return true, nil
		}
	}
	return false, nil
}

type headerWriter struct {
	w   io.Writer
	hdr []byte
}

func (h *headerWriter) Write(p []byte) (int, error) {
	if need := 12 - len(h.hdr); need > 0 {
		h.hdr = append(h.hdr, p[:min(need, len(p))]...)
	}
	return h.w.Write(p)
}

// IndexPack indexes the pack read from r into the object store and returns
// the absolute path of the .keep file protecting it, which the helper hands
// to git as a lock. r is drained afterwards so any trailing error (such as a
// failed authentication check) is reported.
func (g *Git) IndexPack(r io.Reader) (string, error) {
	if err := g.RequireRepo(); err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := g.command(r, "index-pack", "--stdin", "--keep=kura", "--fix-thin")
	cmd.Stdout = &out
	runErr := cmd.Run()
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", fmt.Errorf("git index-pack: %w", runErr)
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if hash, ok := strings.CutPrefix(sc.Text(), "keep\t"); ok {
			return filepath.Join(g.commonDir(), "objects", "pack", "pack-"+hash+".keep"), nil
		}
	}
	return "", nil
}

func (g *Git) commonDir() string {
	if g.CommonDir != "" {
		return g.CommonDir
	}
	return g.Dir
}

// ConfigGet returns a git config value, or "" if it is unset. Any other
// failure (e.g. an unparsable config) is an error, so security settings such
// as a pinned fingerprint are never silently skipped.
func (g *Git) ConfigGet(key string) (string, error) {
	var out bytes.Buffer
	cmd := g.command(nil, "config", "--get", key)
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git config --get %s: %w", key, err)
	}
	return strings.TrimSpace(out.String()), nil
}
