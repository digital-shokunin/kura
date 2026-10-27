// Command kura manages end-to-end encrypted git remotes. The same binary,
// invoked as git-remote-kura (a symlink), is the git remote helper.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/digital-shokunin/kura/internal/helper"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `kura: end-to-end encrypted git remotes

Usage:
  kura init <url> [--age R]... [--age-file F]... [--gpg FPR]...
  kura unlock <url> [-i IDENTITY_FILE]... [--gpg] [--fp FINGERPRINT]
  kura lock <url>
  kura fingerprint <url>
  kura info <url>
  kura recipients <url> list
  kura recipients <url> add|remove [--age R]... [--gpg FPR]...
  kura verify <url> [--repo DIR]
  kura compact <url>
  kura gc <url> [--grace 24h] [--dry-run]
  kura probe <store-url>
  kura s3-creds set <name> [--stdin]

URLs look like kura::s3://bucket/prefix?region=us-east-2&fp=... or file:///path.
Git uses them via: git remote add origin 'kura::s3://...?fp=...'
`

func main() {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "git-remote-kura") {
		os.Exit(helper.Main(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"init":        cmdInit,
		"unlock":      cmdUnlock,
		"lock":        cmdLock,
		"fingerprint": cmdFingerprint,
		"info":        cmdInfo,
		"recipients":  cmdRecipients,
		"verify":      cmdVerify,
		"compact":     cmdCompact,
		"gc":          cmdGC,
		"probe":       cmdProbe,
		"s3-creds":    cmdS3Creds,
		"remote-helper": func(args []string) error {
			if code := helper.Main(args); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	name := os.Args[1]
	if name == "version" || name == "--version" {
		fmt.Println("kura", version)
		return
	}
	if name == "-h" || name == "--help" || name == "help" {
		fmt.Print(usage)
		return
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "kura: unknown command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "kura %s: %v\n", name, err)
		os.Exit(1)
	}
}
