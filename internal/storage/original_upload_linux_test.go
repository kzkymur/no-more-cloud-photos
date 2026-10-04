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
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOriginalUploadSealThenPublishSealed(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	payload := []byte("sealed original")
	digest := sha256.Sum256(payload)
	if _, err := upload.Write(payload); err != nil {
		t.Fatal(err)
	}

	synced := false
	originalSync := store.ops.fsync
	store.ops.fsync = func(fd int) error {
		if fd == upload.temporary.fileFD {
			synced = true
		}
		return originalSync(fd)
	}
	readCalls := 0
	originalRead := store.ops.read
	store.ops.read = func(fd int, value []byte) (int, error) {
		if fd == upload.temporary.fileFD {
			readCalls++
		}
		return originalRead(fd, value)
	}
	validation := Validation{
		ExpectedSize:   int64(len(payload)),
		ExpectedSHA256: &digest,
		Validate: func(_ context.Context, file *os.File) error {
			if !synced {
				return errors.New("validator ran before file sync")
			}
			if _, err := file.Write([]byte("mutation")); err == nil {
				return errors.New("validator received writable file")
			}
			return nil
		},
	}
	info, err := upload.Seal(context.Background(), validation)
	if err != nil || info.Size != int64(len(payload)) || info.SHA256 != digest {
		t.Fatalf("Seal() = %+v, %v", info, err)
	}
	sealedReads := readCalls
	if sealedReads == 0 {
		t.Fatal("Seal() did not hash through the pinned descriptor")
	}
	if _, err := upload.Seal(context.Background(), validation); !errors.Is(err, ErrValidation) {
		t.Fatalf("repeated callback Seal() error = %v", err)
	}
	if _, err := upload.Seal(context.Background(), Validation{ExpectedSize: -1}); !errors.Is(err, ErrValidation) {
		t.Fatalf("incompatible Seal() error = %v", err)
	}
	if _, err := upload.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write() after Seal() error = %v", err)
	}
	key, publishedInfo, err := upload.PublishSealed(context.Background(), OriginalJPEG)
	if err != nil || publishedInfo != info {
		t.Fatalf("PublishSealed() = %q, %+v, %v", key.String(), publishedInfo, err)
	}
	if readCalls != sealedReads {
		t.Fatalf("PublishSealed() read count = %d, want %d", readCalls, sealedReads)
	}
	if got := string(readRelative(t, root, key.String())); got != string(payload) {
		t.Fatalf("published bytes = %q", got)
	}
}

func TestOriginalUploadSealRetryCompatibility(t *testing.T) {
	t.Run("callback-free validation is idempotent", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		payload := []byte("bytes")
		digest := sha256.Sum256(payload)
		_, _ = upload.Write(payload)
		validation := Validation{ExpectedSize: int64(len(payload)), ExpectedSHA256: &digest}
		first, err := upload.Seal(context.Background(), validation)
		if err != nil {
			t.Fatal(err)
		}
		second, err := upload.Seal(context.Background(), validation)
		if err != nil || second != first {
			t.Fatalf("repeated Seal() = %+v, %v", second, err)
		}
	})

	for _, test := range []struct {
		name        string
		validations func(*validationMethodCounter) (Validation, Validation)
	}{
		{
			name: "closures with the same code",
			validations: func(_ *validationMethodCounter) (Validation, Validation) {
				makeValidation := func(counter *int) Validation {
					return Validation{ExpectedSize: 5, Validate: func(context.Context, *os.File) error {
						*counter++
						return nil
					}}
				}
				firstCalls, secondCalls := 0, 0
				return makeValidation(&firstCalls), makeValidation(&secondCalls)
			},
		},
		{
			name: "method values with the same method",
			validations: func(counter *validationMethodCounter) (Validation, Validation) {
				other := &validationMethodCounter{}
				return Validation{ExpectedSize: 5, Validate: counter.validate}, Validation{ExpectedSize: 5, Validate: other.validate}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{})
			upload := beginTestOriginalUpload(t, store, testAttempt(t))
			_, _ = upload.Write([]byte("bytes"))
			counter := &validationMethodCounter{}
			first, second := test.validations(counter)
			if _, err := upload.Seal(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			if _, err := upload.Seal(context.Background(), second); !errors.Is(err, ErrValidation) {
				t.Fatalf("Seal() with a distinct callback error = %v", err)
			}
		})
	}
}

type validationMethodCounter struct {
	calls int
}

func (counter *validationMethodCounter) validate(context.Context, *os.File) error {
	counter.calls++
	return nil
}

func TestBeginOriginalUploadFailureRemovesNewUUIDDirectory(t *testing.T) {
	fault := errors.New("begin original upload fault")
	for _, point := range []struct {
		boundary Boundary
		phase    Phase
	}{
		{BoundaryDirectoryCreate, After},
		{BoundaryNewDirectorySync, Before},
		{BoundaryNewDirectorySync, After},
		{BoundaryParentDirectorySync, Before},
		{BoundaryParentDirectorySync, After},
		{BoundaryTempCreate, Before},
		{BoundaryTempCreate, After},
	} {
		t.Run(string(point.boundary)+"_"+string(point.phase), func(t *testing.T) {
			root := t.TempDir()
			injector := FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
				if event.Boundary == point.boundary && event.Phase == point.phase &&
					(event.Boundary == BoundaryTempCreate || event.Depth == 2) {
					return fault
				}
				return nil
			})
			store := openTestStore(t, root, Options{Faults: injector})
			originalID, _ := ParseOriginalID(testOriginalID)
			if _, err := store.BeginOriginalUpload(context.Background(), originalID, testAttempt(t)); !errors.Is(err, fault) {
				t.Fatalf("BeginOriginalUpload() error = %v", err)
			}
			assertOriginalUploadDirectoryMissing(t, root)
		})
	}

	t.Run("cancellation after final mkdir", func(t *testing.T) {
		root := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if event.Boundary == BoundaryDirectoryCreate && event.Phase == After && event.Depth == 2 {
				cancel()
			}
			return nil
		})})
		originalID, _ := ParseOriginalID(testOriginalID)
		if _, err := store.BeginOriginalUpload(ctx, originalID, testAttempt(t)); !errors.Is(err, context.Canceled) {
			t.Fatalf("BeginOriginalUpload() error = %v", err)
		}
		assertOriginalUploadDirectoryMissing(t, root)
	})

	t.Run("temporary open failure", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		originalOpen := store.ops.openat
		store.ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
			if flags&unix.O_CREAT != 0 {
				return -1, syscall.EIO
			}
			return originalOpen(fd, name, flags, mode)
		}
		originalID, _ := ParseOriginalID(testOriginalID)
		if _, err := store.BeginOriginalUpload(context.Background(), originalID, testAttempt(t)); err == nil {
			t.Fatal("BeginOriginalUpload() unexpectedly succeeded")
		}
		assertOriginalUploadDirectoryMissing(t, root)
	})
}

func TestBeginOriginalUploadFailurePreservesUnownedDirectories(t *testing.T) {
	fault := errors.New("stop before temporary create")
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "preexisting",
			prepare: func(t *testing.T, directory string) {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "newly nonempty",
			prepare: func(t *testing.T, directory string) {
				if err := os.WriteFile(filepath.Join(directory, "foreign"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
			if test.name == "preexisting" {
				test.prepare(t, directory)
			}
			store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
				if event.Boundary == BoundaryTempCreate && event.Phase == Before {
					if test.name != "preexisting" {
						test.prepare(t, directory)
					}
					return fault
				}
				return nil
			})})
			originalID, _ := ParseOriginalID(testOriginalID)
			if _, err := store.BeginOriginalUpload(context.Background(), originalID, testAttempt(t)); !errors.Is(err, fault) {
				t.Fatalf("BeginOriginalUpload() error = %v", err)
			}
			if stat, err := os.Stat(directory); err != nil || !stat.IsDir() {
				t.Fatalf("unowned directory was removed: %v", err)
			}
		})
	}

	t.Run("replaced", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
		moved := directory + ".moved"
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if event.Boundary == BoundaryTempCreate && event.Phase == Before {
				if err := os.Rename(directory, moved); err != nil {
					return err
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					return err
				}
				return fault
			}
			return nil
		})})
		originalID, _ := ParseOriginalID(testOriginalID)
		_, err := store.BeginOriginalUpload(context.Background(), originalID, testAttempt(t))
		if !errors.Is(err, fault) || !errors.Is(err, ErrValidation) {
			t.Fatalf("BeginOriginalUpload() error = %v", err)
		}
		if stat, statErr := os.Stat(directory); statErr != nil || !stat.IsDir() {
			t.Fatalf("replacement directory was removed: %v", statErr)
		}
	})
}

func assertOriginalUploadDirectoryMissing(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("UUID directory remains: %v", err)
	}
}

func TestOriginalUploadSealSyncFailurePreventsValidationAndPublish(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	_, _ = upload.Write([]byte("bytes"))
	validated, renamed := false, false
	originalSync := store.ops.fsync
	store.ops.fsync = func(fd int) error {
		if fd == upload.temporary.fileFD {
			return syscall.EIO
		}
		return originalSync(fd)
	}
	originalRename := store.ops.renameat2
	store.ops.renameat2 = func(oldFD int, oldName string, newFD int, newName string, flags uint) error {
		renamed = true
		return originalRename(oldFD, oldName, newFD, newName, flags)
	}
	if _, err := upload.Seal(context.Background(), Validation{ExpectedSize: 5, Validate: func(context.Context, *os.File) error {
		validated = true
		return nil
	}}); !errors.Is(err, ErrDurability) {
		t.Fatalf("Seal(fsync failure) error = %v", err)
	}
	if validated {
		t.Fatal("validation callback ran after failed fsync")
	}
	if _, _, err := upload.PublishSealed(context.Background(), OriginalJPEG); !errors.Is(err, ErrValidation) {
		t.Fatalf("PublishSealed(unsealed) error = %v", err)
	}
	if renamed {
		t.Fatal("failed Seal reached rename")
	}
	assertFinalMissing(t, root, testOriginalKey(t).String())
}

func TestOriginalUploadLeafReplacementBeforeRenameCannotPublish(t *testing.T) {
	root := t.TempDir()
	var upload *OriginalUpload
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary != BoundaryRename || event.Phase != Before {
			return nil
		}
		if err := unix.Unlinkat(upload.temporary.parentFD, upload.temporary.tempName, 0); err != nil {
			return err
		}
		fd, err := unix.Openat(upload.temporary.parentFD, upload.temporary.tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := unix.Write(fd, []byte("replacement"))
		closeErr := unix.Close(fd)
		return errors.Join(writeErr, closeErr)
	})})
	upload = beginTestOriginalUpload(t, store, testAttempt(t))
	_, _ = upload.Write([]byte("original"))
	if _, err := upload.Seal(context.Background(), Validation{ExpectedSize: 8}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := upload.PublishSealed(context.Background(), OriginalJPEG); !errors.Is(err, ErrValidation) {
		t.Fatalf("PublishSealed(replaced leaf) error = %v", err)
	}
	assertFinalMissing(t, root, testOriginalKey(t).String())
}

func TestOriginalUploadAbortDirectoryOwnership(t *testing.T) {
	t.Run("new empty directory removed", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
		if err := upload.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new UUID directory remains: %v", err)
		}
	})

	t.Run("preexisting directory retained", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		store := openTestStore(t, root, Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		if err := upload.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if stat, err := os.Stat(directory); err != nil || !stat.IsDir() {
			t.Fatalf("preexisting UUID directory removed: %v", err)
		}
	})

	t.Run("nonempty directory retained", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		first := beginTestOriginalUpload(t, store, testAttempt(t))
		second := beginTestOriginalUpload(t, store, mustAttempt(t, "44444444-5555-4666-8777-888888888888"))
		directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
		if err := first.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("nonempty UUID directory removed: %v", err)
		}
		if err := second.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("directory not owned by second upload was removed: %v", err)
		}
	})
}

func TestOriginalUploadProbeAndCleanupFaultBoundaries(t *testing.T) {
	fault := errors.New("injected original-upload fault")
	for _, phase := range []Phase{Before, After} {
		t.Run("read_only_probe_"+string(phase), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), Options{Faults: faultAt(BoundaryReadOnlyProbe, phase, fault)})
			upload := beginTestOriginalUpload(t, store, testAttempt(t))
			called := false
			err := upload.UseReadOnlyFile(func(*os.File) error {
				called = true
				return nil
			})
			if !errors.Is(err, fault) {
				t.Fatalf("UseReadOnlyFile() error = %v", err)
			}
			if called != (phase == After) {
				t.Fatalf("callback called = %v at %s boundary", called, phase)
			}
			store.faults = nil
			if err := upload.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, point := range []struct {
		boundary Boundary
		phase    Phase
		removed  bool
	}{
		{BoundaryUploadDirectoryDelete, Before, false},
		{BoundaryUploadDirectoryDelete, After, true},
		{BoundaryUploadDirectorySync, Before, true},
		{BoundaryUploadDirectorySync, After, true},
	} {
		t.Run(string(point.boundary)+"_"+string(point.phase), func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root, Options{Faults: faultAt(point.boundary, point.phase, fault)})
			upload := beginTestOriginalUpload(t, store, testAttempt(t))
			err := upload.Abort(context.Background())
			if !errors.Is(err, fault) {
				t.Fatalf("Abort() error = %v", err)
			}
			directory := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
			_, statErr := os.Stat(directory)
			if point.removed != errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("UUID directory removed = %v, stat error = %v", point.removed, statErr)
			}
		})
	}
}

func TestOriginalUploadDirectoryCleanupErrorIsClassified(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	originalUnlink := store.ops.unlinkat
	store.ops.unlinkat = func(fd int, name string, flags int) error {
		if flags&unix.AT_REMOVEDIR != 0 {
			return syscall.EROFS
		}
		return originalUnlink(fd, name, flags)
	}
	if err := upload.Abort(context.Background()); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Abort(UUID cleanup EROFS) error = %v", err)
	}
	if !upload.temporary.finished {
		t.Fatal("cleanup failure retained upload descriptors")
	}
	if _, err := os.Stat(filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)); err != nil {
		t.Fatalf("failed cleanup did not retain UUID directory: %v", err)
	}
}

func TestOriginalUploadPublishesEveryClosedExtensionWithLateKey(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	originalID, err := ParseOriginalID(testOriginalID)
	if err != nil {
		t.Fatal(err)
	}
	extensions := []OriginalExtension{
		OriginalJPEG, OriginalPNG, OriginalGIF, OriginalHEIC, OriginalHEIF, OriginalWebP,
		OriginalBMP, OriginalMP4, OriginalMOV, OriginalDNG, OriginalNEF, OriginalCR2,
		OriginalCR3, OriginalARW, OriginalRAF, OriginalORF, OriginalRW2,
	}
	if len(extensions) != len(originalExtensions) {
		t.Fatalf("tested extensions = %d, registered = %d", len(extensions), len(originalExtensions))
	}

	for index, extension := range extensions {
		extension := extension
		t.Run(originalExtensions[extension], func(t *testing.T) {
			upload, err := store.BeginOriginalUpload(context.Background(), originalID, uploadAttempt(t, index))
			if err != nil {
				t.Fatal(err)
			}
			if upload.temporary.finalName != "" || upload.temporary.tempName != ".original."+uploadAttempt(t, index).String()+".tmp" {
				t.Fatalf("staged names = temp %q, final %q", upload.temporary.tempName, upload.temporary.finalName)
			}
			parentPath, err := os.Readlink("/proc/self/fd/" + fmt.Sprint(upload.temporary.parentFD))
			if err != nil {
				t.Fatal(err)
			}
			wantParent := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID)
			if parentPath != wantParent {
				t.Fatalf("staged parent = %q, want %q", parentPath, wantParent)
			}

			payload := []byte("payload-" + originalExtensions[extension])
			if _, err := upload.Write(payload); err != nil {
				t.Fatal(err)
			}
			key, info, err := upload.Publish(context.Background(), extension, Validation{ExpectedSize: int64(len(payload))})
			if err != nil {
				t.Fatal(err)
			}
			if key.originalID != originalID || key.extension != extension {
				t.Fatalf("late key = %#v", key)
			}
			if info.Size != int64(len(payload)) || string(readRelative(t, root, key.String())) != string(payload) {
				t.Fatalf("published info/bytes = %+v, %q", info, readRelative(t, root, key.String()))
			}
		})
	}
}

func TestOriginalUploadReadOnlyCallbackIsScopedAndOffsetIndependent(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	if _, err := upload.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	var retained *os.File
	if err := upload.UseReadOnlyFile(func(file *os.File) error {
		retained = file
		if file.Name() != "storage-original-input" || strings.Contains(file.Name(), testOriginalID) || filepath.IsAbs(file.Name()) {
			t.Fatalf("callback file name exposed storage path: %q", file.Name())
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil {
			return err
		}
		if flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatalf("callback descriptor flags = %#x", flags)
		}
		if _, err := file.Write([]byte("mutation")); err == nil {
			t.Fatal("read-only callback descriptor accepted a write")
		}
		buffer := make([]byte, 3)
		if _, err := io.ReadFull(file, buffer); err != nil || string(buffer) != "bef" {
			t.Fatalf("callback read = %q, %v", buffer, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained callback descriptor Stat() error = %v", err)
	}
	if _, err := upload.Write([]byte("-after")); err != nil {
		t.Fatal(err)
	}
	key, _, err := upload.Publish(context.Background(), OriginalJPEG, Validation{ExpectedSize: int64(len("before-after"))})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := store.OpenOriginal(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || string(got) != "before-after" {
		t.Fatalf("published bytes = %q, %v", got, err)
	}
}

func TestOriginalUploadReadOnlyCallbackErrorAndPanicCloseDescriptors(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		_, _ = upload.Write([]byte("bytes"))
		callbackErr := errors.New("probe failed")
		var retained *os.File
		err := upload.UseReadOnlyFile(func(file *os.File) error {
			retained = file
			return callbackErr
		})
		if !errors.Is(err, callbackErr) {
			t.Fatalf("UseReadOnlyFile() error = %v", err)
		}
		if _, err := retained.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("callback descriptor remained open: %v", err)
		}
		if err := upload.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("panic", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		_, _ = upload.Write([]byte("bytes"))
		var retained *os.File
		func() {
			defer func() {
				if recovered := recover(); recovered != "probe panic" {
					t.Fatalf("recovered panic = %v", recovered)
				}
			}()
			_ = upload.UseReadOnlyFile(func(file *os.File) error {
				retained = file
				panic("probe panic")
			})
		}()
		if _, err := retained.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("panic callback descriptor remained open: %v", err)
		}
		if _, _, err := upload.Publish(context.Background(), OriginalPNG, Validation{ExpectedSize: 5}); err != nil {
			t.Fatalf("read-only callback panic poisoned upload: %v", err)
		}
	})
}

func TestOriginalUploadCallbackSerializesPublish(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	_, _ = upload.Write([]byte("bytes"))
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- upload.UseReadOnlyFile(func(*os.File) error {
			close(callbackEntered)
			<-releaseCallback
			return nil
		})
	}()
	<-callbackEntered

	renameEntered := make(chan struct{}, 1)
	originalRename := store.ops.renameat2
	store.ops.renameat2 = func(oldFD int, oldName string, newFD int, newName string, flags uint) error {
		renameEntered <- struct{}{}
		return originalRename(oldFD, oldName, newFD, newName, flags)
	}
	publishDone := make(chan error, 1)
	go func() {
		_, _, err := upload.Publish(context.Background(), OriginalJPEG, Validation{ExpectedSize: 5})
		publishDone <- err
	}()
	select {
	case <-renameEntered:
		t.Fatal("publish renamed while read-only callback was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCallback)
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-renameEntered:
	default:
		t.Fatal("publish did not rename after callback returned")
	}
}

func TestOriginalUploadAbortCollisionAndNoWritableLeak(t *testing.T) {
	t.Run("abort", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		tempPath := filepath.Join(root, "originals", testOriginalID[0:2], testOriginalID, upload.temporary.tempName)
		if err := upload.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("aborted temp stat error = %v", err)
		}
		if _, err := unix.FcntlInt(uintptr(upload.temporary.fileFD), unix.F_GETFD, 0); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("aborted writable descriptor remained open: %v", err)
		}
	})

	t.Run("collision", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key := testOriginalKey(t)
		publishTestObject(t, store, key, []byte("existing"))
		upload := beginTestOriginalUpload(t, store, mustAttempt(t, "44444444-5555-4666-8777-888888888888"))
		_, _ = upload.Write([]byte("replacement"))
		lateKey, _, err := upload.Publish(context.Background(), OriginalJPEG, Validation{ExpectedSize: int64(len("replacement"))})
		if !errors.Is(err, ErrCollision) || lateKey.String() != key.String() {
			t.Fatalf("collision Publish() = %q, %v", lateKey.String(), err)
		}
		if err := upload.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := string(readRelative(t, root, key.String())); got != "existing" {
			t.Fatalf("collision replaced final bytes: %q", got)
		}
	})

	t.Run("success closes writer", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), Options{})
		upload := beginTestOriginalUpload(t, store, testAttempt(t))
		_, _ = upload.Write([]byte("bytes"))
		if _, _, err := upload.Publish(context.Background(), OriginalJPEG, Validation{ExpectedSize: 5}); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(uintptr(upload.temporary.fileFD), unix.F_GETFD, 0); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("published writable descriptor remained open: %v", err)
		}
		if _, err := upload.Write([]byte("late write")); !errors.Is(err, ErrClosed) {
			t.Fatalf("Write() after publish error = %v", err)
		}
		if err := upload.UseReadOnlyFile(func(*os.File) error { return nil }); !errors.Is(err, ErrClosed) {
			t.Fatalf("UseReadOnlyFile() after publish error = %v", err)
		}
	})
}

func TestOriginalUploadRejectsInvalidInputsAndSymlinkEscape(t *testing.T) {
	store := openTestStore(t, t.TempDir(), Options{})
	if _, err := store.BeginOriginalUpload(context.Background(), OriginalID{}, testAttempt(t)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("zero OriginalID error = %v", err)
	}
	originalID, _ := ParseOriginalID(testOriginalID)
	if _, err := store.BeginOriginalUpload(context.Background(), originalID, AttemptID{}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("zero AttemptID error = %v", err)
	}
	upload := beginTestOriginalUpload(t, store, testAttempt(t))
	if key, _, err := upload.Publish(context.Background(), OriginalExtension(255), Validation{ExpectedSize: 0}); !errors.Is(err, ErrInvalidKey) || key != (OriginalKey{}) {
		t.Fatalf("invalid extension Publish() = %#v, %v", key, err)
	}
	if err := upload.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "originals")); err != nil {
		t.Fatal(err)
	}
	symlinkStore := openTestStore(t, root, Options{})
	if _, err := symlinkStore.BeginOriginalUpload(context.Background(), originalID, testAttempt(t)); !errors.Is(err, ErrSymlink) && !errors.Is(err, ErrUnexpectedType) {
		t.Fatalf("symlink BeginOriginalUpload() error = %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("staged upload escaped through symlink: %v, %v", entries, err)
	}
}

func beginTestOriginalUpload(t *testing.T, store *Store, attempt AttemptID) *OriginalUpload {
	t.Helper()
	originalID, err := ParseOriginalID(testOriginalID)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.BeginOriginalUpload(context.Background(), originalID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return upload
}

func uploadAttempt(t *testing.T, index int) AttemptID {
	t.Helper()
	return mustAttempt(t, fmt.Sprintf("33333333-4444-4555-8666-%012x", index+1))
}
