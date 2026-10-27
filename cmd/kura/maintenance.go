package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/remote"
)

// cliState opens the CLI's own high-water mark for a repo, so maintenance
// commands also refuse rollback.
func cliState(repoID string) (*remote.State, error) {
	dir, err := keyring.ConfigDir()
	if err != nil {
		return nil, err
	}
	return remote.OpenState(filepath.Join(dir, "state", repoID))
}

func openRemoteWithState(ctx context.Context, rawURL string) (*remote.Remote, func(), error) {
	r, _, err := openRemote(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}
	st, err := cliState(r.RepoID)
	if err != nil {
		return nil, nil, err
	}
	r.State = st
	return r, func() { st.Close() }, nil
}

func cmdInfo(args []string) error {
	pos, err := parse(newFlags("info"), args, 1)
	if err != nil {
		return err
	}
	ctx := context.Background()
	r, done, err := openRemoteWithState(ctx, pos[0])
	if err != nil {
		return err
	}
	defer done()
	s, err := r.Latest(ctx)
	if err != nil {
		return err
	}
	var total int64
	for _, p := range s.Packs {
		total += p.Size
	}
	fmt.Printf(`Repository:    %s
Fingerprint:   %s
Object format: %s
Manifest:      seq %d, written by %q at %s
Refs:          %d (HEAD -> %s)
Packs:         %d (%s)
Recipients:    %d age, %d gpg
`, r.RepoID, r.Key.Fingerprint(r.RepoID), s.Format(), s.Seq, s.Writer, s.Time.Format(time.RFC3339),
		len(s.Refs), s.Head, len(s.Packs), remote.HumanBytes(total), len(s.Recipients.Age), len(s.Recipients.GPG))
	return nil
}

func cmdCompact(args []string) error {
	pos, err := parse(newFlags("compact"), args, 1)
	if err != nil {
		return err
	}
	ctx := context.Background()
	r, done, err := openRemoteWithState(ctx, pos[0])
	if err != nil {
		return err
	}
	defer done()
	res, err := r.Compact(ctx)
	if err != nil {
		return err
	}
	if res == nil {
		fmt.Println("Nothing to compact (one pack or fewer).")
		return nil
	}
	fmt.Printf("Compacted %d packs (%s) into 1 (%s). Old packs remain until `kura gc`.\n",
		res.PacksBefore, remote.HumanBytes(res.BytesBefore), remote.HumanBytes(res.Pack.Size))
	return nil
}

func cmdGC(args []string) error {
	fs := newFlags("gc")
	grace := fs.Duration("grace", 24*time.Hour, "keep packs referenced by manifests newer than this")
	dry := fs.Bool("dry-run", false, "report what would be deleted")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	ctx := context.Background()
	r, done, err := openRemoteWithState(ctx, pos[0])
	if err != nil {
		return err
	}
	defer done()
	res, err := r.GC(ctx, *grace, *dry)
	if err != nil {
		return err
	}
	verb := "Deleted"
	if *dry {
		verb = "Would delete"
	}
	fmt.Printf("%d pack(s) referenced. %s %d unreferenced pack(s); kept %d younger than %s.\n",
		res.Referenced, verb, len(res.Deleted), len(res.Kept), *grace)
	return nil
}

func run(dir string, env []string, name string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

// helperEnv returns an environment whose PATH finds this binary as
// git-remote-kura, so verification exercises exactly this build.
func helperEnv(tmp string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return nil, err
	}
	if err := os.Symlink(exe, filepath.Join(bin, "git-remote-kura")); err != nil {
		return nil, err
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "GIT_DIR=") {
			env = append(env, kv)
		}
	}
	return append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")), nil
}

func cmdVerify(args []string) error {
	fs := newFlags("verify")
	local := fs.String("repo", "", "also compare refs and HEAD tree with this local repository")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	ctx := context.Background()
	r, done, err := openRemoteWithState(ctx, pos[0])
	if err != nil {
		return err
	}
	latest, err := r.Latest(ctx)
	done()
	if err != nil {
		return err
	}
	_, t, _ := openRemote(ctx, pos[0])
	tmp, err := os.MkdirTemp("", "kura-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	env, err := helperEnv(tmp)
	if err != nil {
		return err
	}
	mirror := filepath.Join(tmp, "mirror.git")
	fmt.Printf("Cloning %d refs from manifest seq %d...\n", len(latest.Refs), latest.Seq)
	if _, err := run(tmp, env, "git", "clone", "--mirror", "--quiet", t.URL(), mirror); err != nil {
		return err
	}
	if _, err := run(mirror, env, "git", "fsck", "--full", "--strict", "--no-dangling", "--no-progress"); err != nil {
		return err
	}
	fmt.Println("  ok    git fsck --full --strict")

	out, err := run(mirror, env, "git", "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return err
	}
	cloned := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, ref, ok := strings.Cut(line, " "); ok {
			cloned[ref] = sha
		}
	}
	var problems []string
	refs := make([]string, 0, len(latest.Refs))
	for ref := range latest.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if cloned[ref] != latest.Refs[ref] {
			problems = append(problems, fmt.Sprintf("clone has %s at %q, manifest says %s", ref, cloned[ref], latest.Refs[ref]))
		}
	}
	if len(problems) == 0 {
		fmt.Printf("  ok    cloned refs match manifest (%d)\n", len(refs))
	}
	if *local != "" {
		for _, ref := range refs {
			sha, err := run(*local, os.Environ(), "git", "rev-parse", "--verify", "--quiet", ref)
			if err != nil || strings.TrimSpace(sha) != latest.Refs[ref] {
				problems = append(problems, fmt.Sprintf("local %s is %q, remote is %s", ref, strings.TrimSpace(sha), latest.Refs[ref]))
			}
		}
		if latest.Head != "" {
			lt, err1 := run(*local, os.Environ(), "git", "rev-parse", "HEAD^{tree}")
			rt, err2 := run(mirror, env, "git", "rev-parse", latest.Head+"^{tree}")
			if err1 != nil || err2 != nil || strings.TrimSpace(lt) != strings.TrimSpace(rt) {
				problems = append(problems, fmt.Sprintf("local HEAD tree %s differs from remote %s tree %s",
					strings.TrimSpace(lt), latest.Head, strings.TrimSpace(rt)))
			} else {
				fmt.Printf("  ok    local HEAD tree matches remote %s (%s)\n", latest.Head, strings.TrimSpace(lt)[:12])
			}
		}
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("  FAIL ", p)
		}
		return fmt.Errorf("%d problem(s)", len(problems))
	}
	fmt.Println("Verified.")
	return nil
}
