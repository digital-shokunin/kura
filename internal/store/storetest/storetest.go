// Package storetest is a conformance suite every store backend must pass.
// It is used by unit tests and by `kura probe` against live buckets.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/digital-shokunin/kura/internal/store"
)

// Racers is how many concurrent PutIfAbsent calls race for one key.
const Racers = 20

// Check is one named conformance check.
type Check struct {
	Name string
	Run  func(ctx context.Context, s store.Store, prefix string) error
}

// Checks returns the conformance checks. Each uses keys under prefix only.
func Checks() []Check {
	return []Check{
		{"get missing returns ErrNotFound", checkGetMissing},
		{"put/get/head round trip", checkRoundTrip},
		{"put overwrites", checkOverwrite},
		{"put-if-absent refuses existing key", checkPutIfAbsent},
		{fmt.Sprintf("put-if-absent: exactly one of %d racers wins", Racers), checkRace},
		{"list returns keys under prefix", checkList},
		{"delete is idempotent", checkDelete},
	}
}

// Run executes all checks, cleaning up keys under prefix afterwards.
func Run(ctx context.Context, s store.Store, prefix string, report func(name string, err error)) (failed int) {
	defer cleanup(ctx, s, prefix)
	for _, c := range Checks() {
		err := c.Run(ctx, s, prefix)
		if err != nil {
			failed++
		}
		report(c.Name, err)
	}
	return failed
}

func cleanup(ctx context.Context, s store.Store, prefix string) {
	keys, _ := s.List(ctx, prefix)
	for _, k := range keys {
		s.Delete(ctx, k)
	}
}

func get(ctx context.Context, s store.Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func put(ctx context.Context, s store.Store, key string, b []byte) error {
	return s.Put(ctx, key, bytes.NewReader(b), int64(len(b)))
}

func checkGetMissing(ctx context.Context, s store.Store, p string) error {
	if _, err := s.Get(ctx, p+"missing"); !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("Get: got %v, want ErrNotFound", err)
	}
	if _, err := s.Head(ctx, p+"missing"); !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("Head: got %v, want ErrNotFound", err)
	}
	return nil
}

func checkRoundTrip(ctx context.Context, s store.Store, p string) error {
	want := bytes.Repeat([]byte("kura"), 1000)
	key := p + "dir/obj"
	if err := put(ctx, s, key, want); err != nil {
		return err
	}
	got, err := get(ctx, s, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("contents differ")
	}
	info, err := s.Head(ctx, key)
	if err != nil {
		return err
	}
	if info.Size != int64(len(want)) {
		return fmt.Errorf("Head size %d, want %d", info.Size, len(want))
	}
	return nil
}

func checkOverwrite(ctx context.Context, s store.Store, p string) error {
	key := p + "over"
	if err := put(ctx, s, key, []byte("one")); err != nil {
		return err
	}
	if err := put(ctx, s, key, []byte("two")); err != nil {
		return err
	}
	got, err := get(ctx, s, key)
	if err != nil {
		return err
	}
	if string(got) != "two" {
		return fmt.Errorf("got %q after overwrite", got)
	}
	return nil
}

func checkPutIfAbsent(ctx context.Context, s store.Store, p string) error {
	key := p + "cas"
	if err := s.PutIfAbsent(ctx, key, bytes.NewReader([]byte("first")), 5); err != nil {
		return fmt.Errorf("first PutIfAbsent: %w", err)
	}
	err := s.PutIfAbsent(ctx, key, bytes.NewReader([]byte("second")), 6)
	if !errors.Is(err, store.ErrExists) {
		return fmt.Errorf("second PutIfAbsent: got %v, want ErrExists (backend may ignore If-None-Match)", err)
	}
	got, err := get(ctx, s, key)
	if err != nil {
		return err
	}
	if string(got) != "first" {
		return fmt.Errorf("object was overwritten: %q", got)
	}
	return nil
}

func checkRace(ctx context.Context, s store.Store, p string) error {
	key := p + "race"
	var wg sync.WaitGroup
	errs := make([]error, Racers)
	for i := range Racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := fmt.Appendf(nil, "racer-%02d", i)
			errs[i] = s.PutIfAbsent(ctx, key, bytes.NewReader(b), int64(len(b)))
		}()
	}
	wg.Wait()
	winners, winner := 0, -1
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			winner = i
		case errors.Is(err, store.ErrExists):
		default:
			return fmt.Errorf("racer %d: unexpected error: %w", i, err)
		}
	}
	if winners != 1 {
		return fmt.Errorf("%d racers won, want exactly 1", winners)
	}
	got, err := get(ctx, s, key)
	if err != nil {
		return err
	}
	if want := fmt.Sprintf("racer-%02d", winner); string(got) != want {
		return fmt.Errorf("stored %q but winner was %q", got, want)
	}
	return nil
}

func checkList(ctx context.Context, s store.Store, p string) error {
	want := []string{p + "list/a", p + "list/b/c", p + "list/d"}
	for _, k := range want {
		if err := put(ctx, s, k, []byte("x")); err != nil {
			return err
		}
	}
	if err := put(ctx, s, p+"other", []byte("x")); err != nil {
		return err
	}
	got, err := s.List(ctx, p+"list/")
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("List = %q, want %q", got, want)
	}
	return nil
}

func checkDelete(ctx context.Context, s store.Store, p string) error {
	key := p + "del"
	if err := put(ctx, s, key, []byte("x")); err != nil {
		return err
	}
	if err := s.Delete(ctx, key); err != nil {
		return err
	}
	if err := s.Delete(ctx, key); err != nil {
		return fmt.Errorf("second delete: %w", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("Get after delete: %v", err)
	}
	return nil
}
