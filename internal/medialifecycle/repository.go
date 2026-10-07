package medialifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

const rollbackBudget = 5 * time.Second
const cleanupDrainBudget = 30 * time.Second

const (
	purgeSchedulerBatchMax = 50
	purgeBackoffBase       = 5 * time.Second
	purgeBackoffCap        = 15 * time.Minute
)

type transactionDatabase interface {
	Begin(context.Context) (pgx.Tx, error)
}

type PostgresRepository struct {
	db                    transactionDatabase
	fileBaseURL           string
	newID                 func() (string, error)
	afterCommit           func(context.Context) error
	jitter                func(time.Duration) time.Duration
	beforeReclaimCommit   func(context.Context) error
	afterPurgeFileAction  func(context.Context, pgx.Tx) error
	beforeHeartbeatUpdate func(context.Context, pgx.Tx) error
	cleanupMu             sync.Mutex
	cleanupBatch          []cleanupCandidate
	cleanupBatchActive    bool
}

func NewPostgresRepository(pool *pgxpool.Pool, fileBaseURL string) (*PostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("media lifecycle database pool is required")
	}
	baseURL, err := normalizeFileBaseURL(fileBaseURL)
	if err != nil {
		return nil, err
	}
	return &PostgresRepository{db: pool, fileBaseURL: baseURL, newID: newUUIDv4, jitter: cryptoJitter}, nil
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
	if err := r.commit(ctx, tx); err != nil {
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
	if err := r.commit(ctx, tx); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Media: media}, nil
}

func (r *PostgresRepository) EnqueuePurge(ctx context.Context, mediaID string) (EnqueueResult, error) {
	return r.enqueuePurge(ctx, mediaID, false)
}

func (r *PostgresRepository) EnqueueDuePurge(ctx context.Context, mediaID string) (EnqueueResult, error) {
	return r.enqueuePurge(ctx, mediaID, true)
}

func (r *PostgresRepository) enqueuePurge(ctx context.Context, mediaID string, automatic bool) (EnqueueResult, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return EnqueueResult{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return EnqueueResult{}, err
	}
	var deletedAt *time.Time
	var due bool
	err = tx.QueryRow(ctx, `SELECT deleted_at,
		deleted_at IS NOT NULL AND purge_after IS NOT NULL AND purge_after<=clock_timestamp()
		FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		if automatic {
			return EnqueueResult{}, ErrNoPurgeWork
		}
		return EnqueueResult{}, newMediaNotFound()
	}
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("lock media for purge enqueue: %w", err)
	}
	if automatic && !due {
		return EnqueueResult{}, ErrNoPurgeWork
	}
	if !automatic && deletedAt == nil {
		return EnqueueResult{}, newMediaNotDeleted()
	}
	jobs, err := lockPurgeJobs(ctx, tx, mediaID)
	if err != nil {
		return EnqueueResult{}, err
	}
	for _, job := range jobs {
		switch job.Status {
		case readapi.JobQueued:
			if err := r.commit(ctx, tx); err != nil {
				return EnqueueResult{}, err
			}
			return EnqueueResult{Job: job, Disposition: EnqueueExistingQueued}, nil
		case readapi.JobRunning:
			if err := r.commit(ctx, tx); err != nil {
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
	if err := r.commit(ctx, tx); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Job: job, Disposition: EnqueueCreated}, nil
}

func (r *PostgresRepository) ScanDuePurges(ctx context.Context, dueThrough time.Time, after *DuePurgeCursor, limit int) ([]DuePurgeCandidate, error) {
	if dueThrough.IsZero() || limit <= 0 || limit > 100 || after != nil && (!readapi.IsUUIDv4(after.MediaID) || after.PurgeAfter.IsZero()) {
		return nil, newInvariant(errors.New("invalid due purge scan"))
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	var afterTime any
	var afterID any
	if after != nil {
		afterTime, afterID = after.PurgeAfter, after.MediaID
	}
	rows, err := tx.Query(ctx, `SELECT id::text,purge_after
		FROM media
		WHERE deleted_at IS NOT NULL AND purge_after IS NOT NULL AND purge_after<=$1
		  AND ($2::timestamptz IS NULL OR (purge_after,id)>($2,$3::uuid))
		ORDER BY purge_after,id LIMIT $4`, dueThrough, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("scan due purge media: %w", err)
	}
	defer rows.Close()
	candidates := make([]DuePurgeCandidate, 0, limit)
	for rows.Next() {
		var candidate DuePurgeCandidate
		if err := rows.Scan(&candidate.MediaID, &candidate.PurgeAfter); err != nil {
			return nil, fmt.Errorf("read due purge media: %w", err)
		}
		candidate.PurgeAfter = candidate.PurgeAfter.UTC()
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due purge media: %w", err)
	}
	if err := r.commit(ctx, tx); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (r *PostgresRepository) DatabaseNow(ctx context.Context) (time.Time, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer rollback(ctx, tx)
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	if err := r.commit(ctx, tx); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

type cleanupCandidate struct {
	ID, MediaID, TargetID, RelativePath string
	SizeBytes                           int64
	PurgeAfter                          time.Time
}

// CleanupNextRendition selects from a read-only repeatable-read snapshot, then
// adopts all destructive locks and revalidates the exact snapshot before
// invoking action. A committed terminal progress row is the retry witness for
// an uncertain commit response.
func (r *PostgresRepository) CleanupNextRendition(ctx context.Context, preferredID string, excludedIDs []string, action func(context.Context, string, string, int64) (bool, error)) (string, error) {
	if action == nil {
		return "", newInvariant(errors.New("rendition cleanup action is required"))
	}
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	excluded := make(map[string]struct{}, len(excludedIDs))
	for _, id := range excludedIDs {
		excluded[id] = struct{}{}
	}
	if preferredID == "" && r.cleanupBatchActive {
		for index, cached := range r.cleanupBatch {
			if _, skip := excluded[cached.ID]; skip {
				continue
			}
			r.cleanupBatch = append(r.cleanupBatch[:index], r.cleanupBatch[index+1:]...)
			return r.cleanupCandidate(ctx, cached, action)
		}
		r.cleanupBatch = nil
		r.cleanupBatchActive = false
		return "", nil
	}
	scan, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollback(ctx, scan)
	if _, err := scan.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return "", fmt.Errorf("start rendition cleanup snapshot: %w", err)
	}
	if preferredID != "" {
		var converged bool
		if err := scan.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM rendition_cleanup_progress p
			WHERE p.rendition_id=$1 AND p.disposition IN ('deleted','missing')
			  AND NOT EXISTS (SELECT 1 FROM renditions r WHERE r.id=p.rendition_id))`, preferredID).Scan(&converged); err != nil {
			return "", fmt.Errorf("verify rendition cleanup outcome: %w", err)
		}
		if converged {
			if err := scan.Commit(ctx); err != nil {
				return preferredID, err
			}
			return preferredID, nil
		}
	}
	var candidates []cleanupCandidate
	var cutoff time.Time
	if err := scan.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		return "", fmt.Errorf("capture rendition cleanup cutoff: %w", err)
	}
	rows, err := scan.Query(ctx, `SELECT r.id::text,r.media_id::text,r.job_target_id::text,r.relative_path,r.size_bytes,r.purge_after
		FROM renditions r
		JOIN media m ON m.id=r.media_id
		JOIN job_targets candidate_target ON candidate_target.id=r.job_target_id AND candidate_target.status='succeeded'
		JOIN jobs candidate_job ON candidate_job.id=candidate_target.job_id AND candidate_job.type='transform' AND candidate_job.media_id_snapshot=r.media_id
		JOIN profiles candidate_profile ON candidate_profile.id=candidate_target.profile_id AND candidate_profile.key=r.profile_key
		JOIN renditions current_rendition ON current_rendition.media_id=r.media_id AND current_rendition.profile_key=r.profile_key AND current_rendition.is_current
		JOIN job_targets current_target ON current_target.id=current_rendition.job_target_id AND current_target.status='succeeded'
		JOIN jobs current_job ON current_job.id=current_target.job_id AND current_job.type='transform' AND current_job.media_id_snapshot=r.media_id
		JOIN profiles current_profile ON current_profile.id=current_target.profile_id AND current_profile.key=current_rendition.profile_key AND current_profile.version>=candidate_profile.version
		WHERE NOT r.is_current AND r.purge_after IS NOT NULL AND r.purge_after<=$1
		  AND ($2='' OR r.id=$2::uuid)
		  AND NOT EXISTS (SELECT 1 FROM jobs purge_job WHERE purge_job.type='purge' AND purge_job.media_id_snapshot=r.media_id AND purge_job.started_at IS NOT NULL)
		ORDER BY r.purge_after,r.media_id,r.id LIMIT 50`, cutoff, preferredID)
	if err != nil {
		return "", fmt.Errorf("select rendition cleanup candidates: %w", err)
	}
	for rows.Next() {
		var candidate cleanupCandidate
		if err := rows.Scan(&candidate.ID, &candidate.MediaID, &candidate.TargetID, &candidate.RelativePath, &candidate.SizeBytes, &candidate.PurgeAfter); err != nil {
			rows.Close()
			return "", fmt.Errorf("read rendition cleanup candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("read rendition cleanup candidates: %w", err)
	}
	rows.Close()
	if len(candidates) == 0 {
		if preferredID != "" {
			return preferredID, newInvariant(errors.New("preferred rendition cleanup outcome is not resolvable"))
		}
		if err := scan.Commit(ctx); err != nil {
			return "", err
		}
		return "", nil
	}
	if err := scan.Commit(ctx); err != nil {
		return "", err
	}
	if preferredID != "" {
		return r.cleanupCandidate(ctx, candidates[0], action)
	}
	r.cleanupBatch = candidates
	r.cleanupBatchActive = true
	for index, candidate := range r.cleanupBatch {
		if _, skip := excluded[candidate.ID]; skip {
			continue
		}
		r.cleanupBatch = append(r.cleanupBatch[:index], r.cleanupBatch[index+1:]...)
		return r.cleanupCandidate(ctx, candidate, action)
	}
	r.cleanupBatch = nil
	r.cleanupBatchActive = false
	return "", nil
}

func (r *PostgresRepository) cleanupCandidate(ctx context.Context, candidate cleanupCandidate, action func(context.Context, string, string, int64) (bool, error)) (string, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return candidate.ID, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return candidate.ID, err
	}
	if _, err := lockMedia(ctx, tx, candidate.MediaID); err != nil {
		return candidate.ID, err
	}
	jobs, err := lockPurgeJobs(ctx, tx, candidate.MediaID)
	if err != nil {
		return candidate.ID, err
	}
	for _, job := range jobs {
		if job.StartedAt != nil {
			return candidate.ID, r.commit(ctx, tx)
		}
	}
	var exact cleanupCandidate
	err = tx.QueryRow(ctx, `SELECT id::text,media_id::text,job_target_id::text,relative_path,size_bytes,purge_after
		FROM renditions WHERE id=$1 AND media_id=$2 FOR UPDATE`, candidate.ID, candidate.MediaID).Scan(
		&exact.ID, &exact.MediaID, &exact.TargetID, &exact.RelativePath, &exact.SizeBytes, &exact.PurgeAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return candidate.ID, r.commit(ctx, tx)
	}
	if err != nil {
		return candidate.ID, fmt.Errorf("lock rendition cleanup candidate: %w", err)
	}
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT r.job_target_id=$2 AND r.relative_path=$3 AND r.size_bytes=$4 AND r.purge_after=$5
		  AND NOT r.is_current AND r.purge_after<=clock_timestamp()
		  AND EXISTS (
			SELECT 1 FROM job_targets ct JOIN jobs cj ON cj.id=ct.job_id
			JOIN profiles cp ON cp.id=ct.profile_id
			JOIN renditions current_rendition ON current_rendition.media_id=r.media_id AND current_rendition.profile_key=r.profile_key AND current_rendition.is_current
			JOIN job_targets nt ON nt.id=current_rendition.job_target_id JOIN jobs nj ON nj.id=nt.job_id JOIN profiles np ON np.id=nt.profile_id
			WHERE ct.id=r.job_target_id AND ct.status='succeeded' AND cj.type='transform' AND cj.media_id_snapshot=r.media_id AND cp.key=r.profile_key
			  AND nt.status='succeeded' AND nj.type='transform' AND nj.media_id_snapshot=r.media_id AND np.key=r.profile_key AND np.version>=cp.version)
		FROM renditions r WHERE r.id=$1`, candidate.ID, candidate.TargetID, candidate.RelativePath, candidate.SizeBytes, candidate.PurgeAfter).Scan(&eligible); err != nil {
		return candidate.ID, fmt.Errorf("recheck rendition cleanup candidate: %w", err)
	}
	if !eligible {
		return candidate.ID, r.commit(ctx, tx)
	}
	progressID, err := r.newID()
	if err != nil {
		return candidate.ID, fmt.Errorf("generate rendition cleanup progress ID: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rendition_cleanup_progress
		(id,media_id_snapshot,rendition_id,job_target_id,relative_path,size_bytes,purge_after)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, progressID, candidate.MediaID, candidate.ID, candidate.TargetID, candidate.RelativePath, candidate.SizeBytes, candidate.PurgeAfter); err != nil {
		return candidate.ID, fmt.Errorf("insert rendition cleanup progress: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return candidate.ID, err
	}
	missing, err := action(ctx, candidate.ID, candidate.RelativePath, candidate.SizeBytes)
	if err != nil {
		return candidate.ID, err
	}
	disposition := "deleted"
	if missing {
		disposition = "missing"
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupDrainBudget)
	defer cancel()
	if _, err := tx.Exec(drainCtx, `SELECT nmcp_complete_rendition_cleanup($1,$2,$3,$4)`, progressID, candidate.MediaID, candidate.ID, disposition); err != nil {
		return candidate.ID, fmt.Errorf("complete rendition cleanup: %w", err)
	}
	if err := r.commit(drainCtx, tx); err != nil {
		return candidate.ID, err
	}
	return candidate.ID, nil
}

func (r *PostgresRepository) DiscoverPurgeJobs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > purgeSchedulerBatchMax {
		return nil, newInvariant(errors.New("invalid purge discovery limit"))
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT j.id::text
		FROM jobs AS j JOIN media AS m ON m.id=j.media_id_snapshot
		WHERE j.type='purge' AND j.status='queued' AND j.available_at<=clock_timestamp()
		  AND j.attempts<j.max_attempts AND j.cancelled_at IS NULL AND m.deleted_at IS NOT NULL
		ORDER BY j.available_at,j.created_at,j.id FOR UPDATE OF m SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("discover purge jobs: %w", err)
	}
	defer rows.Close()
	jobIDs := make([]string, 0, limit)
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return nil, fmt.Errorf("read discovered purge job: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read discovered purge jobs: %w", err)
	}
	if err := r.commit(ctx, tx); err != nil {
		return nil, err
	}
	return jobIDs, nil
}

func (r *PostgresRepository) ReclaimExpiredPurges(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > purgeSchedulerBatchMax || r.jitter == nil {
		return nil, newInvariant(errors.New("invalid purge reclaim configuration"))
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return nil, err
	}
	reclaimed := make([]string, 0, limit)
	excludedJobIDs := make([]string, 0)
	for examined := 0; examined < limit; examined++ {
		var jobID, mediaID string
		err := tx.QueryRow(ctx, `SELECT j.id::text,m.id::text
			FROM jobs AS j JOIN media AS m ON m.id=j.media_id_snapshot
			WHERE j.type='purge' AND j.status='running' AND j.lease_expires_at<=clock_timestamp()
			  AND NOT (j.id=ANY($1::uuid[]))
			ORDER BY j.lease_expires_at,j.id FOR UPDATE OF m SKIP LOCKED LIMIT 1`, excludedJobIDs).Scan(&jobID, &mediaID)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("lock expired purge media: %w", err)
		}
		var attempts int
		err = tx.QueryRow(ctx, `SELECT attempts FROM jobs
			WHERE id=$1 AND media_id_snapshot=$2 AND type='purge' AND status='running'
			  AND lease_expires_at<=clock_timestamp() FOR UPDATE SKIP LOCKED`, jobID, mediaID).Scan(&attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			excludedJobIDs = append(excludedJobIDs, jobID)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock expired purge job: %w", err)
		}
		delay, err := purgeRetryDelay(r.jitter, attempts)
		if err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `UPDATE jobs SET
			status=CASE WHEN attempts<max_attempts THEN 'queued' ELSE 'failed' END,
			available_at=CASE WHEN attempts<max_attempts THEN clock_timestamp()+$2::interval ELSE available_at END,
			lease_token=NULL,lease_expires_at=NULL,error_code='lease_expired',error_message='job lease expired',
			finished_at=CASE WHEN attempts<max_attempts THEN NULL ELSE clock_timestamp() END,
			updated_at=clock_timestamp()
			WHERE id=$1 AND type='purge' AND status='running' AND lease_expires_at<=clock_timestamp()`, jobID, intervalText(delay))
		if err != nil {
			return nil, fmt.Errorf("reclaim expired purge lease: %w", err)
		}
		if tag.RowsAffected() == 1 {
			reclaimed = append(reclaimed, jobID)
		} else {
			excludedJobIDs = append(excludedJobIDs, jobID)
		}
	}
	if r.beforeReclaimCommit != nil {
		if err := r.beforeReclaimCommit(ctx); err != nil {
			return nil, err
		}
	}
	if err := r.commit(ctx, tx); err != nil {
		return nil, err
	}
	return reclaimed, nil
}

func (r *PostgresRepository) StartPurge(ctx context.Context, jobID string) (PurgeLease, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return PurgeLease{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return PurgeLease{}, err
	}

	var mediaID string
	if err := tx.QueryRow(ctx, `SELECT media_id_snapshot::text FROM jobs WHERE id=$1`, jobID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return PurgeLease{}, ErrNoPurgeWork
	} else if err != nil {
		return PurgeLease{}, fmt.Errorf("read purge media snapshot: %w", err)
	}

	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt); errors.Is(err, pgx.ErrNoRows) {
		return PurgeLease{}, ErrNoPurgeWork
	} else if err != nil {
		return PurgeLease{}, fmt.Errorf("lock purge media: %w", err)
	}
	if deletedAt == nil {
		return PurgeLease{}, ErrNoPurgeWork
	}

	var lease PurgeLease
	var jobType, status string
	var cancelledAt *time.Time
	var available bool
	if err := tx.QueryRow(ctx, `
		SELECT id::text,media_id_snapshot::text,type,status,attempts,max_attempts,
		       available_at<=clock_timestamp(),cancelled_at,available_at,created_at
		FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(
		&lease.JobID, &lease.MediaID, &jobType, &status, &lease.Attempts, &lease.MaxAttempts,
		&available, &cancelledAt, &lease.AvailableAt, &lease.CreatedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return PurgeLease{}, ErrNoPurgeWork
	} else if err != nil {
		return PurgeLease{}, fmt.Errorf("lock purge job: %w", err)
	}
	if lease.MediaID != mediaID || jobType != "purge" || status != "queued" || !available ||
		lease.Attempts >= lease.MaxAttempts || cancelledAt != nil {
		return PurgeLease{}, ErrNoPurgeWork
	}

	token, err := r.newID()
	if err != nil {
		return PurgeLease{}, fmt.Errorf("generate purge lease token: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		WITH instant AS (SELECT clock_timestamp() AS now)
		UPDATE jobs SET status='running',attempts=attempts+1,lease_token=$2,
			lease_expires_at=instant.now+$3::interval,
			started_at=COALESCE(started_at,instant.now),updated_at=instant.now
		FROM instant WHERE id=$1
		RETURNING attempts,lease_expires_at,started_at`, jobID, token, intervalText(DefaultPurgeLeaseDuration)).Scan(
		&lease.Attempts, &lease.LeaseExpiresAt, &lease.StartedAt,
	); err != nil {
		return PurgeLease{}, fmt.Errorf("start purge job: %w", err)
	}
	if err := initializePurgeManifest(ctx, tx, lease, token); err != nil {
		return PurgeLease{}, err
	}
	if err := r.commit(ctx, tx); err != nil {
		return PurgeLease{}, err
	}
	lease.Token = token
	lease.LeaseExpiresAt = lease.LeaseExpiresAt.UTC()
	lease.StartedAt = lease.StartedAt.UTC()
	lease.AvailableAt = lease.AvailableAt.UTC()
	lease.CreatedAt = lease.CreatedAt.UTC()
	return lease, nil
}

func initializePurgeManifest(ctx context.Context, tx pgx.Tx, lease PurgeLease, token string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_lease_token',$1,true)`, token); err != nil {
		return fmt.Errorf("authorize purge manifest: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO purge_file_progress
			(job_id,media_id_snapshot,object_kind,object_id,relative_path,size_bytes)
		SELECT $1::nmcp_uuid_v4,$2::nmcp_uuid_v4,'original',id,relative_path,size_bytes
		FROM originals WHERE media_id=$2::nmcp_uuid_v4
		UNION ALL
		SELECT $1::nmcp_uuid_v4,$2::nmcp_uuid_v4,'rendition',id,relative_path,size_bytes
		FROM renditions WHERE media_id=$2::nmcp_uuid_v4
		ON CONFLICT (job_id,object_kind,object_id) DO NOTHING`, lease.JobID, lease.MediaID); err != nil {
		return fmt.Errorf("initialize purge manifest: %w", err)
	}
	var mismatch bool
	if err := tx.QueryRow(ctx, `WITH expected AS (
			SELECT $2::nmcp_uuid_v4 AS media_id_snapshot,'original'::text AS object_kind,id AS object_id,relative_path,size_bytes
			FROM originals WHERE media_id=$2
			UNION ALL
			SELECT $2::nmcp_uuid_v4,'rendition'::text,id,relative_path,size_bytes
			FROM renditions WHERE media_id=$2
		), actual AS (
			SELECT media_id_snapshot,object_kind,object_id,relative_path,size_bytes
			FROM purge_file_progress WHERE job_id=$1
		), difference AS (
			(SELECT * FROM expected EXCEPT SELECT * FROM actual)
			UNION ALL
			(SELECT * FROM actual EXCEPT SELECT * FROM expected)
		)
		SELECT EXISTS (SELECT 1 FROM difference)`, lease.JobID, lease.MediaID).Scan(&mismatch); err != nil {
		return fmt.Errorf("verify purge manifest: %w", err)
	}
	if mismatch {
		return newInvariant(errors.New("purge manifest does not exactly match owned files"))
	}
	return nil
}

func (r *PostgresRepository) RunPurgeFileStep(ctx context.Context, jobID, token string, action PurgeFileAction) (PurgeStepResult, error) {
	if !readapi.IsUUIDv4(jobID) || !readapi.IsUUIDv4(token) || action == nil {
		return PurgeStepResult{}, newInvariant(errors.New("invalid purge file step"))
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return PurgeStepResult{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return PurgeStepResult{}, err
	}
	var mediaID string
	if err := tx.QueryRow(ctx, `SELECT media_id_snapshot::text FROM jobs WHERE id=$1 AND type='purge'`, jobID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	} else if err != nil {
		return PurgeStepResult{}, fmt.Errorf("read purge step media: %w", err)
	}
	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt); errors.Is(err, pgx.ErrNoRows) {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	} else if err != nil {
		return PurgeStepResult{}, fmt.Errorf("lock purge step media: %w", err)
	}
	if deletedAt == nil {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		FROM jobs WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 FOR UPDATE`, jobID, token, mediaID).Scan(&live); errors.Is(err, pgx.ErrNoRows) {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	} else if err != nil {
		return PurgeStepResult{}, fmt.Errorf("lock purge step job: %w", err)
	}
	if !live {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	}
	file := PurgeFile{JobID: jobID, MediaID: mediaID}
	var kind string
	err = tx.QueryRow(ctx, `SELECT object_kind,object_id::text,relative_path,size_bytes
		FROM purge_file_progress WHERE job_id=$1 AND disposition='pending'
		ORDER BY CASE object_kind WHEN 'rendition' THEN 0 ELSE 1 END,object_id LIMIT 1`, jobID).Scan(&kind, &file.ObjectID, &file.RelativePath, &file.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := r.commit(ctx, tx); err != nil {
			return PurgeStepResult{}, err
		}
		return PurgeStepResult{Done: true}, nil
	}
	if err != nil {
		return PurgeStepResult{}, fmt.Errorf("lock pending purge file: %w", err)
	}
	file.Kind = PurgeFileKind(kind)
	if file.Kind != PurgeFileOriginal && file.Kind != PurgeFileRendition {
		return PurgeStepResult{}, newInvariant(errors.New("invalid purge file kind"))
	}
	disposition, err := action(ctx, file)
	if err != nil {
		return PurgeStepResult{}, err
	}
	if disposition != PurgeFileDeleted && disposition != PurgeFileMissing {
		return PurgeStepResult{}, newInvariant(errors.New("invalid purge file disposition"))
	}
	if r.afterPurgeFileAction != nil {
		if err := r.afterPurgeFileAction(ctx, tx); err != nil {
			return PurgeStepResult{}, err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs
		WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 AND status='running'
		  AND lease_token=$2 AND lease_expires_at>clock_timestamp())`, jobID, token, mediaID).Scan(&live); err != nil {
		return PurgeStepResult{}, fmt.Errorf("recheck purge file lease: %w", err)
	}
	if !live {
		return PurgeStepResult{}, ErrPurgeLeaseLost
	}
	if _, err := tx.Exec(ctx, `SELECT nmcp_complete_purge_file_progress($1,$2,$3,$4,$5,$6)`, jobID, file.MediaID, file.Kind, file.ObjectID, token, disposition); err != nil {
		return PurgeStepResult{}, fmt.Errorf("complete purge file: %w", err)
	}
	if err := r.commit(ctx, tx); err != nil {
		return PurgeStepResult{}, err
	}
	return PurgeStepResult{File: &file}, nil
}

func (r *PostgresRepository) FinalizePurge(ctx context.Context, jobID, token string) error {
	if !readapi.IsUUIDv4(jobID) || !readapi.IsUUIDv4(token) {
		return newInvalidID()
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return err
	}
	var mediaID string
	if err := tx.QueryRow(ctx, `SELECT media_id_snapshot::text FROM jobs WHERE id=$1 AND type='purge'`, jobID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return ErrPurgeLeaseLost
	} else if err != nil {
		return fmt.Errorf("read purge finalizer media: %w", err)
	}
	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt); errors.Is(err, pgx.ErrNoRows) {
		var converged bool
		if err := tx.QueryRow(ctx, `SELECT status='succeeded' AND lease_token IS NULL AND lease_expires_at IS NULL
			AND (SELECT count(*)=1 FROM change_events WHERE media_id=$2 AND event_type='media_purged' AND reason='physical_purge')
			FROM jobs WHERE id=$1 AND type='purge' AND media_id_snapshot=$2`, jobID, mediaID).Scan(&converged); errors.Is(err, pgx.ErrNoRows) {
			return ErrPurgeLeaseLost
		} else if err != nil {
			return fmt.Errorf("verify committed purge finalization: %w", err)
		}
		if !converged {
			return ErrPurgeLeaseLost
		}
		return r.commit(ctx, tx)
	} else if err != nil {
		return fmt.Errorf("lock purge finalizer media: %w", err)
	}
	if deletedAt == nil {
		return ErrPurgeLeaseLost
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		FROM jobs WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 FOR UPDATE`, jobID, token, mediaID).Scan(&live); errors.Is(err, pgx.ErrNoRows) {
		return ErrPurgeLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock purge finalizer Job: %w", err)
	}
	if !live {
		return ErrPurgeLeaseLost
	}
	var mismatch, pending bool
	if err := tx.QueryRow(ctx, `WITH expected AS (
			SELECT $2::nmcp_uuid_v4 AS media_id_snapshot,'original'::text AS object_kind,id AS object_id,relative_path,size_bytes FROM originals WHERE media_id=$2
			UNION ALL
			SELECT $2::nmcp_uuid_v4,'rendition'::text,id,relative_path,size_bytes FROM renditions WHERE media_id=$2
		), actual AS (
			SELECT media_id_snapshot,object_kind,object_id,relative_path,size_bytes FROM purge_file_progress WHERE job_id=$1
		), difference AS (
			(SELECT * FROM expected EXCEPT SELECT * FROM actual)
			UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)
		)
		SELECT EXISTS(SELECT 1 FROM difference),EXISTS(SELECT 1 FROM purge_file_progress WHERE job_id=$1 AND disposition='pending')`, jobID, mediaID).Scan(&mismatch, &pending); err != nil {
		return fmt.Errorf("verify purge finalizer manifest: %w", err)
	}
	if mismatch {
		return newInvariant(errors.New("purge manifest does not match Media-owned files"))
	}
	if pending {
		return ErrNoPurgeWork
	}
	var existingTombstones int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM change_events WHERE media_id=$1 AND event_type='media_purged' AND reason='physical_purge'`, mediaID).Scan(&existingTombstones); err != nil {
		return fmt.Errorf("check physical purge tombstone history: %w", err)
	}
	if existingTombstones != 0 {
		return newInvariant(errors.New("physical purge tombstone already exists"))
	}
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('nmcp.purge_job_id',$1,true),pg_catalog.set_config('nmcp.purge_lease_token',$2,true)`, jobID, token); err != nil {
		return fmt.Errorf("authorize purge finalizer: %w", err)
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if tag, err := tx.Exec(ctx, `DELETE FROM media WHERE id=$1`, mediaID); err != nil {
		return fmt.Errorf("delete purged Media: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ErrPurgeLeaseLost
	}
	if err := r.insertEvent(ctx, tx, mediaID, "media_purged", "physical_purge", nil, now); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,
		finished_at=$4,error_code=NULL,error_message=NULL,updated_at=$4
		WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 AND status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()`, jobID, token, mediaID, now)
	if err != nil {
		return fmt.Errorf("complete purge Job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrPurgeLeaseLost
	}
	return r.commit(ctx, tx)
}

func (r *PostgresRepository) HeartbeatPurge(ctx context.Context, jobID, token string) (time.Time, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return time.Time{}, err
	}
	var mediaID string
	if err := tx.QueryRow(ctx, `SELECT media_id_snapshot::text FROM jobs WHERE id=$1 AND type='purge'`, jobID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrPurgeLeaseLost
	} else if err != nil {
		return time.Time{}, fmt.Errorf("read purge heartbeat Media: %w", err)
	}
	var deletedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrPurgeLeaseLost
	} else if err != nil {
		return time.Time{}, fmt.Errorf("lock purge heartbeat Media: %w", err)
	}
	if deletedAt == nil {
		return time.Time{}, ErrPurgeLeaseLost
	}
	var previousExpiry time.Time
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at FROM jobs
		WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 AND status='running' AND lease_token=$2
		FOR UPDATE`, jobID, token, mediaID).Scan(&previousExpiry); errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrPurgeLeaseLost
	} else if err != nil {
		return time.Time{}, fmt.Errorf("lock purge heartbeat Job: %w", err)
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	if !previousExpiry.After(now) {
		return time.Time{}, ErrPurgeLeaseLost
	}
	if r.beforeHeartbeatUpdate != nil {
		if err := r.beforeHeartbeatUpdate(ctx, tx); err != nil {
			return time.Time{}, fmt.Errorf("before purge heartbeat update: %w", err)
		}
	}
	var expires time.Time
	if err := tx.QueryRow(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()+interval '2 minutes',updated_at=clock_timestamp()
		WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 AND status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		RETURNING lease_expires_at`, jobID, token, mediaID).Scan(&expires); errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrPurgeLeaseLost
	} else if err != nil {
		return time.Time{}, fmt.Errorf("update purge heartbeat: %w", err)
	}
	if err := r.commit(ctx, tx); err != nil {
		return time.Time{}, err
	}
	return expires.UTC(), nil
}

func (r *PostgresRepository) FinishPurgeAttempt(ctx context.Context, jobID, token, code string) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err := lockMaintenance(ctx, tx); err != nil {
		return err
	}
	var mediaID string
	if err := tx.QueryRow(ctx, `SELECT media_id_snapshot::text FROM jobs WHERE id=$1 AND type='purge'`, jobID).Scan(&mediaID); errors.Is(err, pgx.ErrNoRows) {
		return ErrPurgeLeaseLost
	} else if err != nil {
		return fmt.Errorf("read purge attempt Media: %w", err)
	}
	var deletedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, mediaID).Scan(&deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPurgeLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock purge attempt Media: %w", err)
	}
	if deletedAt == nil {
		return ErrPurgeLeaseLost
	}
	var attempts, maximum int
	if err := tx.QueryRow(ctx, `SELECT attempts,max_attempts FROM jobs WHERE id=$1 AND type='purge' AND media_id_snapshot=$3 AND status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp() FOR UPDATE`, jobID, token, mediaID).Scan(&attempts, &maximum); errors.Is(err, pgx.ErrNoRows) {
		return ErrPurgeLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock purge attempt Job: %w", err)
	}
	status := "queued"
	var finished any
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	delay, err := purgeRetryDelay(r.jitter, attempts)
	if err != nil {
		return newInvariant(err)
	}
	available := now.Add(delay)
	if attempts >= maximum {
		status, finished = "failed", now
	}
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status=$3,lease_token=NULL,lease_expires_at=NULL,available_at=$4,finished_at=$5,error_code=$6,error_message='purge attempt failed',updated_at=clock_timestamp() WHERE id=$1 AND lease_token=$2 AND lease_expires_at>clock_timestamp()`, jobID, token, status, available, finished, code)
	if err != nil {
		return fmt.Errorf("finish purge attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrPurgeLeaseLost
	}
	return r.commit(ctx, tx)
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

func (r *PostgresRepository) commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		var postgresError *pgconn.PgError
		if errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &postgresError) {
			return &CommitRolledBack{Cause: err}
		}
		return &CommitOutcomeUnknown{Cause: err}
	}
	if r.afterCommit != nil {
		if err := r.afterCommit(ctx); err != nil {
			return &CommitOutcomeUnknown{Cause: err}
		}
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

func intervalText(duration time.Duration) string {
	return fmt.Sprintf("%d microseconds", duration.Microseconds())
}

func purgeBackoffMaximum(attempt int) time.Duration {
	maximum := purgeBackoffBase
	for index := 1; index < attempt && maximum < purgeBackoffCap; index++ {
		if maximum > purgeBackoffCap/2 {
			return purgeBackoffCap
		}
		maximum *= 2
	}
	if maximum > purgeBackoffCap {
		return purgeBackoffCap
	}
	return maximum
}

func purgeRetryDelay(jitter func(time.Duration) time.Duration, attempt int) (time.Duration, error) {
	if jitter == nil {
		return 0, newInvariant(errors.New("purge retry jitter is required"))
	}
	maximum := purgeBackoffMaximum(attempt)
	delay := jitter(maximum)
	if delay < 0 || delay > maximum {
		return 0, newInvariant(errors.New("invalid purge retry jitter"))
	}
	return delay, nil
}

func cryptoJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0
	}
	var number uint64
	for _, b := range value {
		number = number<<8 | uint64(b)
	}
	return time.Duration(number % (uint64(maximum) + 1))
}

const purgeJobsForUpdateSQL = `
	SELECT id::text,type,status,media_id_snapshot::text,original_id::text,attempts,max_attempts,
	       available_at,started_at,finished_at,error_code,error_message,cancelled_at,cancel_reason,created_at,updated_at
	FROM jobs WHERE media_id_snapshot=$1 AND type='purge' ORDER BY id FOR UPDATE`
