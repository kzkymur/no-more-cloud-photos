//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPublishOpenAndIdempotentDelete(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key, attempt := testOriginalKey(t), testAttempt(t)
	payload := []byte("immutable original bytes")
	digest := sha256.Sum256(payload)
	temporary, err := store.BeginOriginal(context.Background(), key, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := temporary.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	var tempStat, directoryStat unix.Stat_t
	if err := unix.Fstat(temporary.fileFD, &tempStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(temporary.parentFD, &directoryStat); err != nil {
		t.Fatal(err)
	}
	if tempStat.Dev != directoryStat.Dev {
		t.Fatalf("temporary device %d differs from final directory device %d", tempStat.Dev, directoryStat.Dev)
	}
	info, err := temporary.Publish(context.Background(), Validation{ExpectedSize: int64(len(payload)), ExpectedSHA256: &digest})
	if err != nil || info.Size != int64(len(payload)) || info.SHA256 != digest {
		t.Fatalf("Publish() = %+v, %v", info, err)
	}
	file, err := store.OpenOriginal(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !reflect.DeepEqual(got, payload) {
		t.Fatalf("read published bytes = %q, %v", got, err)
	}

	collision, err := store.BeginOriginal(context.Background(), key, mustAttempt(t, "44444444-5555-4666-8777-888888888888"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collision.Write([]byte("replacement"))
	if _, err := collision.Publish(context.Background(), Validation{ExpectedSize: int64(len("replacement"))}); !errors.Is(err, ErrCollision) {
		t.Fatalf("collision Publish() error = %v", err)
	}
	if err := collision.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readRelative(t, root, key.String()); !reflect.DeepEqual(got, payload) {
		t.Fatalf("collision changed existing bytes: %q", got)
	}

	deleted, err := store.DeleteOriginal(context.Background(), key)
	if err != nil || deleted.Missing {
		t.Fatalf("DeleteOriginal() = %+v, %v", deleted, err)
	}
	deleted, err = store.DeleteOriginal(context.Background(), key)
	if err != nil || !deleted.Missing {
		t.Fatalf("second DeleteOriginal() = %+v, %v", deleted, err)
	}
}

func TestDirectoryDurabilityOrder(t *testing.T) {
	root := t.TempDir()
	var events []FaultEvent
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		events = append(events, event)
		return nil
	})})
	temporary, err := store.BeginRendition(context.Background(), testRenditionKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range events {
		if event.Boundary == BoundaryDirectoryCreate || event.Boundary == BoundaryNewDirectorySync || event.Boundary == BoundaryParentDirectorySync {
			got = append(got, string(event.Boundary)+":"+string(event.Phase)+":"+string(rune('0'+event.Depth)))
		}
	}
	var want []string
	for depth := 0; depth < 4; depth++ {
		for _, pair := range [][2]string{{string(BoundaryDirectoryCreate), string(Before)}, {string(BoundaryDirectoryCreate), string(After)}, {string(BoundaryNewDirectorySync), string(Before)}, {string(BoundaryNewDirectorySync), string(After)}, {string(BoundaryParentDirectorySync), string(Before)}, {string(BoundaryParentDirectorySync), string(After)}} {
			want = append(want, pair[0]+":"+pair[1]+":"+string(rune('0'+depth)))
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("directory durability events = %v, want %v", got, want)
	}
}

func TestPublishAndDeleteBoundaryOrder(t *testing.T) {
	root := t.TempDir()
	var events []FaultEvent
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		events = append(events, event)
		return nil
	})})
	key := testOriginalKey(t)
	temporary, err := store.BeginOriginal(context.Background(), key, testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = temporary.Write([]byte("bytes"))
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteOriginal(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range events {
		switch event.Boundary {
		case BoundaryTempCreate, BoundaryWrite, BoundaryFileSync, BoundaryValidation, BoundaryRename, BoundaryFinalDirectorySync, BoundaryDelete, BoundaryDeleteDirectorySync:
			got = append(got, string(event.Boundary)+":"+string(event.Phase))
		}
	}
	want := []string{
		"temp_create:before", "temp_create:after",
		"write:before", "write:after",
		"file_sync:before", "file_sync:after",
		"validation:before", "validation:after",
		"rename:before", "rename:after",
		"final_directory_sync:before", "final_directory_sync:after",
		"delete:before", "delete:after",
		"delete_directory_sync:before", "delete_directory_sync:after",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publish/delete events = %v, want %v", got, want)
	}
}

func TestShortWritesAndNoProgress(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	key := testOriginalKey(t)
	originalWrite := store.ops.write
	store.ops.write = func(fd int, value []byte) (int, error) {
		if len(value) > 2 {
			value = value[:2]
		}
		return originalWrite(fd, value)
	}
	temporary, err := store.BeginOriginal(context.Background(), key, testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("many short writes")
	if written, err := temporary.Write(payload); err != nil || written != len(payload) {
		t.Fatalf("short Write() = %d, %v", written, err)
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}

	store.ops.write = func(int, []byte) (int, error) { return 0, nil }
	temporary, err = store.BeginOriginal(context.Background(), key, mustAttempt(t, "44444444-5555-4666-8777-888888888888"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write(payload); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("zero-progress Write() error = %v", err)
	}
	store.ops.write = originalWrite
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFaultPhasesAreInjectable(t *testing.T) {
	fault := errors.New("write boundary")
	for _, phase := range []Phase{Before, After} {
		t.Run(string(phase), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{Faults: faultAt(BoundaryWrite, phase, fault)})
			temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := temporary.Write([]byte("bytes")); !errors.Is(err, fault) {
				t.Fatalf("Write() error = %v", err)
			}
			store.faults = nil
			if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5}); !errors.Is(err, ErrValidation) {
				t.Fatalf("Publish() after write boundary error = %v", err)
			}
			if err := temporary.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSharedDatabaseCommitBoundaries(t *testing.T) {
	var got []FaultEvent
	injector := FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		got = append(got, event)
		return nil
	})
	for _, boundary := range []Boundary{BoundaryBeforeDBCommit, BoundaryAfterDBCommit} {
		if err := Inject(context.Background(), injector, FaultEvent{Boundary: boundary, Phase: Before, Key: testOriginalKey(t).String()}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].Boundary != BoundaryBeforeDBCommit || got[1].Boundary != BoundaryAfterDBCommit {
		t.Fatalf("DB boundary events = %v", got)
	}
}

func TestWriteSyncAndRenameFailuresDoNotCreateFinal(t *testing.T) {
	t.Run("partial write with ENOSPC", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		originalWrite := store.ops.write
		called := false
		store.ops.write = func(fd int, value []byte) (int, error) {
			if called {
				return 0, syscall.ENOSPC
			}
			called = true
			if len(value) > 2 {
				value = value[:2]
			}
			count, _ := originalWrite(fd, value)
			return count, syscall.ENOSPC
		}
		if written, err := temporary.Write([]byte("payload")); written != 2 || !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("ENOSPC Write() = %d, %v", written, err)
		}
		store.ops.write = originalWrite
		if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 2}); !errors.Is(err, ErrValidation) {
			t.Fatalf("Publish() after failed write error = %v", err)
		}
		if err := temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertFinalMissing(t, root, testOriginalKey(t).String())
	})

	t.Run("file fsync", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("payload"))
		originalSync := store.ops.fsync
		store.ops.fsync = func(fd int) error {
			if fd == temporary.fileFD {
				return syscall.EIO
			}
			return originalSync(fd)
		}
		if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 7}); !errors.Is(err, ErrDurability) {
			t.Fatalf("file-sync Publish() error = %v", err)
		}
		store.ops.fsync = originalSync
		if err := temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertFinalMissing(t, root, testOriginalKey(t).String())
	})

	t.Run("rename", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("payload"))
		originalRename := store.ops.renameat2
		store.ops.renameat2 = func(int, string, int, string, uint) error { return syscall.EIO }
		if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 7}); err == nil {
			t.Fatal("rename failure Publish() succeeded")
		}
		store.ops.renameat2 = originalRename
		if err := temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertFinalMissing(t, root, testOriginalKey(t).String())
	})
}

func TestEveryDirectoryDurabilityBoundaryIsInjectable(t *testing.T) {
	fault := errors.New("crash boundary")
	for _, boundary := range []Boundary{BoundaryDirectoryCreate, BoundaryNewDirectorySync, BoundaryParentDirectorySync} {
		for _, phase := range []Phase{Before, After} {
			for depth := range 4 {
				t.Run(fmt.Sprintf("%s_%s_depth_%d", boundary, phase, depth), func(t *testing.T) {
					root := t.TempDir()
					store := openTestStore(t, root, Options{Faults: faultAtDepth(boundary, phase, depth, fault)})
					if _, err := store.BeginRendition(context.Background(), testRenditionKey(t), testAttempt(t)); !errors.Is(err, fault) {
						t.Fatalf("BeginRendition() error = %v", err)
					}
					assertFinalMissing(t, root, testRenditionKey(t).String())
				})
			}
		}
	}
}

func TestDirectoryDurabilityRetryRepairsPartialCreation(t *testing.T) {
	fault := errors.New("first sync failed")
	for _, point := range []struct {
		boundary Boundary
		phase    Phase
	}{
		{BoundaryDirectoryCreate, After},
		{BoundaryNewDirectorySync, Before},
		{BoundaryNewDirectorySync, After},
		{BoundaryParentDirectorySync, Before},
		{BoundaryParentDirectorySync, After},
	} {
		for depth := range 4 {
			t.Run(fmt.Sprintf("%s_%s_depth_%d", point.boundary, point.phase, depth), func(t *testing.T) {
				key := testRenditionKey(t)
				store := openTestStore(t, t.TempDir(), Options{Faults: faultAtDepth(point.boundary, point.phase, depth, fault)})
				if _, err := store.BeginRendition(context.Background(), key, testAttempt(t)); !errors.Is(err, fault) {
					t.Fatalf("first BeginRendition() error = %v", err)
				}
				var retryEvents []FaultEvent
				store.faults = FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
					retryEvents = append(retryEvents, event)
					return nil
				})
				temporary, err := store.BeginRendition(context.Background(), key, testAttempt(t))
				if err != nil {
					t.Fatalf("retry BeginRendition() error = %v", err)
				}
				if err := temporary.Abort(context.Background()); err != nil {
					t.Fatal(err)
				}
				var got []FaultEvent
				for _, event := range retryEvents {
					if event.Depth <= depth && (event.Boundary == BoundaryDirectoryCreate || event.Boundary == BoundaryNewDirectorySync || event.Boundary == BoundaryParentDirectorySync) {
						got = append(got, event)
					}
				}
				var want []FaultEvent
				for existingDepth := 0; existingDepth <= depth; existingDepth++ {
					want = append(want,
						FaultEvent{Boundary: BoundaryNewDirectorySync, Phase: Before, Key: key.String(), Depth: existingDepth},
						FaultEvent{Boundary: BoundaryNewDirectorySync, Phase: After, Key: key.String(), Depth: existingDepth},
						FaultEvent{Boundary: BoundaryParentDirectorySync, Phase: Before, Key: key.String(), Depth: existingDepth},
						FaultEvent{Boundary: BoundaryParentDirectorySync, Phase: After, Key: key.String(), Depth: existingDepth},
					)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("retry existing-directory sequence = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestConcurrentDirectoryCreationRace(t *testing.T) {
	var arrivals sync.WaitGroup
	arrivals.Add(2)
	release := make(chan struct{})
	injector := FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary == BoundaryDirectoryCreate && event.Phase == Before && event.Depth == 0 {
			arrivals.Done()
			<-release
		}
		return nil
	})
	store := openTestStore(t, t.TempDir(), Options{Faults: injector})
	first := testOriginalKey(t)
	secondID, err := ParseOriginalID("01abcdef-89ab-4cde-8f01-23456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOriginalKey(secondID, OriginalPNG)
	if err != nil {
		t.Fatal(err)
	}
	type beginResult struct {
		temporary *Temp
		err       error
	}
	results := make(chan beginResult, 2)
	for index, key := range []OriginalKey{first, second} {
		go func(index int, key OriginalKey) {
			attempt := testAttemptID
			if index == 1 {
				attempt = "44444444-5555-4666-8777-888888888888"
			}
			temporary, err := store.BeginOriginal(context.Background(), key, mustParseAttempt(attempt))
			results <- beginResult{temporary: temporary, err: err}
		}(index, key)
	}
	arrivals.Wait()
	close(release)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent BeginOriginal() error = %v", result.err)
		}
		if err := result.temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCancellationBeforeFilesystemMutation(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.BeginOriginal(ctx, testOriginalKey(t), testAttempt(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled BeginOriginal() error = %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("cancelled BeginOriginal() entries = %v, %v", entries, err)
	}
}

func TestCancellationImmediatelyAfterTempCreateCleansDurably(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	originalOpen := store.ops.openat
	store.ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		opened, err := originalOpen(fd, name, flags, mode)
		if err == nil && flags&unix.O_CREAT != 0 {
			cancel()
		}
		return opened, err
	}
	key := testOriginalKey(t)
	if _, err := store.BeginOriginal(ctx, key, testAttempt(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginOriginal(canceled after create) error = %v", err)
	}
	directories, finalName := splitKey(key.String())
	tempName := "." + finalName + "." + testAttempt(t).String() + ".tmp"
	if _, err := os.Stat(filepath.Join(append([]string{root}, append(directories, tempName)...)...)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled temp still exists: %v", err)
	}
	temporary, err := store.BeginOriginal(context.Background(), key, testAttempt(t))
	if err != nil {
		t.Fatalf("same attempt remained collided: %v", err)
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledTempCleanupSyncFailureReportsUncertainty(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	originalOpen := store.ops.openat
	failCleanupSync := false
	store.ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		opened, err := originalOpen(fd, name, flags, mode)
		if err == nil && flags&unix.O_CREAT != 0 {
			failCleanupSync = true
			cancel()
		}
		return opened, err
	}
	originalSync := store.ops.fsync
	store.ops.fsync = func(fd int) error {
		if failCleanupSync {
			return syscall.EROFS
		}
		return originalSync(fd)
	}
	_, err := store.BeginOriginal(ctx, testOriginalKey(t), testAttempt(t))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUncertain) ||
		!errors.Is(err, ErrDurability) || !errors.Is(err, ErrReadOnly) {
		t.Fatalf("BeginOriginal(canceled cleanup EROFS) error = %v", err)
	}
}

func TestCanceledTempCleanupUnlinkFailureReportsDurabilityUncertainty(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	originalOpen := store.ops.openat
	store.ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		opened, err := originalOpen(fd, name, flags, mode)
		if err == nil && flags&unix.O_CREAT != 0 {
			cancel()
		}
		return opened, err
	}
	store.ops.unlinkat = func(int, string, int) error { return syscall.EROFS }
	_, err := store.BeginOriginal(ctx, testOriginalKey(t), testAttempt(t))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUncertain) ||
		!errors.Is(err, ErrDurability) || !errors.Is(err, ErrReadOnly) {
		t.Fatalf("BeginOriginal(canceled cleanup unlink EROFS) error = %v", err)
	}
}

func TestCloseDoesNotDeadlockWithUnfinishedTemp(t *testing.T) {
	store, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Store.Close() deadlocked with unfinished Temp")
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAbortCancellationAndPostMutationCleanup(t *testing.T) {
	t.Run("already canceled leaves retryable temp", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := temporary.Abort(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Abort(canceled) error = %v", err)
		}
		if temporary.finished {
			t.Fatal("canceled Abort finished an untouched temp")
		}
		if err := temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancellation after unlink cannot skip durability", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		originalUnlink := store.ops.unlinkat
		store.ops.unlinkat = func(fd int, name string, flags int) error {
			err := originalUnlink(fd, name, flags)
			cancel()
			return err
		}
		if err := temporary.Abort(ctx); err != nil {
			t.Fatalf("Abort(canceled after unlink) error = %v", err)
		}
		if !temporary.finished {
			t.Fatal("Abort did not finish after durable unlink")
		}
	})

	t.Run("sync failure reports uncertainty and closes", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		store.ops.fsync = func(int) error { return syscall.EIO }
		err = temporary.Abort(context.Background())
		if !errors.Is(err, ErrOutcomeUncertain) || !errors.Is(err, ErrDurability) {
			t.Fatalf("Abort(sync failure) error = %v", err)
		}
		if !temporary.finished {
			t.Fatal("Abort sync failure retained descriptors")
		}
	})
}

func TestValidationAndReadOnlyValidator(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = temporary.Write([]byte("bytes"))
	wrong := sha256.Sum256([]byte("wrong"))
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5, ExpectedSHA256: &wrong}); !errors.Is(err, ErrValidation) {
		t.Fatalf("digest mismatch error = %v", err)
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}

	temporary, err = store.BeginOriginal(context.Background(), testOriginalKey(t), mustAttempt(t, "44444444-5555-4666-8777-888888888888"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = temporary.Write([]byte("bytes"))
	_, err = temporary.Publish(context.Background(), Validation{ExpectedSize: 5, Validate: func(_ context.Context, file *os.File) error {
		_, writeErr := file.Write([]byte("mutation"))
		if writeErr == nil {
			return errors.New("validator unexpectedly mutated file")
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("read-only validator Publish() error = %v", err)
	}
}

func TestValidationHashStopsOnCancellationBeforeRename(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write(make([]byte, 1024*1024)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	originalRead := store.ops.read
	readCalls, bytesRead := 0, 0
	store.ops.read = func(fd int, value []byte) (int, error) {
		count, err := originalRead(fd, value)
		readCalls++
		bytesRead += count
		if count > 0 {
			cancel()
		}
		return count, err
	}
	renameCalled := false
	originalRename := store.ops.renameat2
	store.ops.renameat2 = func(oldFD int, oldName string, newFD int, newName string, flags uint) error {
		renameCalled = true
		return originalRename(oldFD, oldName, newFD, newName, flags)
	}
	if _, err := temporary.Publish(ctx, Validation{ExpectedSize: 1024 * 1024}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish(canceled validation) error = %v", err)
	}
	if renameCalled {
		t.Fatal("canceled validation reached rename")
	}
	if readCalls != 1 || bytesRead <= 0 || bytesRead >= 1024*1024 {
		t.Fatalf("canceled hash reads = %d calls, %d bytes", readCalls, bytesRead)
	}
	assertFinalMissing(t, root, testOriginalKey(t).String())
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestValidationHashRetriesInterruptedRead(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write([]byte("bytes")); err != nil {
		t.Fatal(err)
	}
	originalRead := store.ops.read
	interrupted := false
	store.ops.read = func(fd int, value []byte) (int, error) {
		if !interrupted {
			interrupted = true
			return 0, syscall.EINTR
		}
		return originalRead(fd, value)
	}
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5}); err != nil {
		t.Fatalf("Publish(after EINTR) error = %v", err)
	}
}

func TestTrustedWriterCanUseDuplicateDescriptor(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	temporary, err := store.BeginRendition(context.Background(), testRenditionKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := temporary.UseWritableFile(func(file *os.File) error {
		_, err := file.Write([]byte("codec output"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: int64(len("codec output"))}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.OpenRendition(context.Background(), testRenditionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || string(got) != "codec output" {
		t.Fatalf("codec output = %q, %v", got, err)
	}
}

func TestWritableCallbackPanicClosesAndPoisonsTemp(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = temporary.UseWritableFile(func(file *os.File) error {
			_, _ = file.Write([]byte("partial"))
			panic("codec crashed")
		})
	}()
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 7}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Publish() after writer panic error = %v", err)
	}
	if err := temporary.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinksAndPinnedRootCannotEscape(t *testing.T) {
	outside := t.TempDir()
	rootParent := t.TempDir()
	root := filepath.Join(rootParent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := filepath.Join(rootParent, "symlink")
	if err := os.Symlink(root, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(symlinkRoot, Options{}); err == nil {
		_ = store.Close()
		t.Fatal("Open() accepted symlink root")
	}
	intermediateTarget := t.TempDir()
	outsideRoot := filepath.Join(intermediateTarget, "storage")
	if err := os.Mkdir(outsideRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(rootParent, "intermediate")
	if err := os.Symlink(intermediateTarget, intermediate); err != nil {
		t.Fatal(err)
	}
	if escaped, err := Open(filepath.Join(intermediate, "storage"), Options{}); err == nil {
		_ = escaped.Close()
		t.Fatal("Open() accepted intermediate symlink")
	}

	store := openTestStore(t, root, Options{})
	if err := os.Symlink(outside, filepath.Join(root, "originals")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t)); !errors.Is(err, ErrSymlink) && !errors.Is(err, ErrUnexpectedType) {
		t.Fatalf("symlink component error = %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("symlink operation touched outside directory: %v", entries)
	}
	if err := os.Remove(filepath.Join(root, "originals")); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(rootParent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
	if err != nil {
		t.Fatalf("pinned root BeginOriginal() error = %v", err)
	}
	_, _ = temporary.Write([]byte("pinned"))
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.FromSlash(testOriginalKey(t).String()))); err != nil {
		t.Fatalf("published object missing under pinned root: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("root replacement touched outside directory: %v", entries)
	}
}

func TestOpenRejectsMissingAndNonDirectoryRoots(t *testing.T) {
	parent := t.TempDir()
	regular := filepath.Join(parent, "regular")
	if err := os.WriteFile(regular, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(parent, "missing"), regular} {
		if store, err := Open(root, Options{}); err == nil {
			_ = store.Close()
			t.Fatalf("Open(%q) unexpectedly succeeded", filepath.Base(root))
		}
	}
}

func TestLeafSymlinkDeleteRefused(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	leaf := filepath.Join(root, filepath.FromSlash(key.String()))
	if err := os.MkdirAll(filepath.Dir(leaf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteOriginal(context.Background(), key); !errors.Is(err, ErrSymlink) {
		t.Fatalf("DeleteOriginal(symlink) error = %v", err)
	}
	if _, err := store.OpenOriginal(context.Background(), key); !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenOriginal(symlink) error = %v", err)
	}
}

func TestDeleteRejectsUnexpectedLeafTypes(t *testing.T) {
	for _, kind := range []string{"directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root, Options{})
			key := testOriginalKey(t)
			leaf := filepath.Join(root, filepath.FromSlash(key.String()))
			if err := os.MkdirAll(filepath.Dir(leaf), 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "directory" {
				err = os.Mkdir(leaf, 0o700)
			} else {
				err = unix.Mkfifo(leaf, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.DeleteOriginal(context.Background(), key); !errors.Is(err, ErrUnexpectedType) {
				t.Fatalf("DeleteOriginal(%s) error = %v", kind, err)
			}
		})
	}
}

func TestOpenFIFORejectsWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	leaf := filepath.Join(root, filepath.FromSlash(key.String()))
	if err := os.MkdirAll(filepath.Dir(leaf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(leaf, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenOriginal(context.Background(), key); !errors.Is(err, ErrUnexpectedType) {
		t.Fatalf("OpenOriginal(FIFO) error = %v", err)
	}
}

func TestDirectorySyncSyscallFailureIsDurabilityError(t *testing.T) {
	for _, failure := range []error{syscall.EIO, syscall.EROFS} {
		t.Run(failure.Error(), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{})
			store.ops.fsync = func(int) error { return failure }
			_, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
			if !errors.Is(err, ErrDurability) {
				t.Fatalf("BeginOriginal() error = %v", err)
			}
			if errors.Is(failure, syscall.EROFS) && !errors.Is(err, ErrReadOnly) {
				t.Fatalf("BeginOriginal() lost read-only classification: %v", err)
			}
		})
	}
}

func TestPublishFaultSemantics(t *testing.T) {
	fault := errors.New("injected fault")
	for _, point := range []struct {
		boundary Boundary
		phase    Phase
	}{
		{BoundaryFileSync, Before}, {BoundaryFileSync, After},
		{BoundaryValidation, Before}, {BoundaryValidation, After},
	} {
		t.Run(string(point.boundary)+"_"+string(point.phase), func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root, Options{Faults: faultAt(point.boundary, point.phase, fault)})
			temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = temporary.Write([]byte("bytes"))
			if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5}); !errors.Is(err, fault) {
				t.Fatalf("Publish() error = %v", err)
			}
			assertFinalMissing(t, root, testOriginalKey(t).String())
			store.faults = nil
			if err := temporary.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("before rename leaves temp only", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{Faults: faultAt(BoundaryRename, Before, fault)})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("bytes"))
		if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 5}); !errors.Is(err, fault) {
			t.Fatalf("Publish() error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(storeRootPath(t, store), filepath.FromSlash(testOriginalKey(t).String()))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("final exists before rename: %v", err)
		}
		if err := temporary.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	for _, point := range []struct {
		name      string
		boundary  Boundary
		phase     Phase
		uncertain bool
	}{
		{name: "after rename", boundary: BoundaryRename, phase: After, uncertain: true},
		{name: "after final directory sync", boundary: BoundaryFinalDirectorySync, phase: After},
	} {
		t.Run(point.name, func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root, Options{Faults: faultAt(point.boundary, point.phase, fault)})
			temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = temporary.Write([]byte("bytes"))
			_, err = temporary.Publish(context.Background(), Validation{ExpectedSize: 5})
			var publishError *PublishError
			if !errors.As(err, &publishError) || !publishError.Published || publishError.Uncertain != point.uncertain || !errors.Is(err, fault) {
				t.Fatalf("post-publish error = %#v, %v", publishError, err)
			}
			if got := readRelative(t, root, testOriginalKey(t).String()); string(got) != "bytes" {
				t.Fatalf("published bytes = %q", got)
			}
		})
	}

	t.Run("after rename is published and uncertain", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{Faults: faultAt(BoundaryFinalDirectorySync, Before, fault)})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("bytes"))
		_, err = temporary.Publish(context.Background(), Validation{ExpectedSize: 5})
		var publishError *PublishError
		if !errors.As(err, &publishError) || !publishError.Published || !publishError.Uncertain || !errors.Is(err, ErrOutcomeUncertain) {
			t.Fatalf("post-rename error = %#v, %v", publishError, err)
		}
		if got := readRelative(t, root, testOriginalKey(t).String()); string(got) != "bytes" {
			t.Fatalf("published uncertain bytes = %q", got)
		}
	})
}

func TestFinalDeleteAndAbortSyncClassification(t *testing.T) {
	fault := errors.New("after durable sync")

	t.Run("publish syscall EROFS", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("bytes"))
		originalSync := store.ops.fsync
		store.ops.fsync = func(fd int) error {
			if fd == temporary.parentFD {
				return syscall.EROFS
			}
			return originalSync(fd)
		}
		_, err = temporary.Publish(context.Background(), Validation{ExpectedSize: 5})
		var publishError *PublishError
		if !errors.As(err, &publishError) || !publishError.Published || !publishError.Uncertain ||
			!errors.Is(err, ErrReadOnly) || !errors.Is(err, ErrDurability) || !errors.Is(err, ErrOutcomeUncertain) {
			t.Fatalf("Publish(EROFS) = %#v, %v", publishError, err)
		}
	})

	t.Run("publish after sync is durable", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{Faults: faultAt(BoundaryFinalDirectorySync, After, fault)})
		temporary, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = temporary.Write([]byte("bytes"))
		_, err = temporary.Publish(context.Background(), Validation{ExpectedSize: 5})
		var publishError *PublishError
		if !errors.As(err, &publishError) || !publishError.Published || publishError.Uncertain || !errors.Is(err, fault) ||
			errors.Is(err, ErrDurability) || errors.Is(err, ErrOutcomeUncertain) {
			t.Fatalf("Publish(after sync) = %#v, %v", publishError, err)
		}
	})

	for _, operation := range []string{"delete", "abort"} {
		t.Run(operation+" syscall EROFS", func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{})
			key := testOriginalKey(t)
			var err error
			var temporary *Temp
			if operation == "delete" {
				publishTestObject(t, store, key, []byte("bytes"))
			} else {
				temporary, err = store.BeginOriginal(context.Background(), key, testAttempt(t))
				if err != nil {
					t.Fatal(err)
				}
			}
			store.ops.fsync = func(int) error { return syscall.EROFS }
			if operation == "delete" {
				_, err = store.DeleteOriginal(context.Background(), key)
			} else {
				err = temporary.Abort(context.Background())
			}
			if !errors.Is(err, ErrReadOnly) || !errors.Is(err, ErrDurability) || !errors.Is(err, ErrOutcomeUncertain) {
				t.Fatalf("%s(EROFS) error = %v", operation, err)
			}
		})

		t.Run(operation+" after sync is durable", func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{})
			key := testOriginalKey(t)
			var err error
			var temporary *Temp
			if operation == "delete" {
				publishTestObject(t, store, key, []byte("bytes"))
			} else {
				temporary, err = store.BeginOriginal(context.Background(), key, testAttempt(t))
				if err != nil {
					t.Fatal(err)
				}
			}
			store.faults = faultAt(BoundaryDeleteDirectorySync, After, fault)
			if operation == "delete" {
				_, err = store.DeleteOriginal(context.Background(), key)
			} else {
				err = temporary.Abort(context.Background())
			}
			if !errors.Is(err, fault) || errors.Is(err, ErrDurability) || errors.Is(err, ErrOutcomeUncertain) {
				t.Fatalf("%s(after sync) error = %v", operation, err)
			}
		})
	}
}

func TestDeleteFaultsReportConvergentOutcomes(t *testing.T) {
	fault := errors.New("delete crash boundary")
	for _, test := range []struct {
		name         string
		boundary     Boundary
		phase        Phase
		missingAfter bool
	}{
		{name: "before unlink", boundary: BoundaryDelete, phase: Before},
		{name: "after unlink", boundary: BoundaryDelete, phase: After, missingAfter: true},
		{name: "before directory sync", boundary: BoundaryDeleteDirectorySync, phase: Before, missingAfter: true},
		{name: "after directory sync", boundary: BoundaryDeleteDirectorySync, phase: After, missingAfter: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root, Options{})
			key := testOriginalKey(t)
			publishTestObject(t, store, key, []byte("delete me"))
			store.faults = faultAt(test.boundary, test.phase, fault)
			_, err := store.DeleteOriginal(context.Background(), key)
			if !errors.Is(err, fault) {
				t.Fatalf("DeleteOriginal() error = %v", err)
			}
			_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(key.String())))
			if test.missingAfter != errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("missing after fault = %v, stat error=%v", test.missingAfter, statErr)
			}
			store.faults = nil
			if test.missingAfter {
				result, err := store.DeleteOriginal(context.Background(), key)
				if err != nil || !result.Missing {
					t.Fatalf("retry delete = %+v, %v", result, err)
				}
			}
		})
	}
}

func TestConcurrentDeleteConverges(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	key := testOriginalKey(t)
	publishTestObject(t, store, key, []byte("delete concurrently"))
	originalStat := store.ops.fstatat
	var statArrivals sync.WaitGroup
	statArrivals.Add(2)
	releaseStats := make(chan struct{})
	store.ops.fstatat = func(fd int, name string, stat *unix.Stat_t, flags int) error {
		err := originalStat(fd, name, stat, flags)
		if err == nil && name == "original.jpg" {
			statArrivals.Done()
			<-releaseStats
		}
		return err
	}
	type deleteOutcome struct {
		result DeleteResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan deleteOutcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := store.DeleteOriginal(context.Background(), key)
			results <- deleteOutcome{result: result, err: err}
		}()
	}
	close(start)
	statArrivals.Wait()
	close(releaseStats)
	missing := 0
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("concurrent delete error = %v", outcome.err)
		}
		if outcome.result.Missing {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("concurrent delete missing results = %d, want 1", missing)
	}
}

func TestProbeLeavesNoArtifactsAndDetectsReadOnly(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	if err := store.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("Probe() artifacts = %v, %v", entries, err)
	}

	store.ops.openat = func(int, string, int, uint32) (int, error) { return -1, syscall.EROFS }
	if err := store.Probe(context.Background()); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Probe() error = %v", err)
	}
}

func TestCanceledProbeDoesNotMutateStorage(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Probe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe(canceled) error = %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("Probe(canceled) artifacts = %v, %v", entries, err)
	}
}

func TestFailedProbeDurablyCleansArtifacts(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	originalSync := store.ops.fsync
	failed := false
	store.ops.fsync = func(fd int) error {
		if fd == store.rootFD && !failed {
			failed = true
			return syscall.EIO
		}
		return originalSync(fd)
	}
	if err := store.Probe(context.Background()); err == nil {
		t.Fatal("Probe() succeeded despite injected directory sync failure")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed Probe() artifacts = %v, %v", entries, err)
	}
}

func TestReadOnlyDirectoryRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := store.BeginOriginal(context.Background(), testOriginalKey(t), testAttempt(t)); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only BeginOriginal() error = %v", err)
	}
}

func TestConcurrentPublishHasSingleWinner(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key := testOriginalKey(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, attemptValue := range []string{testAttemptID, "44444444-5555-4666-8777-888888888888"} {
		temporary, err := store.BeginOriginal(context.Background(), key, mustAttempt(t, attemptValue))
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte{byte('a' + index)}
		_, _ = temporary.Write(payload)
		go func(temporary *Temp) {
			<-start
			_, err := temporary.Publish(context.Background(), Validation{ExpectedSize: 1})
			if errors.Is(err, ErrCollision) {
				_ = temporary.Abort(context.Background())
			}
			results <- err
		}(temporary)
	}
	close(start)
	success, collision := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrCollision) {
			collision++
		} else {
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	if success != 1 || collision != 1 {
		t.Fatalf("concurrent results success=%d collision=%d", success, collision)
	}
}

func openTestStore(t *testing.T, root string, options Options) *Store {
	t.Helper()
	store, err := Open(root, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func testOriginalKey(t *testing.T) OriginalKey {
	t.Helper()
	id, _ := ParseOriginalID(testOriginalID)
	key, err := NewOriginalKey(id, OriginalJPEG)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testRenditionKey(t *testing.T) RenditionKey {
	t.Helper()
	originalID, _ := ParseOriginalID(testOriginalID)
	targetID, _ := ParseJobTargetID(testTargetID)
	renditionID, _ := ParseRenditionID(testRenditionID)
	key, err := NewRenditionKey(originalID, targetID, renditionID, RenditionAVIF)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testAttempt(t *testing.T) AttemptID {
	return mustAttempt(t, testAttemptID)
}

func mustAttempt(t *testing.T, value string) AttemptID {
	t.Helper()
	attempt, err := ParseAttemptID(value)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func mustParseAttempt(value string) AttemptID {
	attempt, err := ParseAttemptID(value)
	if err != nil {
		panic(err)
	}
	return attempt
}

func readRelative(t *testing.T, root, key string) []byte {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func publishTestObject(t *testing.T, store *Store, key OriginalKey, payload []byte) {
	t.Helper()
	temporary, err := store.BeginOriginal(context.Background(), key, testAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Publish(context.Background(), Validation{ExpectedSize: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
}

func assertFinalMissing(t *testing.T, root, key string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final key %q exists or has unexpected stat error: %v", key, err)
	}
}

func faultAt(boundary Boundary, phase Phase, fault error) FaultInjector {
	return FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary == boundary && event.Phase == phase {
			return fault
		}
		return nil
	})
}

func faultAtDepth(boundary Boundary, phase Phase, depth int, fault error) FaultInjector {
	return FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary == boundary && event.Phase == phase && event.Depth == depth {
			return fault
		}
		return nil
	})
}

func storeRootPath(t *testing.T, store *Store) string {
	t.Helper()
	path, err := os.Readlink("/proc/self/fd/" + fmt.Sprint(store.rootFD))
	if err != nil {
		t.Fatal(err)
	}
	return path
}
