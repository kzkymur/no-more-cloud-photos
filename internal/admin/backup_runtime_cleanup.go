package admin

import (
	"context"
	"os"
	"time"
)

// systemd 255 emits JobRemoved before synchronously destroying runtime data,
// so a completed start/stop job is not itself an absence observation. The
// wait below polls for absence and so does not depend on that ordering.
// Wait only for removal by the manager: never stop a unit or delete a path here.
func waitBackupRuntimeCredentialsAbsent(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	return waitBackupRuntimeAbsence(ctx, func() error { _, err := os.Lstat("/run/nmcp-backup"); return err }, 20*time.Millisecond)
}
func waitBackupRuntimeAbsence(ctx context.Context, inspect func() error, interval time.Duration) error {
	for {
		if ctx.Err() != nil {
			return errCutover
		}
		err := inspect()
		if ctx.Err() != nil {
			return errCutover
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return errCutover
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errCutover
		case <-timer.C:
		}
	}
}
