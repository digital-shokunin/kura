package helper

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/digital-shokunin/kura/internal/gitcmd"
	"github.com/digital-shokunin/kura/internal/remote"
)

const maxPushAttempts = 10

type update struct {
	src, dst string
	force    bool
	newSHA   string // "" for deletes
	err      string // rejection reason reported to git
}

// builtPack is an uploaded pack plus what it was built against.
type builtPack struct {
	pack    *remote.Pack // nil when there were no new objects
	against []string     // pack ids in the manifest it was built against
}

// push handles a batch of "push [+]<src>:<dst>" lines and commits all
// accepted updates in a single manifest, retrying on concurrent writers.
func (h *helper) push(ctx context.Context, batch []string) error {
	if err := h.git.RequireRepo(); err != nil {
		return err
	}
	f, err := h.git.ObjectFormat()
	if err != nil {
		return err
	}
	h.objectFormat = f
	var ups []*update
	for _, line := range batch {
		spec := strings.TrimPrefix(line, "push ")
		u := &update{}
		if strings.HasPrefix(spec, "+") {
			u.force, spec = true, spec[1:]
		}
		var ok bool
		if u.src, u.dst, ok = strings.Cut(spec, ":"); !ok || u.dst == "" {
			return fmt.Errorf("malformed push line %q", line)
		}
		if u.src != "" {
			sha, err := h.git.ResolveObject(u.src)
			if err != nil {
				u.err = "cannot resolve source"
			}
			u.newSHA = sha
		}
		ups = append(ups, u)
	}

	s := h.snap
	if s == nil {
		var err error
		if s, err = h.latest(ctx); err != nil {
			return err
		}
	}
	r := h.rem
	// An uploaded pack is never deleted here: if a commit's outcome was
	// ambiguous it may be referenced. Orphans are removed by `kura gc`.
	var bp *builtPack
	discard := func() { bp = nil }

	for attempt := 1; ; attempt++ {
		// The object format is fixed once anything is stored, including
		// packs left behind after every ref was deleted.
		empty := len(s.Refs) == 0 && len(s.Packs) == 0
		if !empty && s.Format() != h.objectFormat {
			return fmt.Errorf("local repository uses %s object ids but the remote uses %s", h.objectFormat, s.Format())
		}
		var accepted []*update
		for _, u := range ups {
			if u.err == "cannot resolve source" {
				continue
			}
			u.err = h.check(u, s)
			if u.err == "" {
				accepted = append(accepted, u)
			}
		}
		if h.opts.atomic && len(accepted) != len(ups) {
			for _, u := range accepted {
				u.err = "atomic push failed"
			}
			accepted = nil
		}
		if len(accepted) == 0 || h.opts.dryRun {
			discard()
			break
		}

		if bp != nil && (compacted(bp.against, s) || bp.stale()) {
			discard()
		}
		if bp == nil {
			var err error
			if bp, err = h.buildPack(ctx, accepted, s); err != nil {
				return err
			}
		}

		next := s.Clone()
		for _, u := range accepted {
			if u.src == "" {
				delete(next.Refs, u.dst)
			} else {
				next.Refs[u.dst] = u.newSHA
			}
		}
		if bp.pack != nil {
			next.Packs = append(next.Packs, *bp.pack)
			if next.Base == nil && len(next.Packs) == 1 {
				// The first pack holds everything reachable from these refs.
				next.Base = maps.Clone(next.Refs)
			}
		}
		if empty {
			next.ObjectFormat = h.objectFormat
		}
		h.chooseHead(next)

		committed, err := r.Commit(ctx, s, next)
		if errors.Is(err, remote.ErrStale) && attempt < maxPushAttempts {
			// The machine slept between upload and commit; gc may have
			// deleted the pack, so upload a new one.
			discard()
			continue
		}
		if errors.Is(err, remote.ErrConflict) && attempt < maxPushAttempts {
			h.progressf("kura: remote changed during push, retrying (%d)", attempt)
			time.Sleep(time.Duration(attempt*attempt) * 50 * time.Millisecond)
			if s, err = h.latest(ctx); err != nil {
				return err
			}
			if landed(bp, accepted, s) {
				// Our earlier attempt (or an identical update) already won.
				committed, err = s, nil
			} else {
				continue
			}
		}
		if err != nil {
			return err
		}
		h.snap = committed
		if bp.pack != nil {
			if err := r.State.MarkApplied(bp.pack.ID); err != nil {
				return err
			}
		}
		if merged, err := r.MergeIncremental(ctx, h.git, committed); err != nil {
			h.logf("kura: warning: merging incremental packs failed (push succeeded): %v", err)
		} else if merged != nil {
			h.snap = merged
		}
		break
	}

	for _, u := range ups {
		if u.err == "" {
			h.write("ok " + u.dst + "\n")
		} else {
			h.write("error " + u.dst + " " + u.err + "\n")
		}
	}
	h.write("\n")
	return nil
}

// check validates one update against the current manifest, returning the
// rejection reason git understands, or "".
func (h *helper) check(u *update, s *remote.Snapshot) string {
	if !remote.ValidRefName(u.dst) {
		return "invalid ref name"
	}
	old, exists := s.Refs[u.dst]
	if lease, ok := h.opts.cas[u.dst]; ok {
		if (lease == "" && exists) || (lease != "" && lease != old) {
			return "stale info"
		}
		return ""
	}
	if u.src == "" || !exists || old == u.newSHA || u.force {
		return ""
	}
	if strings.HasPrefix(u.dst, "refs/tags/") {
		return "already exists"
	}
	switch a, err := h.git.CheckAncestor(old, u.newSHA); {
	case err != nil:
		return "non-fast forward"
	case a == gitcmd.UnknownObject:
		return "fetch first"
	case a == gitcmd.NotAncestor:
		return "non-fast forward"
	}
	return ""
}

// stale reports whether the uploaded pack is too old to commit.
func (bp *builtPack) stale() bool {
	return bp.pack != nil && bp.pack.Stale()
}

// landed reports whether s already contains our push: our pack (whose random
// id only we know), or every accepted ref update.
func landed(bp *builtPack, accepted []*update, s *remote.Snapshot) bool {
	if bp != nil && bp.pack != nil && s.HasPack(bp.pack.ID) {
		return true
	}
	for _, u := range accepted {
		got, exists := s.Refs[u.dst]
		if (u.src == "" && exists) || (u.src != "" && got != u.newSHA) {
			return false
		}
	}
	return len(accepted) > 0
}

// compacted reports whether any pack the upload was built against has been
// removed from the manifest, in which case the objects it excluded may no
// longer be stored and the pack must be rebuilt.
func compacted(against []string, s *remote.Snapshot) bool {
	for _, id := range against {
		if !s.HasPack(id) {
			return true
		}
	}
	return false
}

func (h *helper) buildPack(ctx context.Context, accepted []*update, s *remote.Snapshot) (*builtPack, error) {
	bp := &builtPack{}
	for _, p := range s.Packs {
		bp.against = append(bp.against, p.ID)
	}
	var include, exclude []string
	for _, u := range accepted {
		if u.newSHA != "" {
			include = append(include, u.newSHA)
		}
	}
	if len(include) == 0 {
		return bp, nil
	}
	seen := map[string]bool{}
	for _, sha := range s.Refs {
		if !seen[sha] && h.git.HasObject(sha) {
			seen[sha] = true
			exclude = append(exclude, sha)
		}
	}
	sort.Strings(exclude)
	// A remote server would catch this with a connectivity check; kura has
	// no server, so refuse to upload commits whose real parents are unknown.
	if cut, err := h.git.CrossesGraft(include, exclude); err != nil {
		return nil, err
	} else if cut {
		return nil, errors.New("kura: this push reaches a shallow boundary or grafted commit, " +
			"so the remote could be left without its parents; run `git fetch --unshallow` " +
			"(or remove .git/info/grafts) and push again")
	}

	tmp, err := os.CreateTemp("", "kura-push-*.pack")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	count, err := h.git.PackObjects(tmp, include, exclude)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return bp, nil
	}
	size, err := tmp.Seek(0, 1)
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return nil, err
	}
	h.progressf("kura: uploading %d objects (%s)", count, remote.HumanBytes(size))
	p, err := h.rem.UploadPack(ctx, tmp, size)
	if err != nil {
		return nil, err
	}
	bp.pack = &p
	return bp, nil
}

// chooseHead keeps the remote HEAD pointing at an existing branch, preferring
// the pusher's current branch when the remote has none yet.
func (h *helper) chooseHead(m *remote.Manifest) {
	if _, ok := m.Refs[m.Head]; ok && m.Head != "" {
		return
	}
	m.Head = ""
	candidates := []string{h.git.SymbolicHead(), "refs/heads/main", "refs/heads/master"}
	for _, c := range candidates {
		if _, ok := m.Refs[c]; ok && c != "" {
			m.Head = c
			return
		}
	}
	var branches []string
	for ref := range m.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, ref)
		}
	}
	sort.Strings(branches)
	if len(branches) > 0 {
		m.Head = branches[0]
	}
}
