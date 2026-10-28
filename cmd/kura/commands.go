package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"golang.org/x/term"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/keyring"
	"github.com/digital-shokunin/kura/internal/remote"
	"github.com/digital-shokunin/kura/internal/repo"
	"github.com/digital-shokunin/kura/internal/store"
	"github.com/digital-shokunin/kura/internal/store/storetest"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// parse splits leading positional arguments from flags, so that both
// "kura init URL --age R" and "kura init --age R URL" work.
func parse(fs *flag.FlagSet, args []string, positional int) ([]string, error) {
	var pos []string
	for len(args) > 0 && len(pos) < positional && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != positional {
		return nil, fmt.Errorf("expected %d argument(s), got %d", positional, len(pos))
	}
	return pos, nil
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("kura "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func readRecipientFiles(files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				out = append(out, line)
			}
		}
	}
	return out, nil
}

// wrapKeys wraps k to the recipients, returning the key files to store keyed
// by remote path. A nil value means the file should be deleted. The
// recipient list itself lives in the authenticated manifest.
func wrapKeys(k crypt.Key, rc remote.Recipients) (map[string][]byte, error) {
	if rc.Empty() {
		return nil, errors.New("at least one --age or --gpg recipient is required")
	}
	objs := map[string][]byte{keyring.AgeKeyPath: nil, keyring.GPGKeyPath: nil}
	var err error
	if len(rc.Age) > 0 {
		if objs[keyring.AgeKeyPath], err = keyring.WrapAge(k, rc.Age); err != nil {
			return nil, err
		}
	}
	if len(rc.GPG) > 0 {
		if objs[keyring.GPGKeyPath], err = keyring.WrapGPG(k, rc.GPG); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

func storeKeys(ctx context.Context, s store.Store, objs map[string][]byte) error {
	for key, data := range objs {
		var err error
		if data == nil {
			err = s.Delete(ctx, key)
		} else {
			err = s.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func cmdInit(args []string) error {
	fs := newFlags("init")
	var ages, ageFiles, gpgs multiFlag
	fs.Var(&ages, "age", "age recipient (age1..., age1se1..., ...); repeatable")
	fs.Var(&ageFiles, "age-file", "file of age recipients, one per line; repeatable")
	fs.Var(&gpgs, "gpg", "gpg key fingerprint to wrap the key to; repeatable")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	fromFiles, err := readRecipientFiles(ageFiles)
	if err != nil {
		return err
	}
	rc := remote.Recipients{Age: append(ages, fromFiles...), GPG: gpgs}
	t, err := repo.ParseURL(pos[0])
	if err != nil {
		return err
	}
	ctx := context.Background()
	s, err := store.Open(ctx, t.StoreURL)
	if err != nil {
		return err
	}
	if _, err := remote.LoadConfig(ctx, s); err == nil {
		return errors.New("a kura repository already exists at this location")
	}
	k, err := crypt.NewKey()
	if err != nil {
		return err
	}
	repoID := remote.NewRepoID()
	cache, err := keyring.DefaultCache()
	if err != nil {
		return err
	}
	// Wrap first (may run plugins or gpg) so a failure leaves nothing behind.
	objs, err := wrapKeys(k, rc)
	if err != nil {
		return err
	}
	if _, err := remote.Init(ctx, s, k, repoID, rc); err != nil {
		return err
	}
	// Cache K before writing key files, so a failed write can be repaired
	// with `kura recipients ... add` instead of losing the repository.
	if err := cache.Set(repoID, k); err != nil {
		return fmt.Errorf("repository created but caching its key in %s failed: %w", cache.Name(), err)
	}
	if err := storeKeys(ctx, s, objs); err != nil {
		return fmt.Errorf("repository created and key cached, but writing key files failed "+
			"(re-run `kura recipients URL add ...` to repair): %w", err)
	}
	t.Fingerprint = k.Fingerprint(repoID)
	fmt.Printf(`Created kura repository %s
Key fingerprint: %s   <- write this on your recovery sheet
Key cached in:   %s
Wrapped to:      %d age recipient(s), %d gpg key(s)

Add it to a repository with:
  git remote add origin '%s'
  git config remote.origin.kurafp %s
`, repoID, t.Fingerprint, cache.Name(), len(rc.Age), len(rc.GPG), t.URL(), t.Fingerprint)
	return nil
}

func defaultIdentityFile() string {
	dir, err := keyring.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "identities")
}

func cmdUnlock(args []string) error {
	fs := newFlags("unlock")
	var idFiles multiFlag
	fs.Var(&idFiles, "i", "age identity file (native or plugin identities); repeatable")
	useGPG := fs.Bool("gpg", false, "unwrap with gpg (e.g. a Yubikey OpenPGP key)")
	var gpgKeys multiFlag
	fs.Var(&gpgKeys, "gpg-key", "with --gpg: the secret (sub)key to try, so gpg asks for the right smartcard; repeatable")
	fp := fs.String("fp", "", "expected key fingerprint, if not in the url")
	force := fs.Bool("force", false, "unwrap again even if the key is already cached")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	t, err := repo.ParseURL(pos[0])
	if err != nil {
		return err
	}
	ctx := context.Background()
	s, cfg, err := repo.OpenStore(ctx, t)
	if err != nil {
		return err
	}
	cache, err := keyring.DefaultCache()
	if err != nil {
		return err
	}
	if k, err := cache.Get(cfg.RepoID); err == nil && !*force {
		if err := repo.CheckFingerprint(k, cfg.RepoID, t.Fingerprint, *fp); err != nil {
			return fmt.Errorf("cached key: %w", err)
		}
		fmt.Printf("Already unlocked (%s). Use --force to unwrap again.\n", cache.Name())
		return nil
	}
	if len(gpgKeys) > 0 {
		*useGPG = true
	}
	if len(idFiles) == 0 && !*useGPG {
		if def := defaultIdentityFile(); def != "" {
			if _, err := os.Stat(def); err == nil {
				idFiles = append(idFiles, def)
			}
		}
		if len(idFiles) == 0 {
			*useGPG = true
		}
	}
	var k crypt.Key
	if len(idFiles) > 0 {
		var ids []age.Identity
		for _, f := range idFiles {
			got, err := keyring.LoadIdentities(f)
			if err != nil {
				return err
			}
			ids = append(ids, got...)
		}
		ct, err := readObject(ctx, s, keyring.AgeKeyPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", keyring.AgeKeyPath, err)
		}
		if k, err = keyring.UnwrapAge(ct, ids); err != nil {
			return err
		}
	} else {
		ct, err := readObject(ctx, s, keyring.GPGKeyPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", keyring.GPGKeyPath, err)
		}
		fmt.Fprintln(os.Stderr, "kura: decrypting with gpg (your Yubikey may ask for its PIN and a touch)")
		if k, err = keyring.UnwrapGPG(ct, gpgKeys...); err != nil {
			return err
		}
	}
	got := k.Fingerprint(cfg.RepoID)
	if t.Fingerprint == "" && *fp == "" {
		fmt.Fprintf(os.Stderr, "kura: WARNING: no fingerprint pinned; check %s against your recovery sheet\n", got)
	}
	if err := repo.CheckFingerprint(k, cfg.RepoID, t.Fingerprint, *fp); err != nil {
		return err
	}
	// The key must also authenticate the repository's existing manifests.
	r := &remote.Remote{Store: s, RepoID: cfg.RepoID, Key: k}
	latest, err := r.Latest(ctx)
	if err != nil {
		return fmt.Errorf("unwrapped key does not open this repository: %w", err)
	}
	if err := cache.Set(cfg.RepoID, k); err != nil {
		return fmt.Errorf("caching key in %s: %w", cache.Name(), err)
	}
	fmt.Printf("Unlocked repository %s (fingerprint %s, %d refs at seq %d) into %s\n",
		cfg.RepoID, got, len(latest.Refs), latest.Seq, cache.Name())
	return nil
}

func cmdLock(args []string) error {
	pos, err := parse(newFlags("lock"), args, 1)
	if err != nil {
		return err
	}
	t, err := repo.ParseURL(pos[0])
	if err != nil {
		return err
	}
	_, cfg, err := repo.OpenStore(context.Background(), t)
	if err != nil {
		return err
	}
	cache, err := keyring.DefaultCache()
	if err != nil {
		return err
	}
	if err := cache.Delete(cfg.RepoID); err != nil {
		return err
	}
	fmt.Printf("Removed key for %s from %s\n", cfg.RepoID, cache.Name())
	return nil
}

// openRemote opens a remote with the cached key, without local state.
func openRemote(ctx context.Context, rawURL string) (*remote.Remote, repo.Target, error) {
	t, err := repo.ParseURL(rawURL)
	if err != nil {
		return nil, t, err
	}
	cache, err := keyring.DefaultCache()
	if err != nil {
		return nil, t, err
	}
	r, err := repo.Open(ctx, t, cache)
	if err != nil {
		return nil, t, err
	}
	r.Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	return r, t, nil
}

func cmdFingerprint(args []string) error {
	pos, err := parse(newFlags("fingerprint"), args, 1)
	if err != nil {
		return err
	}
	r, _, err := openRemote(context.Background(), pos[0])
	if err != nil {
		return err
	}
	fmt.Println(r.Key.Fingerprint(r.RepoID))
	return nil
}

func cmdRecipients(args []string) error {
	fs := newFlags("recipients")
	var ages, gpgs multiFlag
	fs.Var(&ages, "age", "age recipient; repeatable")
	fs.Var(&gpgs, "gpg", "gpg key fingerprint; repeatable")
	pos, err := parse(fs, args, 2)
	if err != nil {
		return err
	}
	action := pos[1]
	if action != "list" && action != "add" && action != "remove" {
		return fmt.Errorf("unknown recipients action %q (list, add, remove)", action)
	}
	ctx := context.Background()
	r, done, err := openRemoteWithState(ctx, pos[0])
	if err != nil {
		return err
	}
	defer done()
	for attempt := 1; ; attempt++ {
		cur, err := r.Latest(ctx)
		if err != nil {
			return err
		}
		next := cur.Clone()
		rc := &next.Recipients
		switch action {
		case "list":
			printRecipients(*rc)
			return nil
		case "add":
			for _, a := range ages {
				if !slices.Contains(rc.Age, a) {
					rc.Age = append(rc.Age, a)
				}
			}
			for _, g := range gpgs {
				if !slices.Contains(rc.GPG, g) {
					rc.GPG = append(rc.GPG, g)
				}
			}
		case "remove":
			rc.Age = slices.DeleteFunc(rc.Age, func(a string) bool { return slices.Contains(ages, a) })
			rc.GPG = slices.DeleteFunc(rc.GPG, func(g string) bool { return slices.Contains(gpgs, g) })
		}
		objs, err := wrapKeys(r.Key, *rc)
		if err != nil {
			return err
		}
		if err := storeKeys(ctx, r.Store, objs); err != nil {
			return err
		}
		_, err = r.Commit(ctx, cur, next)
		if errors.Is(err, remote.ErrConflict) && attempt < 5 {
			continue
		}
		if err != nil {
			return err
		}
		if action == "remove" {
			fmt.Fprintln(os.Stderr, "kura: note: a removed recipient that already unwrapped the key keeps it; "+
				"re-keying (rotate) is required to cut off a compromised key")
		}
		fmt.Println("Key is now wrapped to:")
		printRecipients(*rc)
		return nil
	}
}

func printRecipients(rc remote.Recipients) {
	for _, a := range rc.Age {
		fmt.Println("  age", a)
	}
	for _, g := range rc.GPG {
		fmt.Println("  gpg", g)
	}
}

func cmdProbe(args []string) error {
	pos, err := parse(newFlags("probe"), args, 1)
	if err != nil {
		return err
	}
	t, err := repo.ParseURL(pos[0])
	if err != nil {
		return err
	}
	ctx := context.Background()
	s, err := store.Open(ctx, t.StoreURL)
	if err != nil {
		return err
	}
	b := make([]byte, 6)
	rand.Read(b)
	prefix := "kura-probe-" + hex.EncodeToString(b) + "/"
	fmt.Printf("Probing %s (scratch prefix %s)\n", t.StoreURL, prefix)
	failed := storetest.Run(ctx, s, prefix, func(name string, err error) {
		if err != nil {
			fmt.Printf("  FAIL  %s: %v\n", name, err)
		} else {
			fmt.Printf("  ok    %s\n", name)
		}
	})
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed; this backend is not safe for kura", failed)
	}
	fmt.Println("All checks passed.")
	return nil
}

func cmdS3Creds(args []string) error {
	fs := newFlags("s3-creds")
	fromStdin := fs.Bool("stdin", false, "read the key from stdin: `aws iam create-access-key` JSON, or two lines (id, secret)")
	pos, err := parse(fs, args, 2)
	if err != nil {
		return err
	}
	if pos[0] != "set" {
		return fmt.Errorf("unknown action %q (set)", pos[0])
	}
	var id, secret string
	if *fromStdin {
		id, secret, err = readKeyFromStdin()
	} else {
		id, secret, err = readKeyFromTTY()
	}
	if err != nil {
		return err
	}
	if id == "" || secret == "" {
		return errors.New("empty access key id or secret")
	}
	if err := keyring.SaveS3Creds(pos[1], id, secret); err != nil {
		return err
	}
	fmt.Printf("Stored access key %s. Use it with &creds=%s in the s3:// url.\n", id, pos[1])
	return nil
}

func readKeyFromStdin() (string, string, error) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
	if err != nil {
		return "", "", err
	}
	var j struct {
		AccessKey struct{ AccessKeyId, SecretAccessKey string }
	}
	if json.Unmarshal(b, &j) == nil && j.AccessKey.AccessKeyId != "" {
		return j.AccessKey.AccessKeyId, j.AccessKey.SecretAccessKey, nil
	}
	lines := strings.Fields(string(b))
	if len(lines) != 2 {
		return "", "", errors.New("expected create-access-key JSON or two lines: id, secret")
	}
	return lines[0], lines[1], nil
}

func readKeyFromTTY() (string, string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", "", errors.New("needs an interactive terminal (or use --stdin)")
	}
	defer tty.Close()
	fmt.Fprint(tty, "Access key ID: ")
	id, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return "", "", err
	}
	fmt.Fprint(tty, "Secret access key (hidden): ")
	secret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(id), strings.TrimSpace(string(secret)), nil
}

func readObject(ctx context.Context, s store.Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 1<<20))
}
