package remote

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/digital-shokunin/kura/internal/gitcmd"
)

// CompactResult describes a compaction.
type CompactResult struct {
	PacksBefore int
	BytesBefore int64
	Pack        Pack
}

// Compact rewrites every object reachable from the remote's refs into one
// pack and commits a manifest that references only it. Superseded packs stay
// in storage until GC removes them, so in-flight readers keep working.
func (r *Remote) Compact(ctx context.Context) (*CompactResult, error) {
	tmp, err := os.MkdirTemp("", "kura-compact-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	g := &gitcmd.Git{Dir: tmp}
	indexed := map[string]bool{}

	for attempt := 1; ; attempt++ {
		cur, err := r.Latest(ctx)
		if err != nil {
			return nil, err
		}
		if len(cur.Packs) <= 1 {
			return nil, nil
		}
		if attempt == 1 {
			if out, err := exec.Command("git", "init", "--bare", "-q", "--object-format="+cur.Format(), tmp).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git init: %v: %s", err, out)
			}
		}
		res := &CompactResult{PacksBefore: len(cur.Packs)}
		for _, p := range cur.Packs {
			res.BytesBefore += p.Size
			if indexed[p.ID] {
				continue
			}
			r.logf("kura: reading pack %s (%d bytes)", p.ID, p.Size)
			rc := r.OpenPack(ctx, p)
			keep, err := g.IndexPack(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			os.Remove(keep)
			indexed[p.ID] = true
		}
		var tips []string
		seen := map[string]bool{}
		for _, sha := range cur.Refs {
			if !seen[sha] {
				seen[sha] = true
				tips = append(tips, sha)
			}
		}
		sort.Strings(tips)
		if !g.ObjectsConnected(tips) {
			return nil, errors.New("remote packs do not contain every object reachable from its refs")
		}
		f, err := os.CreateTemp("", "kura-compact-*.pack")
		if err != nil {
			return nil, err
		}
		_, err = g.PackObjects(f, tips, nil)
		size, serr := f.Seek(0, 1)
		if err == nil {
			err = serr
		}
		if err == nil {
			_, err = f.Seek(0, 0)
		}
		if err == nil {
			res.Pack, err = r.UploadPack(ctx, f, size)
		}
		f.Close()
		os.Remove(f.Name())
		if err != nil {
			return nil, err
		}
		next := cur.Clone()
		next.Packs = []Pack{res.Pack}
		next.Base = maps.Clone(cur.Refs)
		_, err = r.Commit(ctx, cur, next)
		if (errors.Is(err, ErrConflict) || errors.Is(err, ErrStale)) && attempt < 5 {
			r.DeletePack(ctx, res.Pack) // definitely unreferenced
			continue
		}
		if err != nil {
			return nil, err // leave the pack for gc: it may be referenced
		}
		return res, nil
	}
}

// MergeThreshold is the pack count above which a push merges the
// incremental packs (all but the first) into one.
const MergeThreshold = 24

// MergeIncremental replaces Packs[1:] with a single pack built from the
// pusher's local repository: every object reachable from the current refs but
// not from Base, which Packs[0] already contains. Only the small incremental
// history is re-uploaded. It returns nil if there was nothing to do or the
// local repository lacks a ref tip; a conflict just defers the merge.
func (r *Remote) MergeIncremental(ctx context.Context, g *gitcmd.Git, cur *Snapshot) (*Snapshot, error) {
	if len(cur.Packs) <= MergeThreshold || cur.Base == nil {
		return nil, nil
	}
	var tips, exclude []string
	seen := map[string]bool{}
	for _, sha := range cur.Refs {
		if !g.HasObject(sha) {
			return nil, nil
		}
		if !seen[sha] {
			seen[sha] = true
			tips = append(tips, sha)
		}
	}
	for _, sha := range cur.Base {
		// Excluding less only makes the merged pack larger, never wrong.
		if !seen[sha] && g.HasObject(sha) {
			seen[sha] = true
			exclude = append(exclude, sha)
		}
	}
	sort.Strings(tips)
	sort.Strings(exclude)
	f, err := os.CreateTemp("", "kura-merge-*.pack")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := g.PackObjects(f, tips, exclude); err != nil {
		return nil, err
	}
	size, err := f.Seek(0, 1)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	merged, err := r.UploadPack(ctx, f, size)
	if err != nil {
		return nil, err
	}
	next := cur.Clone()
	next.Packs = []Pack{cur.Packs[0], merged}
	s, err := r.Commit(ctx, cur, next)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrStale) {
		r.DeletePack(ctx, merged)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.logf("kura: merged %d incremental packs into one (%s)", len(cur.Packs)-1, HumanBytes(size))
	return s, nil
}

// GCResult describes a garbage collection.
type GCResult struct {
	Referenced int
	Deleted    []string
	Kept       []string // unreferenced but younger than the grace period
}

// GC deletes stored packs that no manifest written within grace references
// and whose newest part is older than grace. An upload in progress keeps
// adding parts, and Commit refuses a pack whose last part is older than
// UploadDeadline (< grace), so gc never deletes a pack a commit can still
// reference. Readers holding a manifest from within the grace window still
// find every pack it names.
func (r *Remote) GC(ctx context.Context, grace time.Duration, dryRun bool) (*GCResult, error) {
	if grace < 2*UploadDeadline {
		return nil, fmt.Errorf("grace period must be at least %s", 2*UploadDeadline)
	}
	cur, err := r.Latest(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-grace)
	referenced := map[string]bool{}
	for s := cur; ; {
		for _, p := range s.Packs {
			referenced[p.ID] = true
		}
		// Keep the newest manifest older than the cutoff too: it was current
		// at the cutoff, so readers may still be using it. Its time comes from
		// the writer's clock, so the store's timestamp must agree that it is
		// old: a writer with a slow clock cannot shorten the walk.
		if s.Seq == 0 {
			break
		}
		if s.Time.Before(cutoff) {
			info, err := r.Store.Head(ctx, manifestKey(s.Seq))
			if err != nil {
				return nil, err
			}
			if info.ModTime.Before(cutoff) {
				break
			}
		}
		if s, err = r.readManifest(ctx, s.Seq-1); err != nil {
			return nil, fmt.Errorf("walking manifests: %w", err)
		}
	}
	keys, err := r.Store.List(ctx, "packs/")
	if err != nil {
		return nil, err
	}
	byPack := map[string][]string{}
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) == 3 {
			byPack[parts[1]] = append(byPack[parts[1]], k)
		}
	}
	res := &GCResult{Referenced: len(referenced)}
	ids := make([]string, 0, len(byPack))
	for id := range byPack {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if referenced[id] {
			continue
		}
		var newest time.Time
		for _, k := range byPack[id] {
			info, err := r.Store.Head(ctx, k)
			if err != nil {
				return nil, err
			}
			if info.ModTime.After(newest) {
				newest = info.ModTime
			}
		}
		if newest.After(cutoff) {
			res.Kept = append(res.Kept, id)
			continue
		}
		if !dryRun {
			for _, k := range byPack[id] {
				if err := r.Store.Delete(ctx, k); err != nil {
					return nil, err
				}
			}
		}
		res.Deleted = append(res.Deleted, id)
	}
	return res, nil
}
