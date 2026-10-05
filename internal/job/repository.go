package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultLeaseDuration = 2 * time.Minute
	defaultReclaimBatch  = 50
	rollbackTimeout      = 5 * time.Second
	backoffBase          = 5 * time.Second
	backoffCap           = 15 * time.Minute
)

type Repository struct {
	pool          *pgxpool.Pool
	leaseDuration time.Duration
	reclaimBatch  int
	jitter        func(time.Duration) time.Duration
	uuid          func() (string, error)
}

func NewRepository(pool *pgxpool.Pool, options Options) (*Repository, error) {
	if pool == nil || options.LeaseDuration < 0 || options.LeaseDuration > 0 && options.LeaseDuration.Microseconds() == 0 ||
		options.ReclaimBatch < 0 || options.ReclaimBatch > 50 {
		return nil, ErrInvalid
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultLeaseDuration
	}
	if options.ReclaimBatch == 0 {
		options.ReclaimBatch = defaultReclaimBatch
	}
	if options.Jitter == nil {
		options.Jitter = cryptoJitter
	}
	if options.UUID == nil {
		options.UUID = newUUIDv4
	}
	return &Repository{pool: pool, leaseDuration: options.LeaseDuration, reclaimBatch: options.ReclaimBatch, jitter: options.Jitter, uuid: options.UUID}, nil
}

func (r *Repository) Claim(ctx context.Context, registeredTypes []Type) (Lease, error) {
	types, err := validateTypes(registeredTypes)
	if err != nil {
		return Lease{}, err
	}
	if len(types) == 0 {
		return Lease{}, ErrNoWork
	}
	token, err := r.uuid()
	if err != nil {
		return Lease{}, fmt.Errorf("generate lease token: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Lease{}, classifyDatabaseError(err)
	}
	defer rollback(tx)

	var lease Lease
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM jobs
			WHERE status='queued' AND type=ANY($1::text[]) AND available_at<=clock_timestamp() AND attempts<max_attempts
			ORDER BY available_at,created_at,id
			FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE jobs AS j SET status='running',attempts=j.attempts+1,lease_token=$2,
			lease_expires_at=clock_timestamp()+$3::interval,started_at=COALESCE(j.started_at,clock_timestamp()),
			updated_at=clock_timestamp()
		FROM candidate WHERE j.id=candidate.id
		RETURNING j.id::text,j.type,j.original_id::text,j.media_id_snapshot::text,j.attempts,j.max_attempts,
			j.lease_expires_at,j.started_at,j.available_at,j.created_at`, types, token, intervalText(r.leaseDuration)).Scan(
		&lease.ID, &lease.Type, &lease.OriginalID, &lease.MediaID, &lease.Attempts, &lease.MaxAttempts,
		&lease.LeaseExpiresAt, &lease.StartedAt, &lease.AvailableAt, &lease.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNoWork
	}
	if err != nil {
		return Lease{}, classifyDatabaseError(err)
	}
	lease.Token = token
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='pending',error_code=NULL,error_message=NULL,updated_at=clock_timestamp() WHERE job_id=$1 AND status='failed'`, lease.ID); err != nil {
		return Lease{}, classifyDatabaseError(err)
	}
	lease.Targets, err = loadTargets(ctx, tx, lease.ID)
	if err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Lease{}, classifyDatabaseError(err)
	}
	normalizeLease(&lease)
	return lease, nil
}

func (r *Repository) Heartbeat(ctx context.Context, jobID, token string) (time.Time, error) {
	var expiry time.Time
	err := r.pool.QueryRow(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()+$3::interval,updated_at=clock_timestamp()
		WHERE id=$1 AND status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		RETURNING lease_expires_at`, jobID, token, intervalText(r.leaseDuration)).Scan(&expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrLeaseLost
	}
	if err != nil {
		return time.Time{}, classifyDatabaseError(err)
	}
	return expiry.UTC(), nil
}

func (r *Repository) BeginTarget(ctx context.Context, jobID, token, targetID string) error {
	return r.withLiveLease(ctx, jobID, token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE job_targets AS jt SET attempts=jt.attempts+1,updated_at=clock_timestamp()
			FROM jobs AS j WHERE jt.id=$1 AND jt.job_id=$2 AND jt.status='pending' AND j.id=jt.job_id AND jt.attempts<j.attempts`, targetID, jobID)
		if err != nil {
			return classifyDatabaseError(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		return targetStateError(ctx, tx, jobID, targetID)
	})
}

func (r *Repository) MarkTargetFailed(ctx context.Context, jobID, token, targetID string, code FailureCode) error {
	message, ok := workerFailure(code)
	if !ok || code == FailureWorkerShutdown {
		return ErrInvalid
	}
	return r.withLiveLease(ctx, jobID, token, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE job_targets AS jt SET status='failed',error_code=$3,error_message=$4,updated_at=clock_timestamp()
			WHERE jt.id=$1 AND jt.job_id=$2 AND jt.status='pending'`, targetID, jobID, code, message)
		if err != nil {
			return classifyDatabaseError(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		return targetStateError(ctx, tx, jobID, targetID)
	})
}

func (r *Repository) FinishAttempt(ctx context.Context, jobID, token string, code FailureCode) error {
	message, ok := workerFailure(code)
	if !ok {
		return ErrInvalid
	}
	return r.withLiveLease(ctx, jobID, token, func(tx pgx.Tx) error {
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT attempts FROM jobs WHERE id=$1`, jobID).Scan(&attempts); err != nil {
			return classifyDatabaseError(err)
		}
		delay, err := r.retryDelay(attempts)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE jobs SET status=CASE WHEN attempts<max_attempts THEN 'queued' ELSE 'failed' END,
			available_at=CASE WHEN attempts<max_attempts THEN clock_timestamp()+$2::interval ELSE available_at END,
			lease_token=NULL,lease_expires_at=NULL,error_code=$3,error_message=$4,
			finished_at=CASE WHEN attempts<max_attempts THEN NULL ELSE clock_timestamp() END,updated_at=clock_timestamp()
			WHERE id=$1`, jobID, intervalText(delay), code, message)
		return classifyDatabaseError(err)
	})
}

func (r *Repository) CompleteSucceeded(ctx context.Context, jobID, token string) error {
	return r.withLiveLease(ctx, jobID, token, func(tx pgx.Tx) error {
		var jobType Type
		if err := tx.QueryRow(ctx, `SELECT type FROM jobs WHERE id=$1`, jobID).Scan(&jobType); err != nil {
			return classifyDatabaseError(err)
		}
		if jobType == TypeTransform {
			var incomplete bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM job_targets WHERE job_id=$1 AND status<>'succeeded')`, jobID).Scan(&incomplete); err != nil {
				return classifyDatabaseError(err)
			}
			if incomplete {
				return ErrConflict
			}
		}
		_, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,
			finished_at=clock_timestamp(),error_code=NULL,error_message=NULL,updated_at=clock_timestamp() WHERE id=$1`, jobID)
		return classifyDatabaseError(err)
	})
}

func (r *Repository) ReclaimExpired(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, classifyDatabaseError(err)
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT id::text,attempts FROM jobs
		WHERE status='running' AND lease_expires_at<=clock_timestamp()
		ORDER BY lease_expires_at,id FOR UPDATE SKIP LOCKED LIMIT $1`, r.reclaimBatch)
	if err != nil {
		return 0, classifyDatabaseError(err)
	}
	type expired struct {
		id       string
		attempts int
	}
	var jobs []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.attempts); err != nil {
			rows.Close()
			return 0, classifyDatabaseError(err)
		}
		jobs = append(jobs, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, classifyDatabaseError(err)
	}
	rows.Close()
	for _, item := range jobs {
		delay, err := r.retryDelay(item.attempts)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `UPDATE jobs SET status=CASE WHEN attempts<max_attempts THEN 'queued' ELSE 'failed' END,
			available_at=CASE WHEN attempts<max_attempts THEN clock_timestamp()+$2::interval ELSE available_at END,
			lease_token=NULL,lease_expires_at=NULL,error_code=$3,error_message=$4,
			finished_at=CASE WHEN attempts<max_attempts THEN NULL ELSE clock_timestamp() END,updated_at=clock_timestamp()
			WHERE id=$1`, item.id, intervalText(delay), FailureLeaseExpired, safeFailureMessages[FailureLeaseExpired])
		if err != nil {
			return 0, classifyDatabaseError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, classifyDatabaseError(err)
	}
	return len(jobs), nil
}

func (r *Repository) AdminRetryFailed(ctx context.Context, jobID string, additionalAttempts int, audit AdminAudit) error {
	if additionalAttempts <= 0 || audit.ID == "" || audit.Actor == "" || audit.Host == "" || !jsonObject(audit.SanitizedArguments) {
		return ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return classifyDatabaseError(err)
	}
	defer rollback(tx)
	var status Status
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		if err := insertAdminRetryAudit(ctx, tx, jobID, audit, "failed", "not_found", "job not found"); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return classifyDatabaseError(err)
		}
		return ErrNotFound
	} else if err != nil {
		return classifyDatabaseError(err)
	}
	if status != StatusFailed {
		if err := insertAdminRetryAudit(ctx, tx, jobID, audit, "failed", "conflict", "job is not failed"); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return classifyDatabaseError(err)
		}
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='queued',max_attempts=GREATEST(max_attempts,attempts+$2),
		available_at=clock_timestamp(),finished_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, jobID, additionalAttempts); err != nil {
		return classifyDatabaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='pending',error_code=NULL,error_message=NULL,updated_at=clock_timestamp()
		WHERE job_id=$1 AND status='failed'`, jobID); err != nil {
		return classifyDatabaseError(err)
	}
	if err := insertAdminRetryAudit(ctx, tx, jobID, audit, "succeeded", "", ""); err != nil {
		return err
	}
	return classifyDatabaseError(tx.Commit(ctx))
}

func insertAdminRetryAudit(ctx context.Context, tx pgx.Tx, jobID string, audit AdminAudit, outcome, code, message string) error {
	var errorCode, errorMessage any
	if code != "" {
		errorCode, errorMessage = code, message
	}
	_, err := tx.Exec(ctx, `INSERT INTO admin_audit
		(id,command,actor,host,sanitized_arguments,outcome,affected_ids,error_code,error_message,started_at,finished_at)
		VALUES ($1,'jobs_retry',$2,$3,$4::jsonb,$5,ARRAY[$6::uuid],$7,$8,clock_timestamp(),clock_timestamp())`,
		audit.ID, audit.Actor, audit.Host, audit.SanitizedArguments, outcome, jobID, errorCode, errorMessage)
	return classifyDatabaseError(err)
}

func (r *Repository) withLiveLease(ctx context.Context, jobID, token string, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return classifyDatabaseError(err)
	}
	defer rollback(tx)
	var live bool
	err = tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		FROM jobs WHERE id=$1 FOR UPDATE`, jobID, token).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return ErrLeaseLost
	}
	if err != nil {
		return classifyDatabaseError(err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return classifyDatabaseError(tx.Commit(ctx))
}

func loadTargets(ctx context.Context, tx pgx.Tx, jobID string) ([]Target, error) {
	rows, err := tx.Query(ctx, `SELECT jt.id::text,jt.status,jt.attempts,jt.error_code,jt.error_message,jt.updated_at,
		p.id::text,p.key,p.version,p.processor,p.parameters_schema_version,p.input_mime_types,p.parameters
		FROM job_targets jt JOIN profiles p ON p.id=jt.profile_id
		WHERE jt.job_id=$1 AND jt.status<>'succeeded' ORDER BY p.key,p.version,jt.id`, jobID)
	if err != nil {
		return nil, classifyDatabaseError(err)
	}
	defer rows.Close()
	targets := make([]Target, 0)
	for rows.Next() {
		var target Target
		var code *string
		if err := rows.Scan(&target.ID, &target.Status, &target.Attempts, &code, &target.ErrorMessage, &target.UpdatedAt,
			&target.Profile.ID, &target.Profile.Key, &target.Profile.Version, &target.Profile.Processor,
			&target.Profile.ParametersSchemaVersion, &target.Profile.InputMIMETypes, &target.Profile.Parameters); err != nil {
			return nil, classifyDatabaseError(err)
		}
		if code != nil {
			value := FailureCode(*code)
			target.ErrorCode = &value
		}
		target.UpdatedAt = target.UpdatedAt.UTC()
		target.Profile.Parameters = append(json.RawMessage(nil), target.Profile.Parameters...)
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDatabaseError(err)
	}
	return targets, nil
}

func targetStateError(ctx context.Context, tx pgx.Tx, jobID, targetID string) error {
	var status TargetStatus
	err := tx.QueryRow(ctx, `SELECT status FROM job_targets WHERE id=$1 AND job_id=$2`, targetID, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return classifyDatabaseError(err)
	}
	return ErrConflict
}

func (r *Repository) retryDelay(attempt int) (time.Duration, error) {
	maximum := backoffMaximum(attempt)
	delay := r.jitter(maximum)
	if delay < 0 || delay > maximum {
		return 0, ErrInvalid
	}
	return delay, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func backoffMaximum(attempt int) time.Duration {
	if attempt <= 1 {
		return backoffBase
	}
	maximum := backoffBase
	for i := 1; i < attempt && maximum < backoffCap; i++ {
		if maximum > backoffCap/2 {
			return backoffCap
		}
		maximum *= 2
	}
	if maximum > backoffCap {
		return backoffCap
	}
	return maximum
}

func workerFailure(code FailureCode) (string, bool) {
	if code == FailureLeaseExpired {
		return "", false
	}
	message, ok := safeFailureMessages[code]
	return message, ok
}

func validateTypes(values []Type) ([]string, error) {
	seen := make(map[Type]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		// Purge acquisition is intentionally deferred to Issue #16. Its first
		// claim must lock Media and recheck deletion/cancellation before setting
		// started_at; the generic job-row claim cannot safely provide that.
		if value != TypeTransform {
			return nil, ErrInvalid
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, string(value))
		}
	}
	return result, nil
}

func normalizeLease(lease *Lease) {
	lease.LeaseExpiresAt = lease.LeaseExpiresAt.UTC()
	lease.StartedAt = lease.StartedAt.UTC()
	lease.AvailableAt = lease.AvailableAt.UTC()
	lease.CreatedAt = lease.CreatedAt.UTC()
}

func intervalText(value time.Duration) string {
	return fmt.Sprintf("%d microseconds", value.Microseconds())
}

func jsonObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return len(value) > 0 && json.Unmarshal(value, &object) == nil && object != nil
}

func classifyDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.SafeToRetry(err) {
		return &classifiedError{kind: ErrDatabaseUnavailable, err: err}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return &classifiedError{kind: ErrDatabaseUnavailable, err: err}
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && (strings.HasPrefix(pgError.Code, "08") || strings.HasPrefix(pgError.Code, "40") ||
		strings.HasPrefix(pgError.Code, "53") || pgError.Code == "55P03" || pgError.Code == "57014" || strings.HasPrefix(pgError.Code, "57P0")) {
		return &classifiedError{kind: ErrDatabaseUnavailable, err: err}
	}
	return &classifiedError{kind: ErrInvariant, err: err}
}

func newUUIDv4() (string, error) {
	var id [16]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	encoded := hex.EncodeToString(id[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
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
