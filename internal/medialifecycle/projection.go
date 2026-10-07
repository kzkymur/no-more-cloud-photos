package medialifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func normalizeFileBaseURL(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("file base URL must be an absolute HTTPS files root")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") {
		return "", errors.New("file base URL must be an absolute HTTPS files root without credentials, query, or fragment")
	}
	canonicalEscapedPath := (&url.URL{Path: parsed.Path}).EscapedPath()
	trimmed := strings.TrimSuffix(parsed.Path, "/")
	if trimmed == "" || parsed.EscapedPath() != canonicalEscapedPath || strings.Contains(trimmed, "//") || strings.Contains(trimmed, "\\") || strings.HasSuffix(parsed.Path, "//") {
		return "", errors.New("file base URL must have an unambiguous non-root path")
	}
	for _, character := range parsed.Path {
		if character < 0x20 || character == 0x7f {
			return "", errors.New("file base URL must not contain control characters")
		}
	}
	for _, segment := range strings.Split(strings.TrimPrefix(trimmed, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("file base URL must not contain empty or dot path segments")
		}
	}
	parsed.Path, parsed.RawPath = trimmed+"/", ""
	return parsed.String(), nil
}

func (r *PostgresRepository) mediaDetail(ctx context.Context, tx pgx.Tx, mediaID string) (readapi.MediaDetail, error) {
	result := readapi.NewMediaDetail()
	var originalID *string
	if err := tx.QueryRow(ctx, mediaDetailSQL, mediaID).Scan(
		&result.ID, &result.MIMEType, &result.OriginalFilename, &result.SizeBytes, &result.Width, &result.Height,
		&result.DurationMS, &result.TakenAt, &result.TakenAtSource, &result.TakenAtTimezone, &result.CreatedAt,
		&result.DeletedAt, &result.PurgeAfter, &originalID,
	); err != nil {
		return readapi.MediaDetail{}, fmt.Errorf("read media detail: %w", err)
	}
	if originalID == nil || !readapi.IsUUIDv4(result.ID) || !readapi.IsUUIDv4(*originalID) || result.SizeBytes < 0 ||
		(result.Width == nil) != (result.Height == nil) || result.DurationMS != nil && *result.DurationMS < 0 ||
		result.PurgeAfter != nil && result.DeletedAt == nil || !validCapture(result.TakenAt, result.TakenAtSource, result.TakenAtTimezone) {
		return readapi.MediaDetail{}, newInvariant(errors.New("inconsistent media detail"))
	}
	normalizeMedia(&result)
	renditions, err := r.currentRenditions(ctx, tx, mediaID)
	if err != nil {
		return readapi.MediaDetail{}, err
	}
	jobs, err := loadJobs(ctx, tx, mediaID)
	if err != nil {
		return readapi.MediaDetail{}, err
	}
	result.CurrentRenditions, result.Jobs = renditions, jobs
	result.InitializeArrays()
	return result, nil
}

func (r *PostgresRepository) currentRenditions(ctx context.Context, tx pgx.Tx, mediaID string) ([]readapi.Rendition, error) {
	rows, err := tx.Query(ctx, currentRenditionsSQL, mediaID)
	if err != nil {
		return nil, fmt.Errorf("read current renditions: %w", err)
	}
	defer rows.Close()
	result := make([]readapi.Rendition, 0)
	seenProfileKeys := make(map[string]struct{})
	for rows.Next() {
		var rendition readapi.Rendition
		var originalID, storedProfileKey, relativePath string
		if err := rows.Scan(&rendition.ID, &rendition.MediaID, &rendition.JobTargetID, &originalID,
			&storedProfileKey, &rendition.Profile.ID, &rendition.Profile.Key, &rendition.Profile.Version, &rendition.MIMEType,
			&rendition.SizeBytes, &rendition.Width, &rendition.Height, &rendition.DurationMS, &rendition.SHA256,
			&relativePath, &rendition.CreatedAt); err != nil {
			return nil, newInvariant(errors.New("invalid current rendition row"))
		}
		key, err := storage.ParseRenditionKey(relativePath)
		if err != nil || !readapi.IsUUIDv4(rendition.ID) || !readapi.IsUUIDv4(rendition.MediaID) ||
			!readapi.IsUUIDv4(rendition.JobTargetID) || !readapi.IsUUIDv4(originalID) || !readapi.IsUUIDv4(rendition.Profile.ID) ||
			!validProfileKey(storedProfileKey) || storedProfileKey != rendition.Profile.Key || rendition.Profile.Version < 1 ||
			!validSHA256(rendition.SHA256) || rendition.SizeBytes < 0 ||
			(rendition.Width == nil) != (rendition.Height == nil) || rendition.DurationMS != nil && *rendition.DurationMS < 0 ||
			key.OriginalID().String() != originalID || key.JobTargetID().String() != rendition.JobTargetID ||
			key.RenditionID().String() != rendition.ID || rendition.MediaID != mediaID {
			return nil, newInvariant(errors.New("inconsistent current rendition"))
		}
		if _, duplicate := seenProfileKeys[storedProfileKey]; duplicate {
			return nil, newInvariant(errors.New("duplicate current rendition key"))
		}
		seenProfileKeys[storedProfileKey] = struct{}{}
		rendition.FileURL = r.fileBaseURL + key.String()
		rendition.CreatedAt = rendition.CreatedAt.UTC()
		result = append(result, rendition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read current rendition rows: %w", err)
	}
	return result, nil
}

func loadJobs(ctx context.Context, tx pgx.Tx, mediaID string) ([]readapi.Job, error) {
	rows, err := tx.Query(ctx, mediaJobsSQL, mediaID)
	if err != nil {
		return nil, fmt.Errorf("read media jobs: %w", err)
	}
	jobs := make([]readapi.Job, 0)
	seenJobs := make(map[string]struct{})
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if _, duplicate := seenJobs[job.ID]; duplicate {
			rows.Close()
			return nil, newInvariant(errors.New("duplicate job row"))
		}
		seenJobs[job.ID] = struct{}{}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read media job rows: %w", err)
	}
	rows.Close()
	if len(jobs) == 0 {
		return jobs, nil
	}
	ids := make([]string, len(jobs))
	indexes := make(map[string]int, len(jobs))
	for index := range jobs {
		ids[index], indexes[jobs[index].ID] = jobs[index].ID, index
	}
	targetRows, err := tx.Query(ctx, jobTargetsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("read media job targets: %w", err)
	}
	defer targetRows.Close()
	seenTargets := make(map[string]struct{})
	for targetRows.Next() {
		var target readapi.JobTarget
		var jobID string
		var errorCode, errorMessage *string
		if err := targetRows.Scan(&target.ID, &jobID, &target.Profile.ID, &target.Profile.Key, &target.Profile.Version,
			&target.Status, &target.Attempts, &errorCode, &errorMessage, &target.RenditionID, &target.UpdatedAt); err != nil {
			return nil, newInvariant(errors.New("invalid job target row"))
		}
		index, ok := indexes[jobID]
		if !ok || !readapi.IsUUIDv4(target.ID) || !readapi.IsUUIDv4(jobID) || !readapi.IsUUIDv4(target.Profile.ID) ||
			!validProfileKey(target.Profile.Key) || target.Profile.Version < 1 || !validTargetStatus(target.Status) ||
			target.Attempts < 0 || target.RenditionID != nil && !readapi.IsUUIDv4(*target.RenditionID) ||
			(errorCode == nil) != (errorMessage == nil) || target.Status != readapi.TargetFailed && errorCode != nil {
			return nil, newInvariant(errors.New("inconsistent job target row"))
		}
		if _, duplicate := seenTargets[target.ID]; duplicate {
			return nil, newInvariant(errors.New("duplicate job target row"))
		}
		seenTargets[target.ID] = struct{}{}
		if errorCode != nil {
			target.Error = &readapi.ResourceError{Code: *errorCode, Message: *errorMessage}
		}
		target.UpdatedAt = target.UpdatedAt.UTC()
		jobs[index].Targets = append(jobs[index].Targets, target)
	}
	if err := targetRows.Err(); err != nil {
		return nil, fmt.Errorf("read media job target rows: %w", err)
	}
	for index := range jobs {
		if jobs[index].Type == readapi.JobPurge && len(jobs[index].Targets) != 0 ||
			jobs[index].Type == readapi.JobTransform && len(jobs[index].Targets) == 0 {
			return nil, newInvariant(errors.New("job target cardinality is inconsistent"))
		}
	}
	return jobs, nil
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (readapi.Job, error) {
	job := readapi.NewJob()
	var errorCode, errorMessage *string
	if err := row.Scan(&job.ID, &job.Type, &job.Status, &job.MediaID, &job.OriginalID, &job.Attempts,
		&job.MaxAttempts, &job.AvailableAt, &job.StartedAt, &job.FinishedAt, &errorCode, &errorMessage,
		&job.CancelledAt, &job.CancelReason, &job.CreatedAt, &job.UpdatedAt); err != nil {
		return readapi.Job{}, newInvariant(errors.New("invalid job row"))
	}
	if !readapi.IsUUIDv4(job.ID) || !readapi.IsUUIDv4(job.MediaID) ||
		(job.OriginalID != nil && !readapi.IsUUIDv4(*job.OriginalID)) || !job.Status.Valid() ||
		(job.Type != readapi.JobTransform && job.Type != readapi.JobPurge) ||
		(job.Type == readapi.JobPurge && job.OriginalID != nil) || job.Attempts < 0 ||
		job.MaxAttempts < job.Attempts || job.MaxAttempts < 1 || (errorCode == nil) != (errorMessage == nil) {
		return readapi.Job{}, newInvariant(errors.New("inconsistent job row"))
	}
	if errorCode != nil {
		job.Error = &readapi.ResourceError{Code: *errorCode, Message: *errorMessage}
	}
	normalizeJob(&job)
	return job, nil
}

func normalizeMedia(media *readapi.MediaDetail) {
	media.CreatedAt = media.CreatedAt.UTC()
	utcPointer(&media.TakenAt)
	utcPointer(&media.DeletedAt)
	utcPointer(&media.PurgeAfter)
}

func normalizeJob(job *readapi.Job) {
	job.AvailableAt, job.CreatedAt, job.UpdatedAt = job.AvailableAt.UTC(), job.CreatedAt.UTC(), job.UpdatedAt.UTC()
	utcPointer(&job.StartedAt)
	utcPointer(&job.FinishedAt)
	utcPointer(&job.CancelledAt)
}

func utcPointer(value **time.Time) {
	if *value != nil {
		utc := (*value).UTC()
		*value = &utc
	}
}

func validCapture(takenAt *time.Time, source readapi.TakenAtSource, timezone *string) bool {
	switch source {
	case readapi.TakenAtEmbeddedOffset:
		return takenAt != nil && timezone == nil
	case readapi.TakenAtDefaultTimezone:
		return takenAt != nil && timezone != nil
	case readapi.TakenAtUnknown:
		return takenAt == nil && timezone == nil
	default:
		return false
	}
}

func validProfileKey(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '_' && character != '-' {
					return false
				}
			}
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validTargetStatus(status readapi.JobTargetStatus) bool {
	return status == readapi.TargetPending || status == readapi.TargetSucceeded || status == readapi.TargetFailed
}

type eventMedia struct {
	ID                string                `json:"id"`
	MIMEType          string                `json:"mime_type"`
	OriginalFilename  *string               `json:"original_filename"`
	SizeBytes         int64                 `json:"size_bytes"`
	Width             *int                  `json:"width"`
	Height            *int                  `json:"height"`
	DurationMS        *int64                `json:"duration_ms"`
	TakenAt           *time.Time            `json:"taken_at"`
	TakenAtSource     readapi.TakenAtSource `json:"taken_at_source"`
	TakenAtTimezone   *string               `json:"taken_at_timezone"`
	CreatedAt         time.Time             `json:"created_at"`
	DeletedAt         *time.Time            `json:"deleted_at"`
	PurgeAfter        *time.Time            `json:"purge_after"`
	CurrentRenditions []readapi.Rendition   `json:"current_renditions"`
}

func mediaEventSnapshot(media readapi.MediaDetail) eventMedia {
	return eventMedia{
		ID: media.ID, MIMEType: media.MIMEType, OriginalFilename: media.OriginalFilename, SizeBytes: media.SizeBytes,
		Width: media.Width, Height: media.Height, DurationMS: media.DurationMS, TakenAt: media.TakenAt,
		TakenAtSource: media.TakenAtSource, TakenAtTimezone: media.TakenAtTimezone, CreatedAt: media.CreatedAt,
		DeletedAt: media.DeletedAt, PurgeAfter: media.PurgeAfter, CurrentRenditions: media.CurrentRenditions,
	}
}

const mediaDetailSQL = `
	SELECT m.id::text,m.media_type,o.original_filename,o.size_bytes,o.width,o.height,o.duration_ms,
	       m.taken_at,m.taken_at_source,m.taken_at_timezone,m.created_at,m.deleted_at,m.purge_after,o.id::text
	FROM media m LEFT JOIN originals o ON o.media_id=m.id WHERE m.id=$1`

const currentRenditionsSQL = `
	SELECT r.id::text,r.media_id::text,r.job_target_id::text,j.original_id::text,r.profile_key,p.id::text,p.key,p.version,
	       r.mime_type,r.size_bytes,r.width,r.height,r.duration_ms,r.sha256,r.relative_path,r.created_at
	FROM renditions r
	LEFT JOIN job_targets jt ON jt.id=r.job_target_id LEFT JOIN jobs j ON j.id=jt.job_id LEFT JOIN profiles p ON p.id=jt.profile_id
	WHERE r.media_id=$1 AND r.is_current ORDER BY r.profile_key`

const mediaJobsSQL = `
	SELECT id::text,type,status,media_id_snapshot::text,original_id::text,attempts,max_attempts,
	       available_at,started_at,finished_at,error_code,error_message,cancelled_at,cancel_reason,created_at,updated_at
	FROM jobs WHERE media_id_snapshot=$1 ORDER BY created_at DESC,id DESC LIMIT 100`

const jobTargetsSQL = `
	SELECT jt.id::text,jt.job_id::text,p.id::text,p.key,p.version,jt.status,jt.attempts,
	       jt.error_code,jt.error_message,r.id::text,jt.updated_at
	FROM job_targets jt LEFT JOIN profiles p ON p.id=jt.profile_id LEFT JOIN renditions r ON r.job_target_id=jt.id
	WHERE jt.job_id=ANY($1::uuid[]) ORDER BY jt.job_id,p.key,p.version DESC,jt.id`
