package medialifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

const rollbackBudget = 5 * time.Second

type transactionDatabase interface {
	Begin(context.Context) (pgx.Tx, error)
}

type PostgresRepository struct {
	db          transactionDatabase
	fileBaseURL string
	newID       func() (string, error)
}

func NewPostgresRepository(pool *pgxpool.Pool, fileBaseURL string) (*PostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("media lifecycle database pool is required")
	}
	baseURL, err := normalizeFileBaseURL(fileBaseURL)
	if err != nil {
		return nil, err
	}
	return &PostgresRepository{db: pool, fileBaseURL: baseURL, newID: newUUIDv4}, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, mediaID string) (DeleteResult, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return DeleteResult{}, err
	}
	deletedAt, err := lockMedia(ctx, tx, mediaID)
	if err != nil {
		return DeleteResult{}, err
	}

	changed := deletedAt == nil
	if changed {
		var retentionDays *int
		if err := tx.QueryRow(ctx, `SELECT deleted_media_retention_days FROM system_config WHERE id=1 FOR SHARE`).Scan(&retentionDays); err != nil {
			return DeleteResult{}, fmt.Errorf("lock deleted media retention: %w", err)
		}
		if retentionDays != nil && (*retentionDays < 0 || *retentionDays > MaxDeletedMediaRetentionDays) {
			return DeleteResult{}, newInvariant(fmt.Errorf("deleted media retention is outside 0..%d days", MaxDeletedMediaRetentionDays))
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return DeleteResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE media SET deleted_at=$2,
				purge_after=CASE WHEN $3::integer IS NULL THEN NULL ELSE $2::timestamptz+$3*interval '1 day' END
			WHERE id=$1`, mediaID, now, retentionDays); err != nil {
			return DeleteResult{}, fmt.Errorf("logically delete media: %w", err)
		}
		if err := r.insertEvent(ctx, tx, mediaID, "media_deleted", "logical_delete", nil, now); err != nil {
			return DeleteResult{}, err
		}
	}
	media, err := r.mediaDetail(ctx, tx, mediaID)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := commit(tx, ctx); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{Media: media, Changed: changed}, nil
}

func (r *PostgresRepository) Restore(ctx context.Context, mediaID string) (RestoreResult, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return RestoreResult{}, err
	}
	deletedAt, err := lockMedia(ctx, tx, mediaID)
	if err != nil {
		return RestoreResult{}, err
	}
	if deletedAt == nil {
		return RestoreResult{}, newMediaNotDeleted()
	}
	jobs, err := lockPurgeJobs(ctx, tx, mediaID)
	if err != nil {
		return RestoreResult{}, err
	}
	for _, job := range jobs {
		if job.StartedAt != nil {
			return RestoreResult{}, newPurgeAlreadyStarted(job.ID)
		}
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return RestoreResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status='cancelled',finished_at=$2,cancelled_at=$2,cancel_reason='media_restored',updated_at=$2
		WHERE media_id_snapshot=$1 AND type='purge' AND status='queued' AND started_at IS NULL`, mediaID, now); err != nil {
		return RestoreResult{}, fmt.Errorf("cancel queued purge jobs: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE media SET deleted_at=NULL,purge_after=NULL WHERE id=$1`, mediaID); err != nil {
		return RestoreResult{}, fmt.Errorf("restore media: %w", err)
	}
	media, err := r.mediaDetail(ctx, tx, mediaID)
	if err != nil {
		return RestoreResult{}, err
	}
	payload, err := json.Marshal(mediaEventSnapshot(media))
	if err != nil {
		return RestoreResult{}, fmt.Errorf("marshal restore event: %w", err)
	}
	if err := r.insertEvent(ctx, tx, mediaID, "media_upsert", "restore", payload, now); err != nil {
		return RestoreResult{}, err
	}
	if err := commit(tx, ctx); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Media: media}, nil
}

func (r *PostgresRepository) EnqueuePurge(ctx context.Context, mediaID string) (EnqueueResult, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return EnqueueResult{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return EnqueueResult{}, err
	}
	deletedAt, err := lockMedia(ctx, tx, mediaID)
	if err != nil {
		return EnqueueResult{}, err
	}
	if deletedAt == nil {
		return EnqueueResult{}, newMediaNotDeleted()
	}
	jobs, err := lockPurgeJobs(ctx, tx, mediaID)
	if err != nil {
		return EnqueueResult{}, err
	}
	for _, job := range jobs {
		switch job.Status {
		case readapi.JobQueued:
			if err := commit(tx, ctx); err != nil {
				return EnqueueResult{}, err
			}
			return EnqueueResult{Job: job, Disposition: EnqueueExistingQueued}, nil
		case readapi.JobRunning:
			if err := commit(tx, ctx); err != nil {
				return EnqueueResult{}, err
			}
			return EnqueueResult{Job: job, Disposition: EnqueueExistingRunning}, nil
		case readapi.JobFailed:
			return EnqueueResult{}, newPurgeFailed(job.ID)
		case readapi.JobCancelled:
			continue
		default:
			return EnqueueResult{}, newInvariant(fmt.Errorf("deleted media has purge job %s in status %s", job.ID, job.Status))
		}
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return EnqueueResult{}, err
	}
	jobID, err := r.newID()
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("generate purge job ID: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (id,type,media_id_snapshot,status,attempts,max_attempts,available_at,created_at,updated_at)
		VALUES ($1,'purge',$2,'queued',0,$3,$4,$4,$4)`, jobID, mediaID, InitialPurgeMaxAttempts, now); err != nil {
		return EnqueueResult{}, fmt.Errorf("insert purge job: %w", err)
	}
	job := readapi.NewJob()
	job.ID, job.Type, job.Status, job.MediaID = jobID, readapi.JobPurge, readapi.JobQueued, mediaID
	job.MaxAttempts, job.AvailableAt, job.CreatedAt, job.UpdatedAt = InitialPurgeMaxAttempts, now.UTC(), now.UTC(), now.UTC()
	if err := commit(tx, ctx); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Job: job, Disposition: EnqueueCreated}, nil
}

func (r *PostgresRepository) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin media lifecycle transaction: %w", err)
	}
	return tx, nil
}

func lockMaintenance(ctx context.Context, tx pgx.Tx) error {
	var mode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id=1 FOR SHARE`).Scan(&mode); err != nil {
		return fmt.Errorf("lock maintenance state: %w", err)
	}
	if mode != "normal" {
		return newUnavailable(errors.New("maintenance mode is active"))
	}
	return nil
}

func lockMedia(ctx context.Context, tx pgx.Tx, mediaID string) (*time.Time, error) {
	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, newMediaNotFound()
		}
		return nil, fmt.Errorf("lock media: %w", err)
	}
	return deletedAt, nil
}

func lockPurgeJobs(ctx context.Context, tx pgx.Tx, mediaID string) ([]readapi.Job, error) {
	rows, err := tx.Query(ctx, purgeJobsForUpdateSQL, mediaID)
	if err != nil {
		return nil, fmt.Errorf("lock purge jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]readapi.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locked purge jobs: %w", err)
	}
	return jobs, nil
}

func databaseNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read lifecycle timestamp: %w", err)
	}
	return now.UTC(), nil
}

func (r *PostgresRepository) insertEvent(ctx context.Context, tx pgx.Tx, mediaID, eventType, reason string, payload json.RawMessage, now time.Time) error {
	var position int64
	if err := tx.QueryRow(ctx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1 RETURNING last_position`).Scan(&position); err != nil {
		return fmt.Errorf("allocate lifecycle change position: %w", err)
	}
	eventID, err := r.newID()
	if err != nil {
		return fmt.Errorf("generate lifecycle event ID: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO change_events (id,position,event_type,reason,media_id,payload,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, eventID, position, eventType, reason, mediaID, payload, now); err != nil {
		return fmt.Errorf("insert lifecycle change event: %w", err)
	}
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackBudget)
	defer cancel()
	_ = tx.Rollback(rollbackCtx)
}

func commit(tx pgx.Tx, ctx context.Context) error {
	if err := tx.Commit(ctx); err != nil {
		var postgresError *pgconn.PgError
		if errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &postgresError) {
			return &CommitRolledBack{Cause: err}
		}
		return &CommitOutcomeUnknown{Cause: err}
	}
	return nil
}

func newUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}

const purgeJobsForUpdateSQL = `
	SELECT id::text,type,status,media_id_snapshot::text,original_id::text,attempts,max_attempts,
	       available_at,started_at,finished_at,error_code,error_message,cancelled_at,cancel_reason,created_at,updated_at
	FROM jobs WHERE media_id_snapshot=$1 AND type='purge' ORDER BY id FOR UPDATE`
