package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupRuntimeAbsenceObservation(t *testing.T) {
	for _, c := range []struct {
		name    string
		present int
		err     error
		want    bool
	}{{"absent", 0, os.ErrNotExist, true}, {"delayed-manager-removal", 2, os.ErrNotExist, true}, {"inspection-error", 0, os.ErrPermission, false}, {"leftover", 1000, nil, false}} {
		t.Run(c.name, func(t *testing.T) {
			limit := 15 * time.Millisecond
			if c.want {
				limit = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			n := 0
			err := waitBackupRuntimeAbsence(ctx, func() error {
				n++
				if n <= c.present {
					return nil
				}
				return c.err
			}, time.Millisecond)
			if (err == nil) != c.want {
				t.Fatal("incorrect absence observation")
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if waitBackupRuntimeAbsence(ctx, func() error { t.Fatal("inspection after cancellation"); return os.ErrNotExist }, time.Millisecond) == nil {
			t.Fatal("canceled observation accepted")
		}
	})
	t.Run("dangling-link-preserved", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "runtime")
		if os.Symlink(filepath.Join(root, "missing"), path) != nil {
			t.Fatal("owned dangling-link setup")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		if waitBackupRuntimeAbsence(ctx, func() error { _, e := os.Lstat(path); return e }, time.Millisecond) == nil {
			t.Fatal("dangling link treated as absent")
		}
		st, e := os.Lstat(path)
		if e != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatal("observation modified path")
		}
	})
}
