package helper

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/digital-shokunin/kura/internal/remote"
)

// fetch handles a batch of "fetch <sha> <ref>" lines. Git only asks when it
// lacks objects, so we index every pack this clone has not applied yet, and
// fall back to all packs if objects are still missing (e.g. after a remote
// compaction, or a clone whose object store was pruned).
func (h *helper) fetch(ctx context.Context, batch []string) error {
	if err := h.git.RequireRepo(); err != nil {
		return err
	}
	var wants []string
	for _, line := range batch {
		f := strings.Fields(line)
		if len(f) < 3 {
			return fmt.Errorf("malformed fetch line %q", line)
		}
		wants = append(wants, f[1])
	}
	s := h.snap
	if s == nil {
		var err error
		if s, err = h.latest(ctx); err != nil {
			return err
		}
	}
	r := h.rem
	var keeps []string
	done := map[string]bool{}
	apply := func(p remote.Pack, i, n int) error {
		h.progressf("kura: fetching pack %d/%d (%s)", i, n, remote.HumanBytes(p.Size))
		rc := r.OpenPack(ctx, p)
		keep, err := h.git.IndexPack(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("pack %s: %w", p.ID, err)
		}
		done[p.ID] = true
		if keep != "" {
			keeps = append(keeps, keep)
		}
		return r.State.MarkApplied(p.ID)
	}

	if !h.git.ObjectsConnected(wants) {
		var todo []remote.Pack
		for _, p := range s.Packs {
			if !r.State.IsApplied(p.ID) {
				todo = append(todo, p)
			}
		}
		for i, p := range todo {
			if err := apply(p, i+1, len(todo)); err != nil {
				return err
			}
		}
	}
	if !h.git.ObjectsConnected(wants) {
		h.logf("kura: objects still missing after incremental fetch; reading every pack")
		var todo []remote.Pack
		for _, p := range s.Packs {
			if !done[p.ID] {
				todo = append(todo, p)
			}
		}
		for i, p := range todo {
			if err := apply(p, i+1, len(todo)); err != nil {
				return err
			}
		}
		if !h.git.ObjectsConnected(wants) {
			return fmt.Errorf("remote manifest %d references objects that no pack contains", s.Seq)
		}
	}
	if err := r.State.Prune(s.Manifest); err != nil {
		return err
	}
	// Git accepts one lock file; the extra .keep files only protect packs
	// for the moment until git updates refs, so drop them now.
	if len(keeps) > 0 {
		h.write("lock " + keeps[0] + "\n")
		for _, k := range keeps[1:] {
			os.Remove(k)
		}
	}
	h.write("\n")
	return nil
}
