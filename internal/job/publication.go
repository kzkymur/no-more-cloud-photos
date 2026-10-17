package job

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	profiledefinition "github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

var mimePattern = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)

// PublishRendition atomically references an already durable rendition file,
// completes its target, and promotes it when its pinned profile version wins.
func (r *Repository) PublishRendition(ctx context.Context, candidate Rendition) (Publication, error) {
	return r.publishRendition(ctx, candidate, nil)
}

// PublishRenditionFile holds the maintenance, Media, Job, and Target locks
// across durable filesystem publication and the database reference commit.
func (r *Repository) PublishRenditionFile(ctx context.Context, candidate Rendition, publish func() (int64, string, error)) (Publication, error) {
	if publish == nil {
		return Publication{}, ErrInvalid
	}
	return r.publishRendition(ctx, candidate, publish)
}

func (r *Repository) publishRendition(ctx context.Context, candidate Rendition, publish func() (int64, string, error)) (Publication, error) {
	if publish == nil && !validRendition(candidate) || publish != nil && !validRenditionIdentity(candidate) {
		return Publication{}, ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	defer rollback(tx)

	// This row share lock is the outer publication fence. Entering maintenance
	// must drain it before repair can revalidate or move any path.
	if _, err := tx.Exec(ctx, `SELECT nmcp_require_normal_maintenance()`); err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "55000" {
			return Publication{}, ErrMaintenance
		}
		return Publication{}, classifyDatabaseError(err)
	}

	// Media is always the first database lock. Purge/delete operations use the
	// same parent lock, making the deleted check and publication indivisible.
	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM media WHERE id=$1 FOR UPDATE`, candidate.MediaID).Scan(&deletedAt); errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	} else if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	if deletedAt != nil {
		return Publication{}, ErrConflict
	}

	var jobType Type
	var jobStatus Status
	var originalID, mediaID string
	var token *string
	var leaseLive bool
	err = tx.QueryRow(ctx, `SELECT type,status,original_id::text,media_id_snapshot::text,lease_token::text,
		COALESCE(lease_expires_at>clock_timestamp(),false) FROM jobs WHERE id=$1 FOR UPDATE`, candidate.JobID).Scan(
		&jobType, &jobStatus, &originalID, &mediaID, &token, &leaseLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}

	var targetStatus TargetStatus
	var profileStatus string
	var pinned Profile
	err = tx.QueryRow(ctx, `SELECT jt.status,p.id::text,p.key,p.version,p.status,p.processor,
		p.parameters_schema_version,p.input_mime_types,p.parameters
		FROM job_targets jt JOIN profiles p ON p.id=jt.profile_id
		WHERE jt.job_id=$1 AND jt.id=$2 FOR UPDATE OF jt`, candidate.JobID, candidate.TargetID).Scan(
		&targetStatus,
		&pinned.ID, &pinned.Key, &pinned.Version, &profileStatus, &pinned.Processor,
		&pinned.ParametersSchemaVersion, &pinned.InputMIMETypes, &pinned.Parameters)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	if jobType != TypeTransform || originalID != candidate.OriginalID || mediaID != candidate.MediaID || pinned.ID != candidate.ProfileID {
		return Publication{}, ErrConflict
	}
	if jobStatus != StatusRunning || token == nil || *token != candidate.LeaseToken || !leaseLive {
		return Publication{}, ErrLeaseLost
	}
	if targetStatus != TargetPending {
		return Publication{}, ErrConflict
	}
	var originalMediaID, originalMIME string
	if err := tx.QueryRow(ctx, `SELECT media_id::text,mime_type FROM originals WHERE id=$1`, candidate.OriginalID).Scan(&originalMediaID, &originalMIME); errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrConflict
	} else if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	if originalMediaID != candidate.MediaID {
		return Publication{}, ErrConflict
	}
	if err := validatePinnedOutput(pinned, profileStatus, originalMIME, candidate.MIMEType, candidate.RelativePath); err != nil {
		return Publication{}, err
	}
	if publish != nil {
		sizeBytes, sha256, err := publish()
		if err != nil {
			return Publication{}, err
		}
		candidate.SizeBytes, candidate.SHA256 = sizeBytes, sha256
		if !validRendition(candidate) {
			return Publication{}, ErrInvariant
		}
	}

	var retentionDays *int
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT superseded_rendition_retention_days,clock_timestamp()
		FROM system_config WHERE id=1 FOR SHARE`).Scan(&retentionDays, &databaseNow); err != nil {
		return Publication{}, classifyDatabaseError(err)
	}

	var currentID string
	var currentVersion int
	err = tx.QueryRow(ctx, `SELECT r.id::text,p.version FROM renditions r
		JOIN job_targets jt ON jt.id=r.job_target_id JOIN profiles p ON p.id=jt.profile_id
		WHERE r.media_id=$1 AND r.profile_key=$2 AND r.is_current`, candidate.MediaID, pinned.Key).Scan(&currentID, &currentVersion)
	hasCurrent := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, classifyDatabaseError(err)
	}
	promote := !hasCurrent || pinned.Version >= currentVersion
	if promote && hasCurrent {
		if _, err := tx.Exec(ctx, `UPDATE renditions SET is_current=false,
			purge_after=CASE WHEN $2::integer IS NULL THEN NULL ELSE $3::timestamptz+$2*interval '1 day' END
			WHERE id=$1`, currentID, retentionDays, databaseNow); err != nil {
			return Publication{}, classifyDatabaseError(err)
		}
	}

	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO renditions
		(id,media_id,job_target_id,profile_key,is_current,purge_after,relative_path,mime_type,width,height,duration_ms,size_bytes,sha256,processor_audit)
		VALUES ($1,$2,$3,$4,$5,
			CASE WHEN $5::boolean THEN NULL WHEN $6::integer IS NULL THEN NULL ELSE $7::timestamptz+$6*interval '1 day' END,
			$8,$9,$10,$11,$12,$13,$14,$15::jsonb) RETURNING created_at`,
		candidate.ID, candidate.MediaID, candidate.TargetID, pinned.Key, promote, retentionDays, databaseNow,
		candidate.RelativePath, candidate.MIMEType, candidate.Width, candidate.Height, candidate.DurationMS,
		candidate.SizeBytes, candidate.SHA256, candidate.ProcessorAudit).Scan(&createdAt)
	if err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE job_targets SET status='succeeded',error_code=NULL,error_message=NULL,
		updated_at=clock_timestamp() WHERE id=$1 AND job_id=$2 AND status='pending'`, candidate.TargetID, candidate.JobID); err != nil {
		return Publication{}, classifyDatabaseError(err)
	}

	var incomplete bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM job_targets WHERE job_id=$1 AND status<>'succeeded')`, candidate.JobID).Scan(&incomplete); err != nil {
		return Publication{}, classifyDatabaseError(err)
	}
	jobFinished := !incomplete
	if promote {
		payload, err := r.mediaEventPayload(ctx, tx, candidate.MediaID)
		if err != nil {
			return Publication{}, err
		}
		eventID, err := r.uuid()
		if err != nil {
			return Publication{}, err
		}
		var position int64
		if err := tx.QueryRow(ctx, `UPDATE change_feed_state SET last_position=last_position+1 WHERE id=1 RETURNING last_position`).Scan(&position); err != nil {
			return Publication{}, classifyDatabaseError(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO change_events
			(id,position,event_type,reason,media_id,payload,occurred_at)
			VALUES ($1,$2,'media_upsert','rendition_current',$3,$4::jsonb,$5)`,
			eventID, position, candidate.MediaID, payload, createdAt); err != nil {
			return Publication{}, classifyDatabaseError(err)
		}
	}
	if r.checkpoint != nil {
		if err := r.checkpoint(ctx, storage.BoundaryBeforeDBCommit, candidate.RelativePath); err != nil {
			return Publication{}, err
		}
	}
	// The change-feed position and checkpoint can block after the initial lease
	// check. Keep the terminal CAS, or the partial-attempt recheck, as the last
	// ownership decision before commit so expired work rolls back atomically.
	if jobFinished {
		tag, err := tx.Exec(ctx, `UPDATE jobs SET status='succeeded',lease_token=NULL,lease_expires_at=NULL,
			finished_at=clock_timestamp(),error_code=NULL,error_message=NULL,updated_at=clock_timestamp()
			WHERE id=$1 AND status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()`, candidate.JobID, candidate.LeaseToken)
		if err != nil {
			return Publication{}, classifyDatabaseError(err)
		}
		if tag.RowsAffected() != 1 {
			return Publication{}, ErrLeaseLost
		}
	} else if err := tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$2 AND lease_expires_at>clock_timestamp()
		FROM jobs WHERE id=$1`, candidate.JobID, candidate.LeaseToken).Scan(&leaseLive); err != nil {
		return Publication{}, classifyDatabaseError(err)
	} else if !leaseLive {
		return Publication{}, ErrLeaseLost
	}
	if err := commitPublication(ctx, tx); err != nil {
		return Publication{}, err
	}
	if r.checkpoint != nil {
		if err := r.checkpoint(ctx, storage.BoundaryAfterDBCommit, candidate.RelativePath); err != nil {
			return Publication{}, &CommitOutcomeUnknown{Cause: err}
		}
	}
	return Publication{RenditionID: candidate.ID, Current: promote, JobFinished: jobFinished, CreatedAt: createdAt.UTC()}, nil
}

func validRenditionIdentity(candidate Rendition) bool {
	candidate.SizeBytes = 0
	candidate.SHA256 = strings.Repeat("0", 64)
	return validRendition(candidate)
}

func validatePinnedOutput(pinned Profile, status, originalMIME, candidateMIME, relativePath string) error {
	if status != "active" && status != "retired" {
		return ErrInvariant
	}
	definition := profiledefinition.Definition{
		ID: pinned.ID, Key: pinned.Key, Version: pinned.Version, InputMIMETypes: pinned.InputMIMETypes,
		Processor: pinned.Processor, ParametersSchemaVersion: pinned.ParametersSchemaVersion,
		Parameters: pinned.Parameters,
	}
	if err := profiledefinition.ValidateDraft(definition); err != nil {
		return ErrInvariant
	}
	parameters, err := profiledefinition.DecodeParameters(pinned.Parameters)
	if err != nil {
		return ErrInvariant
	}
	recipe, ok := parameters.Recipes[originalMIME]
	if !ok {
		return ErrInvariant
	}
	wantMIME, wantSuffix := "", ""
	switch {
	case recipe.SourceMode == profiledefinition.SourceStill && recipe.FramePolicy == profiledefinition.FrameFirst && recipe.StillOutput != nil:
		wantMIME, wantSuffix = "image/avif", ".avif"
	case recipe.SourceMode == profiledefinition.SourceProbeAnimation && recipe.FramePolicy == profiledefinition.FrameAll && recipe.AnimationOutput != nil:
		// A probe-animation WebP may be static. The trusted executor routes that
		// classification through the still processor even for an all-frame recipe.
		if originalMIME == "image/webp" && candidateMIME == "image/avif" && strings.HasSuffix(relativePath, ".avif") {
			return nil
		}
		wantMIME, wantSuffix = "image/webp", ".webp"
	case recipe.SourceMode == profiledefinition.SourceProbeAnimation && recipe.FramePolicy == profiledefinition.FrameFirst && recipe.StillOutput != nil:
		wantMIME, wantSuffix = "image/avif", ".avif"
	case recipe.SourceMode == profiledefinition.SourceVideo && recipe.FramePolicy == profiledefinition.FrameAll && recipe.VideoOutput != nil:
		wantMIME, wantSuffix = "video/mp4", ".mp4"
	case recipe.SourceMode == profiledefinition.SourceVideo && recipe.FramePolicy == profiledefinition.FrameFirst && recipe.StillOutput != nil:
		wantMIME, wantSuffix = "image/avif", ".avif"
	default:
		return ErrInvariant
	}
	if candidateMIME != wantMIME || !strings.HasSuffix(relativePath, wantSuffix) {
		return ErrConflict
	}
	return nil
}

func commitPublication(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		var pgError *pgconn.PgError
		if errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &pgError) {
			return &CommitRolledBack{Cause: err}
		}
		return &CommitOutcomeUnknown{Cause: err}
	}
	return nil
}

type eventMedia struct {
	ID                string           `json:"id"`
	MIMEType          string           `json:"mime_type"`
	OriginalFilename  *string          `json:"original_filename"`
	SizeBytes         int64            `json:"size_bytes"`
	Width             *int             `json:"width"`
	Height            *int             `json:"height"`
	DurationMS        *int64           `json:"duration_ms"`
	TakenAt           *time.Time       `json:"taken_at"`
	TakenAtSource     string           `json:"taken_at_source"`
	TakenAtTimezone   *string          `json:"taken_at_timezone"`
	CreatedAt         time.Time        `json:"created_at"`
	DeletedAt         *time.Time       `json:"deleted_at"`
	PurgeAfter        *time.Time       `json:"purge_after"`
	CurrentRenditions []eventRendition `json:"current_renditions"`
}

type eventRendition struct {
	ID         string       `json:"id"`
	MediaID    string       `json:"media_id"`
	TargetID   string       `json:"job_target_id"`
	Profile    eventProfile `json:"profile"`
	MIMEType   string       `json:"mime_type"`
	SizeBytes  int64        `json:"size_bytes"`
	Width      *int         `json:"width"`
	Height     *int         `json:"height"`
	DurationMS *int64       `json:"duration_ms"`
	SHA256     string       `json:"sha256"`
	FileURL    string       `json:"file_url"`
	CreatedAt  time.Time    `json:"created_at"`
}

type eventProfile struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}

func (r *Repository) mediaEventPayload(ctx context.Context, tx pgx.Tx, mediaID string) (json.RawMessage, error) {
	media := eventMedia{CurrentRenditions: make([]eventRendition, 0)}
	var originalID string
	if err := tx.QueryRow(ctx, `SELECT m.id::text,m.media_type,o.original_filename,o.size_bytes,o.width,o.height,o.duration_ms,
		m.taken_at,m.taken_at_source,m.taken_at_timezone,m.created_at,m.deleted_at,m.purge_after,o.id::text
		FROM media m JOIN originals o ON o.media_id=m.id WHERE m.id=$1`, mediaID).Scan(
		&media.ID, &media.MIMEType, &media.OriginalFilename, &media.SizeBytes, &media.Width, &media.Height,
		&media.DurationMS, &media.TakenAt, &media.TakenAtSource, &media.TakenAtTimezone, &media.CreatedAt,
		&media.DeletedAt, &media.PurgeAfter, &originalID); err != nil {
		return nil, classifyDatabaseError(err)
	}
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.media_id::text,r.job_target_id::text,p.id::text,p.key,p.version,
		r.mime_type,r.size_bytes,r.width,r.height,r.duration_ms,r.sha256,r.relative_path,r.created_at
		FROM renditions r JOIN job_targets jt ON jt.id=r.job_target_id JOIN jobs j ON j.id=jt.job_id
		JOIN profiles p ON p.id=jt.profile_id
		WHERE r.media_id=$1 AND r.is_current ORDER BY r.profile_key`, mediaID)
	if err != nil {
		return nil, classifyDatabaseError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rendition eventRendition
		var relativePath string
		if err := rows.Scan(&rendition.ID, &rendition.MediaID, &rendition.TargetID, &rendition.Profile.ID,
			&rendition.Profile.Key, &rendition.Profile.Version, &rendition.MIMEType, &rendition.SizeBytes,
			&rendition.Width, &rendition.Height, &rendition.DurationMS, &rendition.SHA256, &relativePath,
			&rendition.CreatedAt); err != nil {
			return nil, classifyDatabaseError(err)
		}
		key, err := storage.ParseRenditionKey(relativePath)
		if err != nil || key.OriginalID().String() != originalID || key.JobTargetID().String() != rendition.TargetID || key.RenditionID().String() != rendition.ID {
			return nil, ErrInvariant
		}
		rendition.FileURL = r.fileBaseURL + relativePath
		rendition.CreatedAt = rendition.CreatedAt.UTC()
		media.CurrentRenditions = append(media.CurrentRenditions, rendition)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDatabaseError(err)
	}
	media.CreatedAt = media.CreatedAt.UTC()
	utcPointer(&media.TakenAt)
	utcPointer(&media.DeletedAt)
	utcPointer(&media.PurgeAfter)
	payload, err := json.Marshal(media)
	if err != nil {
		return nil, ErrInvariant
	}
	return payload, nil
}

func validRendition(candidate Rendition) bool {
	originalID, originalErr := storage.ParseOriginalID(candidate.OriginalID)
	targetID, targetErr := storage.ParseJobTargetID(candidate.TargetID)
	renditionID, renditionErr := storage.ParseRenditionID(candidate.ID)
	key, keyErr := storage.ParseRenditionKey(candidate.RelativePath)
	if originalErr != nil || targetErr != nil || renditionErr != nil || keyErr != nil ||
		key.OriginalID() != originalID || key.JobTargetID() != targetID || key.RenditionID() != renditionID {
		return false
	}
	return validUUIDv4(candidate.JobID) && validUUIDv4(candidate.LeaseToken) && validUUIDv4(candidate.MediaID) &&
		validUUIDv4(candidate.ProfileID) && mimePattern.MatchString(candidate.MIMEType) && candidate.SizeBytes >= 0 &&
		validSHA256(candidate.SHA256) && (candidate.Width == nil) == (candidate.Height == nil) &&
		(candidate.Width == nil || *candidate.Width > 0 && *candidate.Height > 0) &&
		(candidate.DurationMS == nil || *candidate.DurationMS >= 0) && len(candidate.ProcessorAudit) <= 1<<20 &&
		validProcessorAudit(candidate.ProcessorAudit) && renditionMIMEMatchesPath(candidate.MIMEType, candidate.RelativePath)
}

func validProcessorAudit(value json.RawMessage) bool {
	var audit struct {
		SchemaVersion int             `json:"schema_version"`
		Family        string          `json:"family"`
		Result        json.RawMessage `json:"result"`
	}
	var result map[string]json.RawMessage
	if !jsonObject(value) || json.Unmarshal(value, &audit) != nil || audit.SchemaVersion != 1 ||
		json.Unmarshal(audit.Result, &result) != nil || len(result) == 0 {
		return false
	}
	return audit.Family == "still" || audit.Family == "animation" || audit.Family == "video"
}

func renditionMIMEMatchesPath(mimeType, relativePath string) bool {
	switch {
	case strings.HasSuffix(relativePath, ".avif"):
		return mimeType == "image/avif"
	case strings.HasSuffix(relativePath, ".webp"):
		return mimeType == "image/webp"
	case strings.HasSuffix(relativePath, ".mp4"):
		return mimeType == "video/mp4"
	default:
		return false
	}
}

func validUUIDv4(value string) bool {
	_, err := storage.ParseRenditionID(value)
	return err == nil
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

func validFileBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || value == "" || strings.TrimSpace(value) != value || parsed.Scheme != "https" ||
		parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || strings.Contains(value, "#") || !strings.HasSuffix(parsed.Path, "/") ||
		parsed.Path == "/" || strings.Contains(parsed.Path, "//") || strings.Contains(parsed.Path, "\\") ||
		parsed.EscapedPath() != (&url.URL{Path: parsed.Path}).EscapedPath() {
		return false
	}
	for _, character := range parsed.Path {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func utcPointer(value **time.Time) {
	if *value != nil {
		utc := (*value).UTC()
		*value = &utc
	}
}
