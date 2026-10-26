package admin

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
)

// BatchRequest's identity is persisted verbatim. Resume cannot change a target,
// retry allowance, or pinned profile; page size is only an execution parameter.
type BatchRequest struct {
	Operation          string `json:"operation"`
	ProfileKey         string `json:"profile_key,omitempty"`
	MediaID            string `json:"media_id,omitempty"`
	JobID              string `json:"job_id,omitempty"`
	JobType            string `json:"job_type,omitempty"`
	All                bool   `json:"all"`
	AdditionalAttempts int    `json:"additional_attempts,omitempty"`
}

type BatchResult struct {
	Token       string          `json:"resume_token,omitempty"`
	ProfileID   *string         `json:"profile_id,omitempty"`
	Status      string          `json:"status"`
	Total       int64           `json:"total"`
	Succeeded   int64           `json:"succeeded"`
	Failed      int64           `json:"failed"`
	Checkpoint  json.RawMessage `json:"checkpoint"`
	DryRun      bool            `json:"dry_run"`
	CommitState string          `json:"commit_state,omitempty"`
}

func (r BatchRequest) Validate() error {
	switch r.Operation {
	case "regenerate":
		if !profileKeyPattern.MatchString(r.ProfileKey) || r.All == (r.MediaID != "") || r.MediaID != "" && !uuidPattern.MatchString(r.MediaID) || r.JobID != "" || r.JobType != "" {
			return ErrValidation
		}
	case "jobs_retry":
		if r.All == (r.JobID != "") || r.JobID != "" && !uuidPattern.MatchString(r.JobID) || r.AdditionalAttempts < 1 || r.AdditionalAttempts > 1000000 || r.MediaID != "" || r.ProfileKey != "" {
			return ErrValidation
		}
		if r.JobType != "" && r.JobType != "transform" && r.JobType != "purge" {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	return nil
}

func readBatch(ctx context.Context, tx pgx.Tx, token string) (BatchResult, error) {
	var result BatchResult
	err := tx.QueryRow(ctx, `SELECT id::text,profile_id::text,status,total_count,succeeded_count,failed_count,checkpoint
		FROM admin_batches WHERE id=$1`, token).Scan(&result.Token, &result.ProfileID, &result.Status,
		&result.Total, &result.Succeeded, &result.Failed, &result.Checkpoint)
	return result, err
}

// BeginBatch freezes the exact eligible set in a single repeatable-read
// transaction. No server cursor or process-local state is needed after commit.
func (s *Service) BeginBatch(ctx context.Context, request BatchRequest, resume string, dry bool) (BatchResult, error) {
	if err := request.Validate(); err != nil {
		return BatchResult{}, err
	}
	if resume != "" && !uuidPattern.MatchString(resume) {
		return BatchResult{}, ErrValidation
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return BatchResult{}, err
	}
	defer rollback(tx)
	encoded, _ := json.Marshal(request)
	if resume != "" {
		var matching bool
		if err := tx.QueryRow(ctx, `SELECT operation=$2 AND config_snapshot=$3::jsonb FROM admin_batches WHERE id=$1`, resume, request.Operation, encoded).Scan(&matching); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return BatchResult{}, ErrConflict
			}
			return BatchResult{}, err
		}
		if !matching {
			return BatchResult{}, ErrConflict
		}
		result, err := readBatch(ctx, tx, resume)
		result.DryRun = dry
		return result, err
	}
	if err := normal(ctx, tx); err != nil {
		return BatchResult{}, err
	}
	if err := tryLock(ctx, tx, "batch:"+request.Operation); err != nil {
		return BatchResult{}, err
	}
	token := newID()
	var profileID *string
	if request.Operation == "regenerate" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM profiles WHERE key=$1 AND status='active'`, request.ProfileKey).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return BatchResult{}, ErrConflict
			}
			return BatchResult{}, err
		}
		profileID = &id
	}
	// Dry-run executes the SAME eligibility query, but never creates batch,
	// receipt, job, target, audit or checkpoint rows.
	selection := `SELECT m.id AS media_id,NULL::uuid AS source_job_id FROM media m JOIN originals o ON o.media_id=m.id
		JOIN profiles p ON p.id=$1 WHERE m.deleted_at IS NULL AND m.media_type=ANY(p.input_mime_types)
		AND ($2='' OR m.id::text=$2) AND NOT EXISTS (SELECT 1 FROM jobs j JOIN job_targets t ON t.job_id=j.id
		WHERE j.media_id_snapshot=m.id AND t.profile_id=p.id)`
	var selectionArgs []any
	if request.Operation == "regenerate" {
		selectionArgs = []any{profileID, request.MediaID}
	} else {
		selection = `SELECT media_id_snapshot AS media_id,id AS source_job_id FROM jobs
			WHERE status='failed' AND ($1='' OR id::text=$1) AND ($2='' OR type=$2)`
		selectionArgs = []any{request.JobID, request.JobType}
	}
	var total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+selection+`) eligible`, selectionArgs...).Scan(&total); err != nil {
		return BatchResult{}, err
	}
	if dry {
		return BatchResult{Status: "preview", Total: total, ProfileID: profileID, DryRun: true, Checkpoint: json.RawMessage(`{}`)}, nil
	}
	if total == 0 {
		return BatchResult{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admin_batches(id,identity_key,operation,status,profile_id,config_snapshot,high_water,checkpoint,total_count)
		VALUES($1::uuid,$1::text,$2,'running',$3,$4,'{}','{}',$5)`, token, request.Operation, profileID, encoded, total); err != nil {
		return BatchResult{}, err
	}
	args := append(selectionArgs, token)
	if _, err := tx.Exec(ctx, `INSERT INTO admin_batch_items(batch_id,ordinal,media_id,source_job_id)
		SELECT $3,row_number() OVER(ORDER BY media_id,source_job_id),media_id,source_job_id FROM (`+selection+`) eligible`, args...); err != nil {
		return BatchResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_batches SET high_water=(SELECT jsonb_build_object('media_id',media_id,'job_id',source_job_id,'ordinal',ordinal)
		FROM admin_batch_items WHERE batch_id=$1 ORDER BY ordinal DESC LIMIT 1) WHERE id=$1`, token); err != nil {
		return BatchResult{}, err
	}
	if err := s.audit(ctx, tx, request.Operation+"_begin", request, []string{token}, "succeeded", time.Now()); err != nil {
		return BatchResult{}, err
	}
	result, err := readBatch(ctx, tx, token)
	if err != nil {
		return BatchResult{}, err
	}
	if err := s.commitBatch(ctx, tx); err != nil {
		return beginCommitFailure(result, err), classified(err)
	}
	return result, nil
}

// A token is resumable only after durable creation. Transport ambiguity retains
// the identity for reconciliation, but never claims committed membership.
func beginCommitFailure(result BatchResult, err error) BatchResult {
	var rejection *pgconn.PgError
	if errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &rejection) {
		return BatchResult{Status: "not_committed", CommitState: "rejected"}
	}
	return BatchResult{Token: result.Token, Status: "unknown", CommitState: "unknown"}
}

type batchItem struct {
	ordinal int64
	mediaID string
	jobID   *string
}

// BatchPage is the crash boundary. A committed page can never apply twice,
// including retry attempts allowance, even if the response/connection is lost.
func (s *Service) BatchPage(ctx context.Context, token string, limit int) (BatchResult, error) {
	if !uuidPattern.MatchString(token) || limit < 1 || limit > 1000 {
		return BatchResult{}, ErrValidation
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BatchResult{}, err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return BatchResult{}, err
	}
	if err := tryLock(ctx, tx, "batch:"+token); err != nil {
		return BatchResult{}, err
	}
	if err := normal(ctx, tx); err != nil {
		return BatchResult{}, err
	}
	var request BatchRequest
	var encoded []byte
	if err := tx.QueryRow(ctx, `SELECT config_snapshot FROM admin_batches WHERE id=$1 FOR UPDATE NOWAIT`, token).Scan(&encoded); err != nil {
		return BatchResult{}, classified(err)
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		return BatchResult{}, err
	}
	result, err := readBatch(ctx, tx, token)
	if err != nil {
		return result, err
	}
	if result.Status != "running" {
		if result.Failed > 0 {
			return result, ErrPartial
		}
		return result, nil
	}
	previous := result // Last durable state, never the speculative page below.
	rows, err := tx.Query(ctx, `SELECT ordinal,media_id::text,source_job_id::text FROM admin_batch_items
		WHERE batch_id=$1 AND outcome='pending' ORDER BY ordinal LIMIT $2`, token, limit)
	if err != nil {
		return result, err
	}
	var items []batchItem
	for rows.Next() {
		var item batchItem
		if err := rows.Scan(&item.ordinal, &item.mediaID, &item.jobID); err != nil {
			rows.Close()
			return result, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, item := range items {
		if _, err := tx.Exec(ctx, `SAVEPOINT item_effect`); err != nil {
			return result, err
		}
		var resultJobID *string
		if request.Operation == "regenerate" {
			id, effectErr := s.regenerateItem(ctx, tx, item.mediaID, *result.ProfileID)
			err = effectErr
			if id != "" {
				resultJobID = &id
			}
		} else {
			// Purge retry must follow the lifecycle total order: maintenance,
			// Media, then Job. Media may already be physically gone.
			_, err = tx.Exec(ctx, `SELECT id FROM media WHERE id=$1 FOR UPDATE`, item.mediaID)
			if err == nil {
				args, _ := json.Marshal(map[string]any{"batch_id": token, "additional_attempts": request.AdditionalAttempts})
				err = job.AdminRetryFailedTx(ctx, tx, *item.jobID, request.AdditionalAttempts,
					job.AdminAudit{ID: newID(), Actor: s.Actor, Host: s.Host, SanitizedArguments: args})
				resultJobID = item.jobID
			}
		}
		outcome := "succeeded"
		var errorCode *string
		if err != nil {
			// Semantic conflict becomes a durable failed item. Operational
			// failure rolls back the WHOLE page; the exact page is retryable.
			semantic := errors.Is(err, ErrConflict) || errors.Is(err, job.ErrConflict) || errors.Is(err, job.ErrNotFound) || errors.Is(err, job.ErrInvalid)
			if !semantic {
				return result, classified(err)
			}
			if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT item_effect`); rollbackErr != nil {
				return result, rollbackErr
			}
			outcome = "failed"
			code := "not_eligible"
			errorCode = &code
		}
		if _, err := tx.Exec(ctx, `UPDATE admin_batch_items SET outcome=$3,result_job_id=$4,error_code=$5,completed_at=clock_timestamp()
			WHERE batch_id=$1 AND ordinal=$2`, token, item.ordinal, outcome, resultJobID, errorCode); err != nil {
			return result, err
		}
		if err := s.audit(ctx, tx, request.Operation+"_item", map[string]any{"batch_id": token, "ordinal": item.ordinal}, []string{item.mediaID}, outcome, time.Now()); err != nil {
			return result, err
		}
		if _, err := tx.Exec(ctx, `UPDATE admin_batches SET checkpoint=jsonb_build_object('ordinal',$2::bigint,'media_id',$3::uuid,'job_id',$4::uuid),
			succeeded_count=succeeded_count+CASE WHEN $5='succeeded' THEN 1 ELSE 0 END,
			failed_count=failed_count+CASE WHEN $5='failed' THEN 1 ELSE 0 END,updated_at=clock_timestamp() WHERE id=$1`,
			token, item.ordinal, item.mediaID, item.jobID, outcome); err != nil {
			return result, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_batches SET status=CASE WHEN failed_count>0 THEN 'failed' ELSE 'succeeded' END,
		finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND succeeded_count+failed_count=total_count`, token); err != nil {
		return result, err
	}
	result, err = readBatch(ctx, tx, token)
	if err != nil {
		return result, err
	}
	if err := s.commitBatch(ctx, tx); err != nil {
		var serverRejection *pgconn.PgError
		if errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &serverRejection) {
			return previous, err
		}
		// Without positive commit evidence, never expose speculative terminal
		// success. The token is safe to replay and receipts decide the truth.
		previous.CommitState = "unknown"
		previous.Status = "unknown"
		return previous, err
	}
	if result.Failed > 0 && result.Status != "running" {
		return result, ErrPartial
	}
	return result, nil
}

func (s *Service) regenerateItem(ctx context.Context, tx pgx.Tx, mediaID, profileID string) (string, error) {
	var originalID string
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT o.id::text,m.deleted_at IS NULL AND m.media_type=ANY(p.input_mime_types)
		FROM media m JOIN originals o ON o.media_id=m.id CROSS JOIN profiles p WHERE m.id=$1 AND p.id=$2
		FOR UPDATE OF m`, mediaID, profileID).Scan(&originalID, &eligible); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrConflict
		}
		return "", err
	}
	if !eligible {
		return "", ErrConflict
	}
	// Every CLI regenerate serializes on Media; an upload's first targets
	// already exist before that media becomes visible. Profile activation
	// cannot change this batch's persisted ID (retirement is harmless).
	var existing string
	err := tx.QueryRow(ctx, `SELECT j.id::text FROM jobs j JOIN job_targets t ON t.job_id=j.id
		WHERE j.media_id_snapshot=$1 AND t.profile_id=$2 ORDER BY j.id LIMIT 1`, mediaID, profileID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	id := newID()
	if _, err := tx.Exec(ctx, `INSERT INTO jobs(id,type,original_id,media_id_snapshot,status,max_attempts)
		VALUES($1,'transform',$2,$3,'queued',3)`, id, originalID, mediaID); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO job_targets(id,job_id,profile_id,status) VALUES($1,$2,$3,'pending')`, newID(), id, profileID)
	return id, err
}
