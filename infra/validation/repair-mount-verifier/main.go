//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"golang.org/x/sys/unix"
)

func fail(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}

func main() {
	root, err := os.MkdirTemp("", "nmcp-repair-late-bind-")
	if err != nil {
		fail("create root: %v", err)
	}
	defer os.RemoveAll(root)

	key, err := storage.ParseOriginalKey("originals/00/00000000-0000-4000-8000-000000000001/original.jpg")
	if err != nil {
		fail("parse source key: %v", err)
	}
	destination, err := storage.ParseQuarantineKey(".quarantine/11111111-1111-4111-8111-111111111111")
	if err != nil {
		fail("parse destination key: %v", err)
	}
	payload := []byte("canonical-repair-payload")
	source := filepath.Join(root, filepath.FromSlash(key.String()))
	if err = os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		fail("create source parent: %v", err)
	}
	if err = os.WriteFile(source, payload, 0o600); err != nil {
		fail("write source: %v", err)
	}
	var stat unix.Stat_t
	if err = unix.Stat(source, &stat); err != nil {
		fail("stat source: %v", err)
	}
	expectation := storage.RepairExpectation{
		Size: int64(len(payload)), SHA256: sha256.Sum256(payload),
		MTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec),
		CTime: time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec),
	}
	store, err := storage.Open(root, storage.Options{})
	if err != nil {
		fail("open store: %v", err)
	}
	defer store.Close()
	operation, progress, err := store.BeginRepairOriginal(context.Background(), key, destination, expectation)
	if err != nil || operation == nil || progress.Disposition != storage.RepairSourceReady {
		fail("begin repair: progress=%+v error=%v", progress, err)
	}
	defer operation.Close()

	decoy := filepath.Join(root, "same-filesystem-decoy")
	if err = os.WriteFile(decoy, payload, 0o600); err != nil {
		fail("write decoy: %v", err)
	}
	var decoyStat unix.Stat_t
	if err = unix.Stat(decoy, &decoyStat); err != nil || decoyStat.Dev != stat.Dev {
		fail("decoy is not on source filesystem: %v", err)
	}
	if err = unix.Mount(decoy, source, "", unix.MS_BIND, ""); err != nil {
		fail("late same-filesystem file bind mount: %v", err)
	}
	mounted := true
	defer func() {
		if mounted {
			_ = unix.Unmount(source, unix.MNT_DETACH)
		}
	}()
	_, err = operation.Revalidate(context.Background())
	var repairErr *storage.RepairError
	if !errors.As(err, &repairErr) || repairErr.Cause != storage.RepairCauseSourceChanged ||
		repairErr.Progress.Revalidated || repairErr.Progress.RenameObserved ||
		repairErr.Progress.RenamePerformedByThisCall {
		fail("late bind revalidation did not fail closed before rename: %#v", err)
	}
	if err = unix.Unmount(source, 0); err != nil {
		fail("unmount late bind: %v", err)
	}
	mounted = false
	actual, err := os.ReadFile(source)
	if err != nil || string(actual) != string(payload) {
		fail("canonical source changed: %q, %v", actual, err)
	}
	var afterStat unix.Stat_t
	if err = unix.Stat(source, &afterStat); err != nil || afterStat.Dev != stat.Dev || afterStat.Ino != stat.Ino {
		fail("canonical source identity changed: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, filepath.FromSlash(destination.String()))); !errors.Is(err, os.ErrNotExist) {
		fail("repair destination exists after rejected late bind: %v", err)
	}
	fmt.Println("PASS: late same-filesystem file bind failed closed before rename")
}
