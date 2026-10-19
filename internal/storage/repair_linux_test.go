//go:build linux

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRepairQuarantineStagesExactAuthority(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key, destination := testOriginalKey(t), testQuarantineKey(t)
	payload := []byte("repair-authority-bytes")
	publishTestObject(t, store, key, payload)
	if err := unix.Chmod(filepath.Join(root, filepath.FromSlash(key.String())), 0o4644); err != nil {
		t.Fatal(err)
	}
	expectation := repairTestExpectation(t, root, key.String(), payload)
	operation, progress, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
	if err != nil || progress.Disposition != RepairSourceReady || progress.RenameObserved {
		t.Fatalf("BeginRepairOriginal() = %+v, %v", progress, err)
	}
	progress, err = operation.Revalidate(context.Background())
	if err != nil || !progress.Revalidated || progress.ObservedSHA256 != expectation.SHA256 {
		t.Fatalf("Revalidate() = %+v, %v", progress, err)
	}
	progress, err = operation.Rename(context.Background())
	if err != nil || !progress.RenameObserved || !progress.RenamePerformedByThisCall {
		t.Fatalf("Rename() = %+v, %v", progress, err)
	}
	progress, err = operation.SyncSourceDirectory(context.Background())
	if err != nil || !progress.SourceDirectoryDurable || progress.DestinationDirectoryDurable {
		t.Fatalf("SyncSourceDirectory() = %+v, %v", progress, err)
	}
	progress, err = operation.SyncDestinationDirectory(context.Background())
	if err != nil || !progress.DestinationDirectoryDurable {
		t.Fatalf("SyncDestinationDirectory() = %+v, %v", progress, err)
	}
	progress, err = operation.VerifyCanonicalDestination(context.Background())
	if err != nil || !progress.CanonicalDestinationVerified {
		t.Fatalf("VerifyCanonicalDestination() = %+v, %v", progress, err)
	}
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	assertFinalMissing(t, root, key.String())
	if got := readRelative(t, root, destination.String()); string(got) != string(payload) {
		t.Fatalf("destination = %q", got)
	}
	var destinationStat unix.Stat_t
	if err := unix.Stat(filepath.Join(root, filepath.FromSlash(destination.String())), &destinationStat); err != nil || destinationStat.Mode&0o7777 != 0o600 {
		t.Fatalf("destination mode = %#o, %v", destinationStat.Mode, err)
	}
}

func TestRepairInspectionHasZeroMutationAndRejectsManifestDrift(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, Options{})
	key, destination := testOriginalKey(t), testQuarantineKey(t)
	payload := []byte("same-size-original")
	publishTestObject(t, store, key, payload)
	leaf := filepath.Join(root, filepath.FromSlash(key.String()))
	reportTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(leaf, reportTime, reportTime); err != nil {
		t.Fatal(err)
	}
	expectation := repairTestExpectation(t, root, key.String(), payload)
	progress, err := store.InspectRepairOriginal(context.Background(), key, destination, expectation)
	if err != nil || progress.Disposition != RepairSourceReady {
		t.Fatalf("InspectRepairOriginal() = %+v, %v", progress, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".quarantine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created quarantine: %v", err)
	}
	replacement := leaf + ".replacement"
	if err := os.WriteFile(replacement, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, leaf); err != nil {
		t.Fatal(err)
	}
	_, err = store.InspectRepairOriginal(context.Background(), key, destination, expectation)
	var repairErr *RepairError
	if !errors.As(err, &repairErr) || repairErr.Cause != RepairCauseSourceChanged {
		t.Fatalf("same-content replacement error = %#v", err)
	}
}

func TestRepairRejectsHardlinkAndRecoversExactDestination(t *testing.T) {
	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("hardlink-authority")
		publishTestObject(t, store, key, payload)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		if err := os.Link(leaf, leaf+".linked"); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination,
			repairTestExpectation(t, root, key.String(), payload))
		var repairErr *RepairError
		if !errors.As(err, &repairErr) || repairErr.Cause != RepairCauseWrongType {
			t.Fatalf("hardlink error = %#v", err)
		}
	})

	t.Run("same attempt recovery", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("recovery-authority")
		publishTestObject(t, store, key, payload)
		expectation := repairTestExpectation(t, root, key.String(), payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err = operation.Rename(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = operation.Close(); !errors.Is(err, ErrOutcomeUncertain) {
			t.Fatalf("Close() = %v", err)
		}
		if err := unix.Chmod(filepath.Join(root, filepath.FromSlash(destination.String())), 0o2600); err != nil {
			t.Fatal(err)
		}

		recovered, progress, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		if err != nil || progress.Disposition != RepairDestinationExact || !progress.RenameObserved || progress.RenamePerformedByThisCall {
			t.Fatalf("recovery begin = %+v, %v", progress, err)
		}
		if _, err = recovered.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		var recoveredStat unix.Stat_t
		if err := unix.Stat(filepath.Join(root, filepath.FromSlash(destination.String())), &recoveredStat); err != nil || recoveredStat.Mode&0o7777 != 0o600 {
			t.Fatalf("recovered destination mode = %#o, %v", recoveredStat.Mode, err)
		}
		if progress, err = recovered.Rename(context.Background()); err != nil || progress.RenamePerformedByThisCall {
			t.Fatalf("recovery rename = %+v, %v", progress, err)
		}
		if _, err = recovered.SyncSourceDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err = recovered.SyncDestinationDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err = recovered.VerifyCanonicalDestination(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = recovered.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRepairPostRenameFaultPreservesTruthfulProgress(t *testing.T) {
	root := t.TempDir()
	fault := errors.New("after source fsync response loss")
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary == BoundaryRepairSourceDirectorySync && event.Phase == After {
			return fault
		}
		return nil
	})})
	key, destination := testOriginalKey(t), testQuarantineKey(t)
	payload := []byte("fault-authority")
	publishTestObject(t, store, key, payload)
	operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination,
		repairTestExpectation(t, root, key.String(), payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operation.Revalidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.Rename(context.Background()); err != nil {
		t.Fatal(err)
	}
	progress, err := operation.SyncSourceDirectory(context.Background())
	if !errors.Is(err, fault) || !progress.SourceDirectoryDurable {
		t.Fatalf("source sync = %+v, %v", progress, err)
	}
	progress, err = operation.SyncDestinationDirectory(context.Background())
	if err != nil || !progress.DestinationDirectoryDurable {
		t.Fatalf("destination sync after source response loss = %+v, %v", progress, err)
	}
	if _, err = operation.VerifyCanonicalDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRepairMetadataDurabilityFailureIsPhysicalEffectUncertainty(t *testing.T) {
	root := t.TempDir()
	fault := errors.New("metadata fsync response lost")
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if event.Boundary == BoundaryRepairDestinationMetadataSync && event.Phase == After {
			return fault
		}
		return nil
	})})
	key, destination := testOriginalKey(t), testQuarantineKey(t)
	payload := []byte("metadata-durability")
	publishTestObject(t, store, key, payload)
	operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	if _, err = operation.Revalidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	progress, err := operation.Rename(context.Background())
	assertRepairCause(t, err, RepairCauseDurability)
	if !progress.RenameObserved {
		t.Fatalf("metadata failure lost physical-effect evidence: %+v", progress)
	}
}

func TestRepairRejectsTypeDigestAndLateLinkChanges(t *testing.T) {
	t.Run("symlink source", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("symlink-authority")
		publishTestObject(t, store, key, payload)
		expectation := repairTestExpectation(t, root, key.String(), payload)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", leaf); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		assertRepairCause(t, err, RepairCauseWrongType)
	})

	t.Run("fifo source", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("fifo-authority")
		publishTestObject(t, store, key, payload)
		expectation := repairTestExpectation(t, root, key.String(), payload)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(leaf, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		assertRepairCause(t, err, RepairCauseWrongType)
	})

	t.Run("same size wrong destination", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("destination-good")
		wrong := []byte("destination-evil")
		if len(wrong) != len(payload) {
			t.Fatal("payload length mismatch")
		}
		publishTestObject(t, store, key, payload)
		expectation := repairTestExpectation(t, root, key.String(), payload)
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(key.String()))); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".quarantine"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(destination.String())), wrong, 0o600); err != nil {
			t.Fatal(err)
		}
		_, progress, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		assertRepairCause(t, err, RepairCauseQuarantineMismatch)
		if progress.SourcePresent || !progress.DestinationPresent {
			t.Fatalf("presence evidence = %+v", progress)
		}
	})

	t.Run("hardlink added immediately before rename", func(t *testing.T) {
		root := t.TempDir()
		key := testOriginalKey(t)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		hashPasses := 0
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if event.Boundary == BoundaryRepairSourceHash && event.Phase == Before {
				hashPasses++
				if hashPasses == 3 {
					return os.Link(leaf, leaf+".late-link")
				}
			}
			return nil
		})})
		destination := testQuarantineKey(t)
		payload := []byte("late-link-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		progress, err := operation.Rename(context.Background())
		assertRepairCause(t, err, RepairCauseSourceChanged)
		if progress.RenameObserved {
			t.Fatalf("late hardlink moved source: %+v", progress)
		}
		if _, err := os.Stat(leaf); err != nil {
			t.Fatalf("source missing after rejected rename: %v", err)
		}
	})
}

func TestRepairHashFailuresKeepTruthfulCause(t *testing.T) {
	for _, test := range []struct {
		name  string
		fault error
		want  RepairCause
	}{
		{name: "cancelled", fault: context.Canceled, want: RepairCauseCancelled},
		{name: "permission", fault: unix.EACCES, want: RepairCausePermission},
		{name: "read only", fault: unix.EROFS, want: RepairCauseReadOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			key, destination := testOriginalKey(t), testQuarantineKey(t)
			injected := false
			store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
				if !injected && event.Boundary == BoundaryRepairSourceHash && event.Phase == Before {
					injected = true
					return test.fault
				}
				return nil
			})})
			payload := []byte("hash-cause")
			publishTestObject(t, store, key, payload)
			_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
			assertRepairCause(t, err, test.want)
		})
		t.Run("destination "+test.name, func(t *testing.T) {
			root := t.TempDir()
			key, destination := testOriginalKey(t), testQuarantineKey(t)
			injectDestination := false
			store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
				if injectDestination && event.Boundary == BoundaryRepairDestinationHash && event.Phase == Before {
					return test.fault
				}
				return nil
			})})
			payload := []byte("destination-hash-cause")
			publishTestObject(t, store, key, payload)
			expectation := repairTestExpectation(t, root, key.String(), payload)
			operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = operation.Revalidate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err = operation.Rename(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = operation.Close()
			injectDestination = true
			_, _, err = store.BeginRepairOriginal(context.Background(), key, destination, expectation)
			assertRepairCause(t, err, test.want)
		})
	}
}

func TestRepairPostRevalidationCancellationRemainsCancelled(t *testing.T) {
	root := t.TempDir()
	cancelAtBoundary := false
	store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
		if cancelAtBoundary && event.Boundary == BoundaryRepairRevalidate && event.Phase == After {
			return context.Canceled
		}
		return nil
	})})
	key, destination := testOriginalKey(t), testQuarantineKey(t)
	payload := []byte("revalidation-cancel")
	publishTestObject(t, store, key, payload)
	operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	cancelAtBoundary = true
	_, err = operation.Revalidate(context.Background())
	assertRepairCause(t, err, RepairCauseCancelled)
}

func TestRepairLaterSourceOperationalFailuresRemainRetryable(t *testing.T) {
	t.Run("inspect open read only", func(t *testing.T) {
		root := t.TempDir()
		inject := false
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if inject && event.Boundary == BoundaryRepairSourceOpen && event.Phase == Before {
				return unix.EROFS
			}
			return nil
		})})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("inspect-open")
		publishTestObject(t, store, key, payload)
		expectation := repairTestExpectation(t, root, key.String(), payload)
		inject = true
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
		assertRepairCause(t, err, RepairCauseReadOnly)
	})

	t.Run("inspect descriptor stat io", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if event.Boundary == BoundaryRepairSourceDescriptorStat && event.Phase == Before {
				return unix.EIO
			}
			return nil
		})})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("inspect-descriptor-stat")
		publishTestObject(t, store, key, payload)
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseInternal)
	})

	t.Run("revalidate open permission", func(t *testing.T) {
		root := t.TempDir()
		failOpen := false
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if failOpen && event.Boundary == BoundaryRepairSourceOpen && event.Phase == Before {
				return unix.EACCES
			}
			return nil
		})})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("revalidate-open")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		failOpen = true
		_, err = operation.Revalidate(context.Background())
		assertRepairCause(t, err, RepairCausePermission)
	})

	t.Run("final open descriptor exhaustion", func(t *testing.T) {
		root := t.TempDir()
		failOpen := false
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if failOpen && event.Boundary == BoundaryRepairSourceOpen && event.Phase == Before {
				return unix.EMFILE
			}
			return nil
		})})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("final-open")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		failOpen = true
		_, err = operation.Rename(context.Background())
		assertRepairCause(t, err, RepairCauseInternal)
	})

	t.Run("final descriptor stat io", func(t *testing.T) {
		root := t.TempDir()
		failStat := false
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if failStat && event.Boundary == BoundaryRepairSourceDescriptorStat && event.Phase == Before {
				return unix.EIO
			}
			return nil
		})})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("final-descriptor-stat")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		failStat = true
		_, err = operation.Rename(context.Background())
		assertRepairCause(t, err, RepairCauseInternal)
	})

	for _, test := range []struct {
		name     string
		boundary Boundary
		fault    error
		want     RepairCause
	}{
		{name: "descriptor stat io", boundary: BoundaryRepairSourceDescriptorStat, fault: unix.EIO, want: RepairCauseInternal},
		{name: "leaf stat permission", boundary: BoundaryRepairSourceLeafStat, fault: unix.EACCES, want: RepairCausePermission},
		{name: "leaf stat stale is retryable", boundary: BoundaryRepairSourceLeafStat, fault: unix.ESTALE, want: RepairCauseInternal},
		{name: "leaf stat absence is changed", boundary: BoundaryRepairSourceLeafStat, fault: unix.ENOENT, want: RepairCauseSourceChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			failStat := false
			store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
				if failStat && event.Boundary == test.boundary && event.Phase == Before {
					return test.fault
				}
				return nil
			})})
			key, destination := testOriginalKey(t), testQuarantineKey(t)
			payload := []byte("post-hash-stat")
			publishTestObject(t, store, key, payload)
			operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
			if err != nil {
				t.Fatal(err)
			}
			defer operation.Close()
			failStat = true
			_, err = operation.Revalidate(context.Background())
			assertRepairCause(t, err, test.want)
		})
	}
}

func TestRepairRejectsCanonicalNamespaceReplacementAndPreRenameCancellation(t *testing.T) {
	t.Run("dry run preserves source access time", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("dry-run-no-atime")
		publishTestObject(t, store, key, payload)
		path := filepath.Join(root, filepath.FromSlash(key.String()))
		old := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		expectation := repairTestExpectation(t, root, key.String(), payload)
		var before unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_ATIME, &before); err != nil {
			t.Fatal(err)
		}
		progress, err := store.InspectRepairOriginal(context.Background(), key, destination, expectation)
		if err != nil || progress.Disposition != RepairSourceReady {
			t.Fatalf("dry-run inspection = %+v, %v", progress, err)
		}
		var after unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_ATIME, &after); err != nil {
			t.Fatal(err)
		}
		if before.Atime != after.Atime {
			t.Fatalf("dry-run changed atime from %+v to %+v", before.Atime, after.Atime)
		}
	})

	t.Run("insecure existing quarantine", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("private-quarantine")
		publishTestObject(t, store, key, payload)
		if err := os.Mkdir(filepath.Join(root, ".quarantine"), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, ".quarantine"), 0o777); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("insecure root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("insecure-root")
		publishTestObject(t, store, key, payload)
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("insecure source ancestor", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("insecure-ancestor")
		publishTestObject(t, store, key, payload)
		if err := os.Chmod(filepath.Join(root, "originals"), 0o777); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("source ancestor replaced", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("ancestor-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		originals := filepath.Join(root, "originals")
		if err := os.Rename(originals, originals+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(originals, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err = operation.Revalidate(context.Background())
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("source ancestor permissions changed", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("ancestor-mode-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if err := os.Chmod(filepath.Join(root, "originals"), 0o777); err != nil {
			t.Fatal(err)
		}
		_, err = operation.Revalidate(context.Background())
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("root permissions changed", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("root-mode-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		_, err = operation.Revalidate(context.Background())
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("quarantine replaced", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("quarantine-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		quarantine := filepath.Join(root, ".quarantine")
		if err := os.Rename(quarantine, quarantine+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(quarantine, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err = operation.Revalidate(context.Background())
		assertRepairCause(t, err, RepairCauseNamespaceChanged)
	})

	t.Run("cancelled before rename", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("cancel-authority")
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		progress, err := operation.Rename(ctx)
		assertRepairCause(t, err, RepairCauseCancelled)
		if progress.RenameObserved {
			t.Fatalf("cancelled rename mutated storage: %+v", progress)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key.String()))); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(destination.String()))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination exists after cancellation: %v", err)
		}
	})

	t.Run("leaf swapped after final validation", func(t *testing.T) {
		root := t.TempDir()
		key := testOriginalKey(t)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		payload := []byte("same-content-leaf-swap")
		swapped := false
		store := openTestStore(t, root, Options{Faults: FaultInjectorFunc(func(_ context.Context, event FaultEvent) error {
			if event.Boundary != BoundaryRepairRenameCommit || event.Phase != Before || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(leaf, leaf+".pinned"); err != nil {
				return err
			}
			return os.WriteFile(leaf, payload, 0o600)
		})})
		destination := testQuarantineKey(t)
		publishTestObject(t, store, key, payload)
		operation, _, err := store.BeginRepairOriginal(context.Background(), key, destination, repairTestExpectation(t, root, key.String(), payload))
		if err != nil {
			t.Fatal(err)
		}
		defer operation.Close()
		if _, err = operation.Revalidate(context.Background()); err != nil {
			t.Fatal(err)
		}
		progress, err := operation.Rename(context.Background())
		assertRepairCause(t, err, RepairCauseSourceChanged)
		if progress.RenameObserved || progress.DestinationPresent {
			t.Fatalf("swapped inode was not rolled back: %+v", progress)
		}
		if _, err := os.Stat(leaf); err != nil {
			t.Fatalf("replacement leaf was not restored: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(destination.String()))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("swapped inode remained quarantined: %v", err)
		}
	})
}

func TestRepairRejectsRealBindMountBoundaries(t *testing.T) {
	t.Run("source ancestor", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("bind-boundary")
		publishTestObject(t, store, key, payload)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		parent := filepath.Dir(leaf)
		if err := unix.Mount(parent, parent, "", unix.MS_BIND, ""); err != nil {
			if errors.Is(err, unix.EPERM) {
				t.Skip("requires privileged mount namespace")
			}
			t.Fatal(err)
		}
		defer unix.Unmount(parent, unix.MNT_DETACH)
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination,
			repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseCrossDevice)
	})

	t.Run("source file", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("file-bind-boundary")
		publishTestObject(t, store, key, payload)
		leaf := filepath.Join(root, filepath.FromSlash(key.String()))
		bindSource := filepath.Join(root, "bind-source")
		if err := os.WriteFile(bindSource, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(bindSource, leaf, "", unix.MS_BIND, ""); err != nil {
			if errors.Is(err, unix.EPERM) {
				t.Skip("requires privileged mount namespace")
			}
			t.Fatal(err)
		}
		defer unix.Unmount(leaf, unix.MNT_DETACH)
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination,
			repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseCrossDevice)
	})

	t.Run("quarantine directory", func(t *testing.T) {
		root := t.TempDir()
		store := openTestStore(t, root, Options{})
		key, destination := testOriginalKey(t), testQuarantineKey(t)
		payload := []byte("quarantine-bind-boundary")
		publishTestObject(t, store, key, payload)
		quarantine := filepath.Join(root, ".quarantine")
		bindSource := filepath.Join(root, "quarantine-bind-source")
		if err := os.Mkdir(quarantine, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(bindSource, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(bindSource, quarantine, "", unix.MS_BIND, ""); err != nil {
			if errors.Is(err, unix.EPERM) {
				t.Skip("requires privileged mount namespace")
			}
			t.Fatal(err)
		}
		defer unix.Unmount(quarantine, unix.MNT_DETACH)
		_, _, err := store.BeginRepairOriginal(context.Background(), key, destination,
			repairTestExpectation(t, root, key.String(), payload))
		assertRepairCause(t, err, RepairCauseCrossDevice)
	})
}

func assertRepairCause(t *testing.T, err error, want RepairCause) {
	t.Helper()
	var repairErr *RepairError
	if !errors.As(err, &repairErr) || repairErr.Cause != want {
		t.Fatalf("repair error = %#v, want cause %q", err, want)
	}
}

func repairTestExpectation(t *testing.T, root, relative string, payload []byte) RepairExpectation {
	t.Helper()
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filepath.Join(root, filepath.FromSlash(relative)), unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MTIME|unix.STATX_CTIME, &stat); err != nil {
		t.Fatal(err)
	}
	return RepairExpectation{Size: int64(len(payload)), SHA256: sha256.Sum256(payload),
		MTime: repairIdentityTime(stat.Mtime.Sec, stat.Mtime.Nsec), CTime: repairIdentityTime(stat.Ctime.Sec, stat.Ctime.Nsec)}
}
