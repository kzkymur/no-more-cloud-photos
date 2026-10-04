package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/metadata"
)

const transformMaxAttempts = 3

const rollbackCleanupBudget = 5 * time.Second

var errTimezoneChanged = errors.New("default timezone changed")

type database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type acceptanceRepository interface {
	DefaultTimezone(context.Context) (string, error)
	Finalize(context.Context, acceptance, func() (string, error)) (Outcome, error)
}

type pgRepository struct {
	db    database
	newID func() (string, error)
}

func newPGRepository(pool *pgxpool.Pool) *pgRepository {
	return &pgRepository{db: pool, newID: NewUUIDv4}
}

type acceptance struct {
	Key         string
	RequestID   string
	RequestHash [sha256.Size]byte
	SHA256      [sha256.Size]byte
	Size        int64
	Filename    *string
	OriginalID  string
	MediaID     string
	Timezone    string
	Metadata    metadata.Result
	EXIFJSON    json.RawMessage
	SourceJSON  json.RawMessage
}

type profileSnapshot struct {
	ID      string
	Key     string
	Version int
}

func (r *pgRepository) DefaultTimezone(ctx context.Context) (string, error) {
	var timezone string
	if err := r.db.QueryRow(ctx, `SELECT default_timezone FROM system_config WHERE id = 1`).Scan(&timezone); err != nil {
		return "", fmt.Errorf("read default timezone: %w", err)
	}
	return timezone, nil
}

func (r *pgRepository) Finalize(ctx context.Context, input acceptance, publish func() (string, error)) (Outcome, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("begin upload acceptance: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackCleanupBudget)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	var maintenanceMode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM maintenance_state WHERE id = 1 FOR SHARE`).Scan(&maintenanceMode); err != nil {
		return Outcome{}, fmt.Errorf("lock maintenance state: %w", err)
	}
	if maintenanceMode != "normal" {
		return Outcome{}, &Failure{Status: 503, Code: "unavailable", Message: "service is temporarily unavailable"}
	}

	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1)`, IdempotencyAdvisoryLock(IdempotencyScopeMediaUpload, input.Key)); err != nil {
		return Outcome{}, fmt.Errorf("lock idempotency key: %w", err)
	}
	requestHash := hex.EncodeToString(input.RequestHash[:])
	var storedHash string
	var storedStatus int
	var storedBody []byte
	err = tx.QueryRow(ctx, `
		SELECT request_hash, http_status, response_body
		FROM idempotency_requests WHERE scope = $1 AND key = $2`,
		IdempotencyScopeMediaUpload, input.Key).Scan(&storedHash, &storedStatus, &storedBody)
	if err == nil {
		if storedHash != requestHash {
			body := marshalResponse(errorResponse{Error: errorDetail{
				Code: "idempotency_conflict", Message: "idempotency key was used with different content",
				RequestID: input.RequestID, Details: map[string]any{"original_request_hash": storedHash},
			}})
			return Outcome{Status: 409, Body: body}, nil
		}
		return Outcome{Status: storedStatus, Body: json.RawMessage(storedBody), Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, fmt.Errorf("read idempotency result: %w", err)
	}

	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1)`, ContentSHA256AdvisoryLock(input.SHA256)); err != nil {
		return Outcome{}, fmt.Errorf("lock original digest: %w", err)
	}
	shaText := hex.EncodeToString(input.SHA256[:])
	var existingMediaID string
	var existingDeleted bool
	err = tx.QueryRow(ctx, `
		SELECT o.media_id::text, m.deleted_at IS NOT NULL
		FROM originals AS o JOIN media AS m ON m.id = o.media_id
		WHERE o.sha256 = $1`, shaText).Scan(&existingMediaID, &existingDeleted)
	if err == nil {
		body := marshalResponse(errorResponse{Error: errorDetail{
			Code: "duplicate_media", Message: "original content already exists", RequestID: input.RequestID,
			Details: map[string]any{"existing_media_id": existingMediaID, "deleted": existingDeleted},
		}})
		if err := insertIdempotency(ctx, tx, input.Key, requestHash, 409, body, &existingMediaID); err != nil {
			return Outcome{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Outcome{}, &OutcomeUnknown{Cause: err}
		}
		return Outcome{Status: 409, Body: body}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, fmt.Errorf("check original digest: %w", err)
	}

	var lockedTimezone string
	if err := tx.QueryRow(ctx, `SELECT default_timezone FROM system_config WHERE id = 1 FOR SHARE`).Scan(&lockedTimezone); err != nil {
		return Outcome{}, fmt.Errorf("lock default timezone: %w", err)
	}
	if lockedTimezone != input.Timezone {
		return Outcome{}, &timezoneChangedError{Timezone: lockedTimezone}
	}

	profiles, err := matchingProfiles(ctx, tx, input.Metadata.MIMEType)
	if err != nil {
		return Outcome{}, err
	}
	var acceptedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&acceptedAt); err != nil {
		return Outcome{}, fmt.Errorf("read acceptance timestamp: %w", err)
	}

	relativePath, err := publish()
	if err != nil {
		return Outcome{}, err
	}

	width, height := input.Metadata.Width, input.Metadata.Height
	media := mediaSummary{
		ID: input.MediaID, MIMEType: input.Metadata.MIMEType, OriginalFilename: input.Filename,
		SizeBytes: input.Size, Width: &width, Height: &height, DurationMS: input.Metadata.DurationMS,
		TakenAt: input.Metadata.Derived.Capture.TakenAt, TakenAtSource: input.Metadata.Derived.Capture.Source,
		TakenAtTimezone: input.Metadata.Derived.Capture.Timezone, CreatedAt: acceptedAt,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO media (id, media_type, source_metadata, taken_at, taken_at_source, taken_at_timezone, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, input.MediaID, input.Metadata.MIMEType, input.SourceJSON,
		media.TakenAt, media.TakenAtSource, media.TakenAtTimezone, acceptedAt); err != nil {
		return Outcome{}, fmt.Errorf("insert media: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO originals (id, media_id, sha256, original_filename, relative_path, mime_type,
			size_bytes, width, height, duration_ms, created_at, exif_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, input.OriginalID, input.MediaID, shaText,
		input.Filename, relativePath, input.Metadata.MIMEType, input.Size, width, height,
		input.Metadata.DurationMS, acceptedAt, input.EXIFJSON); err != nil {
		return Outcome{}, fmt.Errorf("insert original: %w", err)
	}

	job, err := r.insertTransformJob(ctx, tx, input, profiles, acceptedAt)
	if err != nil {
		return Outcome{}, err
	}
	body := marshalResponse(uploadResponse{Media: media, Job: job})
	eventPayload := marshalResponse(mediaDetailEvent{
		ID: media.ID, MIMEType: media.MIMEType, OriginalFilename: media.OriginalFilename,
		SizeBytes: media.SizeBytes, Width: media.Width, Height: media.Height, DurationMS: media.DurationMS,
		TakenAt: media.TakenAt, TakenAtSource: media.TakenAtSource, TakenAtTimezone: media.TakenAtTimezone,
		CreatedAt: media.CreatedAt, CurrentRenditions: []any{},
	})
	if err := r.insertChangeEvent(ctx, tx, input.MediaID, eventPayload, acceptedAt); err != nil {
		return Outcome{}, err
	}
	if err := insertIdempotency(ctx, tx, input.Key, requestHash, 201, body, &input.MediaID); err != nil {
		return Outcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Outcome{}, &OutcomeUnknown{Cause: err}
	}
	return Outcome{Status: 201, Body: body}, nil
}

type timezoneChangedError struct{ Timezone string }

func (e *timezoneChangedError) Error() string { return errTimezoneChanged.Error() }
func (e *timezoneChangedError) Unwrap() error { return errTimezoneChanged }

func matchingProfiles(ctx context.Context, tx pgx.Tx, mimeType string) ([]profileSnapshot, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, key, version FROM profiles
		WHERE status = 'active' AND $1 = ANY(input_mime_types)
		ORDER BY key ASC, version DESC, id ASC`, mimeType)
	if err != nil {
		return nil, fmt.Errorf("select active profiles: %w", err)
	}
	defer rows.Close()
	profiles := make([]profileSnapshot, 0)
	for rows.Next() {
		var profile profileSnapshot
		if err := rows.Scan(&profile.ID, &profile.Key, &profile.Version); err != nil {
			return nil, fmt.Errorf("scan active profile: %w", err)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active profiles: %w", err)
	}
	return profiles, nil
}

func (r *pgRepository) insertTransformJob(ctx context.Context, tx pgx.Tx, input acceptance, profiles []profileSnapshot, now time.Time) (*jobResponse, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	jobID, err := r.newID()
	if err != nil {
		return nil, fmt.Errorf("generate job ID: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (id,type,original_id,media_id_snapshot,status,attempts,max_attempts,available_at,created_at,updated_at)
		VALUES ($1,'transform',$2,$3,'queued',0,$4,$5,$5,$5)`,
		jobID, input.OriginalID, input.MediaID, transformMaxAttempts, now); err != nil {
		return nil, fmt.Errorf("insert transform job: %w", err)
	}
	targets := make([]targetResponse, 0, len(profiles))
	for _, profile := range profiles {
		targetID, err := r.newID()
		if err != nil {
			return nil, fmt.Errorf("generate target ID: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_targets (id,job_id,profile_id,status,attempts,updated_at)
			VALUES ($1,$2,$3,'pending',0,$4)`, targetID, jobID, profile.ID, now); err != nil {
			return nil, fmt.Errorf("insert job target: %w", err)
		}
		targets = append(targets, targetResponse{
			ID: targetID, Profile: profileResponse(profile), Status: "pending", Error: nil, UpdatedAt: now,
		})
	}
	originalID := input.OriginalID
	return &jobResponse{
		ID: jobID, Type: "transform", Status: "queued", MediaID: input.MediaID, OriginalID: &originalID,
		MaxAttempts: transformMaxAttempts, AvailableAt: now, CreatedAt: now, UpdatedAt: now, Error: nil, Targets: targets,
	}, nil
}

func (r *pgRepository) insertChangeEvent(ctx context.Context, tx pgx.Tx, mediaID string, payload json.RawMessage, now time.Time) error {
	var position int64
	if err := tx.QueryRow(ctx, `
		UPDATE change_feed_state SET last_position = last_position + 1 WHERE id = 1
		RETURNING last_position`).Scan(&position); err != nil {
		return fmt.Errorf("allocate change position: %w", err)
	}
	eventID, err := r.newID()
	if err != nil {
		return fmt.Errorf("generate event ID: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO change_events (id,position,event_type,reason,media_id,payload,occurred_at)
		VALUES ($1,$2,'media_upsert','upload',$3,$4,$5)`, eventID, position, mediaID, payload, now); err != nil {
		return fmt.Errorf("insert upload event: %w", err)
	}
	return nil
}

func insertIdempotency(ctx context.Context, tx pgx.Tx, key, requestHash string, status int, body json.RawMessage, mediaID *string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO idempotency_requests (scope,key,request_hash,http_status,response_body,media_id_snapshot)
		VALUES ($1,$2,$3,$4,$5,$6)`, IdempotencyScopeMediaUpload, key, requestHash, status, body, mediaID); err != nil {
		return fmt.Errorf("insert idempotency result: %w", err)
	}
	return nil
}

func marshalResponse(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
