package admin

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// backupAuthority serializes all probes on the original advisory-lock session.
// pgx connections are not concurrency safe. Stop joins the heartbeat before the
// caller may begin its final transaction. The independent filesystem flock stays
// held throughout this lifecycle, including failure journaling.
type backupAuthority struct {
	conn interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
	mu       sync.Mutex
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
	ctx      context.Context
	lost     bool
}

func newBackupAuthority(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) *backupAuthority {
	op, cancel := context.WithCancel(ctx)
	return &backupAuthority{conn: conn, ctx: op, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
}
func (a *backupAuthority) Check(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lost {
		return errBackup
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var held bool
	err := a.conn.QueryRow(probe, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted AND mode='ExclusiveLock' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=((hashtextextended('backup',1818)>>32)&4294967295)::oid AND objid=(hashtextextended('backup',1818)&4294967295)::oid AND objsubid=1)`).Scan(&held)
	if err != nil || !held {
		a.lost = true
		a.cancel()
		return errBackup
	}
	return nil
}
func (a *backupAuthority) Start() {
	go func() {
		defer close(a.done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-a.ctx.Done():
				return
			case <-tick.C:
				if a.Check(a.ctx) != nil {
					return
				}
			}
		}
	}()
}
func (a *backupAuthority) Stop() error {
	a.stopOnce.Do(func() { close(a.stop) })
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lost {
		return errBackup
	}
	return nil
}
