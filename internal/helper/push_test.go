package helper

import (
	"testing"

	"github.com/digital-shokunin/kura/internal/remote"
)

func TestLanded(t *testing.T) {
	snap := &remote.Snapshot{Manifest: &remote.Manifest{
		Refs:  map[string]string{"refs/heads/main": "bbb"},
		Packs: []remote.Pack{{ID: "p1"}},
	}}
	ours := &builtPack{pack: &remote.Pack{ID: "p1"}}
	other := &builtPack{pack: &remote.Pack{ID: "p2"}}
	upd := []*update{{src: "main", dst: "refs/heads/main", newSHA: "ccc"}}
	if !landed(ours, upd, snap) {
		t.Error("our pack is in the manifest: should count as landed")
	}
	if landed(other, upd, snap) {
		t.Error("neither our pack nor our ref update is present")
	}
	same := []*update{{src: "main", dst: "refs/heads/main", newSHA: "bbb"}}
	if !landed(&builtPack{}, same, snap) {
		t.Error("ref already at the pushed value should count as landed")
	}
	del := []*update{{dst: "refs/heads/gone"}}
	if !landed(&builtPack{}, del, snap) {
		t.Error("deleting an absent ref should count as landed")
	}
	if landed(&builtPack{}, nil, snap) {
		t.Error("no updates must not count as landed")
	}
}
