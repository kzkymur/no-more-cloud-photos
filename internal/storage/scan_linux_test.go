//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScanIsDeterministicNoFollowAndHashesCanonicalObjects(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	payload := []byte("checker bytes")
	publishTestObject(t, store, key, payload)
	if err := os.WriteFile(filepath.Join(root, "z-unexpected"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a-link")); err != nil {
		t.Fatal(err)
	}

	first, err := store.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	keys := func(observations []Observation) []string {
		result := make([]string, len(observations))
		for i := range observations {
			result[i] = observations[i].RelativeKey
		}
		return result
	}
	if !reflect.DeepEqual(keys(first), keys(second)) {
		t.Fatalf("scan order changed: %v != %v", keys(first), keys(second))
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].RelativeKey >= first[i].RelativeKey {
			t.Fatalf("scan is not strictly sorted: %q >= %q", first[i-1].RelativeKey, first[i].RelativeKey)
		}
	}
	var foundObject, foundLink bool
	wantDigest := sha256.Sum256(payload)
	for _, observation := range first {
		switch observation.RelativeKey {
		case key.String():
			foundObject = true
			if !observation.Stable || observation.Type != ObservationRegular || observation.SHA256 == nil || *observation.SHA256 != wantDigest {
				t.Fatalf("canonical observation = %+v", observation)
			}
		case "a-link":
			foundLink = true
			if observation.Type != ObservationSymlink || !observation.Stable {
				t.Fatalf("symlink observation = %+v", observation)
			}
		case "a-link/secret":
			t.Fatal("scan followed a symlink outside the pinned root")
		}
	}
	if !foundObject || !foundLink {
		t.Fatalf("missing observations: object=%v link=%v", foundObject, foundLink)
	}
}

func TestScanMarksNamespaceReplacementUnstable(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	publishTestObject(t, store, key, []byte("original bytes"))
	leaf := filepath.Join(root, filepath.FromSlash(key.String()))
	originalRead := store.ops.read
	replaced := false
	store.ops.read = func(fd int, buffer []byte) (int, error) {
		if !replaced {
			replaced = true
			if err := os.Rename(leaf, leaf+".old"); err != nil {
				return 0, err
			}
			if err := os.WriteFile(leaf, []byte("replacement!!"), 0o600); err != nil {
				return 0, err
			}
		}
		return originalRead(fd, buffer)
	}
	observations, err := store.Scan(context.Background())
	if !errors.Is(err, ErrUnstableScan) {
		t.Fatalf("Scan() error = %v, want ErrUnstableScan", err)
	}
	for _, observation := range observations {
		if observation.RelativeKey == key.String() {
			if observation.Stable || observation.SHA256 != nil {
				t.Fatalf("replaced object was trusted: %+v", observation)
			}
			return
		}
	}
	t.Fatal("canonical object observation is absent")
}

func TestScanInvalidatesDescendantsWhenDirectoryNamespaceChanges(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	publishTestObject(t, store, key, []byte("directory race bytes"))
	originalFstatat := store.ops.fstatat
	originalsStats := 0
	store.ops.fstatat = func(fd int, name string, stat *unix.Stat_t, flags int) error {
		if name == "originals" {
			originalsStats++
			if originalsStats == 2 {
				if err := os.Rename(filepath.Join(root, "originals"), filepath.Join(root, "originals.old")); err != nil {
					return err
				}
				if err := os.Mkdir(filepath.Join(root, "originals"), 0o700); err != nil {
					return err
				}
			}
		}
		return originalFstatat(fd, name, stat, flags)
	}
	observations, err := store.Scan(context.Background())
	if !errors.Is(err, ErrUnstableScan) {
		t.Fatalf("Scan() error = %v, want ErrUnstableScan", err)
	}
	for _, observation := range observations {
		if observation.RelativeKey == key.String() {
			if observation.Stable || observation.SHA256 != nil {
				t.Fatalf("detached descendant remained trusted: %+v", observation)
			}
			return
		}
	}
	t.Fatal("detached descendant observation is absent")
}

func TestScanDetectsEntryCreatedDuringFinalRevalidation(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	publishTestObject(t, store, key, []byte("final validation bytes"))
	_, leafName := splitKey(key.String())
	parent := filepath.Dir(filepath.Join(root, filepath.FromSlash(key.String())))
	originalFstatat := store.ops.fstatat
	leafStats := 0
	store.ops.fstatat = func(fd int, name string, stat *unix.Stat_t, flags int) error {
		if name == leafName {
			leafStats++
			if leafStats == 3 {
				if err := os.WriteFile(filepath.Join(parent, "late-object"), []byte("late"), 0o600); err != nil {
					return err
				}
			}
		}
		return originalFstatat(fd, name, stat, flags)
	}
	observations, err := store.Scan(context.Background())
	if !errors.Is(err, ErrUnstableScan) {
		t.Fatalf("Scan() = %v, %v; want unstable source", observations, err)
	}
	for _, observation := range observations {
		if observation.Stable || observation.SHA256 != nil {
			t.Fatalf("unstable scan retained trusted evidence: %+v", observation)
		}
	}
}

func TestScanCancellationAndSpecialType(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	observations, err := store.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Type != ObservationFIFO {
		t.Fatalf("FIFO observations = %+v", observations)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Scan() error = %v", err)
	}
}
