// Package admin implements the administrative application boundary. It never
// logs connection strings, raw argv, profile documents, or database error text.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrValidation     = errors.New("invalid administrative request")
	ErrConflict       = errors.New("conflict or nothing eligible")
	ErrConfirmation   = errors.New("confirmation required (--yes)")
	ErrPartial        = errors.New("partial batch failure; resume with the returned token")
	ErrUnsupported    = errors.New("backup operations belong to Issue19 and are not implemented")
	profileKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type Service struct {
	Pool  *pgxpool.Pool
	Actor string
	Host  string

	afterBatchCommit func(context.Context) error
	afterResumeFence func(context.Context) error
}

func New(pool *pgxpool.Pool) *Service {
	actor := os.Getenv("USER")
	if actor == "" {
		actor = "unknown"
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return &Service{Pool: pool, Actor: actor, Host: host}
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("system random source unavailable")
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func classified(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		switch pgerr.Code {
		case "23505", "55P03", "40001", "40P01":
			return ErrConflict
		case "23514", "22003", "22P02":
			return ErrValidation
		}
	}
	return err
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func tryLock(ctx context.Context, tx pgx.Tx, name string) error {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,1818))`, name).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrConflict
	}
	return nil
}

func normal(ctx context.Context, tx pgx.Tx) error {
	var mode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1 FOR SHARE NOWAIT`).Scan(&mode); err != nil {
		return classified(err)
	}
	if mode != "normal" {
		return ErrConflict
	}
	return nil
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, command string, args any, ids []string, outcome string, started time.Time) error {
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []string{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit
		(id,command,actor,host,sanitized_arguments,outcome,affected_ids,started_at,finished_at)
		VALUES($1,$2,$3,$4,$5,$6,$7::uuid[], $8,clock_timestamp())`,
		newID(), command, s.Actor, s.Host, encoded, outcome, ids, started)
	return err
}

// mutate audits successful changes in their own transaction. Rejected database
// changes use a savepoint, so their sanitized failure audit can still commit.
func (s *Service) mutate(ctx context.Context, command, lock string, args any, ids []string, fn func(pgx.Tx) error) error {
	started := time.Now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT effect`); err != nil {
		return err
	}
	err = tryLock(ctx, tx, lock)
	if err == nil {
		err = fn(tx)
	}
	outcome := "succeeded"
	if err != nil {
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT effect`); rollbackErr != nil {
			return rollbackErr
		}
		outcome = "failed"
	}
	if auditErr := s.audit(ctx, tx, command, args, ids, outcome, started); auditErr != nil {
		return auditErr
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return commitErr
	}
	return classified(err)
}

// commitBatch permits a deterministic lost-reply seam only after a real durable
// commit, matching lifecycle repository fault tests. Production has no hook.
func (s *Service) commitBatch(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.afterBatchCommit != nil {
		return s.afterBatchCommit(ctx)
	}
	return nil
}
