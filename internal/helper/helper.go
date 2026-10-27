// Package helper implements git's remote-helper protocol (gitremote-helpers(7))
// with the fetch, push and option capabilities on top of a kura remote.
package helper

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/digital-shokunin/kura/internal/gitcmd"
	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/remote"
	"github.com/digital-shokunin/kura/internal/repo"
)

// Main runs the helper. args are git's arguments: <remote-name> [<url>].
func Main(args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: git-remote-kura <remote> [<url>] (invoked by git)")
		return 2
	}
	h, err := newHelper(args[0], args[len(args)-1], os.Stdin, os.Stdout)
	if err == nil {
		defer h.close()
		err = h.run(context.Background())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kura: %v\n", err)
		return 1
	}
	return 0
}

type options struct {
	verbosity    int
	objectFormat bool // git asked for ":object-format" in list output
	progress     bool
	dryRun       bool
	atomic       bool
	cas          map[string]string // ref -> expected old value ("" = must not exist)
}

type helper struct {
	name   string
	target repo.Target
	in     *bufio.Reader
	out    *bufio.Writer
	git    *gitcmd.Git
	opts   options

	rem  *remote.Remote // opened lazily
	snap *remote.Snapshot

	objectFormat string // local repository's format, set during push
}

func newHelper(name, rawURL string, in io.Reader, out io.Writer) (*helper, error) {
	t, err := repo.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	g, err := gitcmd.FromEnv()
	if err != nil {
		return nil, err
	}
	return &helper{
		name:   name,
		target: t,
		in:     bufio.NewReader(in),
		out:    bufio.NewWriter(out),
		git:    g,
		opts:   options{verbosity: 1, cas: map[string]string{}},
	}, nil
}

func (h *helper) close() {
	if h.rem != nil && h.rem.State != nil {
		h.rem.State.Close()
	}
}

func (h *helper) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func (h *helper) progressf(format string, args ...any) {
	if h.opts.progress || h.opts.verbosity > 1 {
		h.logf(format, args...)
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// stateDir is $GIT_DIR/kura/<remote>, or a hash of the URL for anonymous
// remotes (e.g. `git push kura::... main`).
func (h *helper) stateDir() string {
	name := h.name
	if strings.Contains(name, "://") || strings.Contains(name, "::") {
		sum := sha256.Sum256([]byte(h.target.StoreURL))
		name = "url-" + hex.EncodeToString(sum[:8])
	} else {
		name = unsafeName.ReplaceAllString(name, "_")
	}
	return filepath.Join(h.git.CommonDir, "kura", name)
}

// open connects to the remote on first use, verifying the key fingerprint
// against the URL and remote.<name>.kurafp.
func (h *helper) open(ctx context.Context) (*remote.Remote, error) {
	if h.rem != nil {
		return h.rem, nil
	}
	cache, err := keyring.DefaultCache()
	if err != nil {
		return nil, err
	}
	var pinned string
	if h.git.Dir != "" {
		if pinned, err = h.git.ConfigGet("remote." + h.name + ".kurafp"); err != nil {
			return nil, err
		}
	}
	r, err := repo.Open(ctx, h.target, cache, pinned)
	if err != nil {
		return nil, err
	}
	r.Logf = func(format string, args ...any) { h.logf(format, args...) }
	if h.git.Dir != "" {
		st, err := remote.OpenState(h.stateDir())
		if err != nil {
			return nil, err
		}
		r.State = st
	}
	h.rem = r
	return r, nil
}

func (h *helper) latest(ctx context.Context) (*remote.Snapshot, error) {
	r, err := h.open(ctx)
	if err != nil {
		return nil, err
	}
	s, err := r.Latest(ctx)
	if err != nil {
		return nil, err
	}
	h.snap = s
	return s, nil
}

func (h *helper) run(ctx context.Context) error {
	for {
		line, err := h.readLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case line == "":
			return nil
		case line == "capabilities":
			h.write("option\nfetch\npush\nobject-format\n\n")
		case line == "list" || line == "list for-push":
			err = h.list(ctx)
		case strings.HasPrefix(line, "option "):
			h.write(h.option(strings.TrimPrefix(line, "option ")) + "\n")
		case strings.HasPrefix(line, "fetch "):
			var batch []string
			if batch, err = h.batch(line); err == nil {
				err = h.fetch(ctx, batch)
			}
		case strings.HasPrefix(line, "push "):
			var batch []string
			if batch, err = h.batch(line); err == nil {
				err = h.push(ctx, batch)
			}
		default:
			return fmt.Errorf("unsupported helper command %q", line)
		}
		if err != nil {
			return err
		}
		if err := h.out.Flush(); err != nil {
			return err
		}
	}
}

func (h *helper) readLine() (string, error) {
	line, err := h.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// batch collects the first line and following lines up to a blank line.
func (h *helper) batch(first string) ([]string, error) {
	lines := []string{first}
	for {
		line, err := h.readLine()
		if err != nil {
			return nil, err
		}
		if line == "" {
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func (h *helper) write(s string) { h.out.WriteString(s) }

func (h *helper) list(ctx context.Context) error {
	s, err := h.latest(ctx)
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(s.Refs))
	for ref := range s.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	if h.opts.objectFormat {
		h.write(":object-format " + s.Format() + "\n")
	}
	for _, ref := range refs {
		h.write(s.Refs[ref] + " " + ref + "\n")
	}
	if _, ok := s.Refs[s.Head]; ok && s.Head != "" {
		h.write("@" + s.Head + " HEAD\n")
	}
	h.write("\n")
	return nil
}

func (h *helper) option(arg string) string {
	name, value, _ := strings.Cut(arg, " ")
	switch name {
	case "verbosity":
		n, err := strconv.Atoi(value)
		if err != nil {
			return "error bad verbosity"
		}
		h.opts.verbosity = n
	case "progress":
		h.opts.progress = value == "true"
	case "dry-run":
		h.opts.dryRun = value == "true"
	case "atomic":
		h.opts.atomic = value == "true"
	case "object-format":
		if value != "true" {
			return "unsupported"
		}
		h.opts.objectFormat = true
	case "cas":
		if strings.HasPrefix(value, `"`) {
			uq, err := strconv.Unquote(value)
			if err != nil {
				return "error bad cas value"
			}
			value = uq
		}
		ref, expect, ok := strings.Cut(value, ":")
		if !ok {
			return "unsupported" // lease without explicit value
		}
		if strings.Trim(expect, "0") == "" {
			expect = ""
		}
		h.opts.cas[ref] = expect
	default:
		return "unsupported"
	}
	return "ok"
}
