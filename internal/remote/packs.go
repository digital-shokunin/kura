package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/digital-shokunin/kura/internal/crypt"
	"github.com/digital-shokunin/kura/internal/store"
)

// PartSize is the maximum size of one stored pack part.
var PartSize int64 = 64 << 20

func partKey(packID string, i int) string { return fmt.Sprintf("packs/%s/%04d", packID, i) }

// packSlot is the associated-data slot for a pack's ciphertext.
func packSlot(packID string) string { return "packs/" + packID }

// UploadPack encrypts the plaintext pack in src (size bytes), pads it, and
// stores it in parts. The returned Pack is not referenced by any manifest
// until a Commit includes it.
func (r *Remote) UploadPack(ctx context.Context, src io.Reader, size int64) (Pack, error) {
	p := Pack{ID: randHex(16), Created: now(), Size: size}
	ct, err := os.CreateTemp("", "kura-pack-*")
	if err != nil {
		return p, err
	}
	defer os.Remove(ct.Name())
	defer ct.Close()

	w, err := r.Key.Encrypt(ct, r.RepoID, packSlot(p.ID))
	if err != nil {
		return p, err
	}
	if err := crypt.WritePadded(w, src, size); err != nil {
		return p, err
	}
	if err := w.Close(); err != nil {
		return p, err
	}
	total, err := ct.Seek(0, io.SeekCurrent)
	if err != nil {
		return p, err
	}
	for i, off := 0, int64(0); off < total; i, off = i+1, off+PartSize {
		n := min(PartSize, total-off)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(ct, off, n)); err != nil {
			return p, err
		}
		if err := r.Store.Put(ctx, partKey(p.ID, i), io.NewSectionReader(ct, off, n), n); err != nil {
			return p, fmt.Errorf("remote: uploading pack part %d: %w", i, err)
		}
		p.Parts = append(p.Parts, Part{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n})
	}
	p.stored = time.Now().Round(0)
	return p, nil
}

// VerifyPackStored checks every part of p is present with the right size.
func (r *Remote) VerifyPackStored(ctx context.Context, p Pack) error {
	for i, part := range p.Parts {
		info, err := r.Store.Head(ctx, partKey(p.ID, i))
		if err != nil {
			return fmt.Errorf("remote: pack %s part %d: %w", p.ID, i, err)
		}
		if info.Size != part.Size {
			return fmt.Errorf("remote: pack %s part %d is %d bytes, want %d", p.ID, i, info.Size, part.Size)
		}
	}
	return nil
}

// OpenPack streams the decrypted plaintext pack. The final error, including
// any authentication failure, is returned by Read once the stream is drained.
func (r *Remote) OpenPack(ctx context.Context, p Pack) io.ReadCloser {
	ctR, ctW := io.Pipe()
	go func() {
		for i, part := range p.Parts {
			if err := copyPart(ctx, r.Store, ctW, partKey(p.ID, i), part); err != nil {
				ctW.CloseWithError(err)
				return
			}
		}
		ctW.Close()
	}()
	ptR, ptW := io.Pipe()
	go func() {
		dec, err := r.Key.Decrypt(ctR, r.RepoID, packSlot(p.ID))
		if err == nil {
			var n int64
			n, err = crypt.ReadPadded(ptW, dec)
			if err == nil && n != p.Size {
				err = fmt.Errorf("remote: pack %s decrypted to %d bytes, manifest says %d", p.ID, n, p.Size)
			}
		}
		if err != nil {
			err = fmt.Errorf("remote: pack %s: %w", p.ID, err)
		}
		ctR.CloseWithError(errors.New("pack reader closed"))
		ptW.CloseWithError(err)
	}()
	return &packReader{PipeReader: ptR, ct: ctR}
}

type packReader struct {
	*io.PipeReader
	ct *io.PipeReader
}

func (p *packReader) Close() error {
	p.ct.CloseWithError(errors.New("pack reader closed"))
	return p.PipeReader.Close()
}

func copyPart(ctx context.Context, s store.Store, w io.Writer, key string, want Part) error {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(w, io.TeeReader(io.LimitReader(rc, want.Size+1), h))
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if n != want.Size || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return fmt.Errorf("%s: stored part does not match manifest (size %d, want %d)", key, n, want.Size)
	}
	return nil
}

// DeletePack removes a pack's parts.
func (r *Remote) DeletePack(ctx context.Context, p Pack) error {
	for i := range p.Parts {
		if err := r.Store.Delete(ctx, partKey(p.ID, i)); err != nil {
			return err
		}
	}
	return nil
}

// UploadDeadline is how long after storing a pack's last part Commit will
// still reference it, so a laptop that slept between upload and commit cannot
// commit packs that a concurrent gc may have treated as orphans.
const UploadDeadline = time.Hour

// Stale reports whether p was stored too long ago to commit, measured on the
// wall clock (Go's monotonic clock stops while a Mac sleeps). A pack read from
// a manifest has no upload time and is always stale.
func (p Pack) Stale() bool {
	return p.stored.IsZero() || time.Now().Round(0).Sub(p.stored) > UploadDeadline
}

// HumanBytes formats a byte count for progress messages.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
