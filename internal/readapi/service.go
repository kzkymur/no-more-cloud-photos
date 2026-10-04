package readapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const (
	mediaCursorPredicate = `(
		$4::boolean = false OR
		($5::timestamptz IS NULL AND m.taken_at IS NULL AND m.id < $6::uuid) OR
		($5::timestamptz IS NOT NULL AND (m.taken_at < $5 OR (m.taken_at = $5 AND m.id < $6::uuid) OR m.taken_at IS NULL))
	)`
	jobsCursorPredicate = `($4::boolean = false OR (j.created_at, j.id) < ($5::timestamptz, $6::uuid))`
)

type Service struct {
	pool        *pgxpool.Pool
	cursors     *CursorCodec
	fileBaseURL string
}

// NewService validates immutable process configuration before exposing reads.
func NewService(pool *pgxpool.Pool, cursors *CursorCodec, fileBaseURL string) (*Service, error) {
	if pool == nil {
		return nil, errors.New("read API database pool is required")
	}
	if cursors == nil || len(cursors.key) < 32 {
		return nil, errors.New("read API cursor codec is required")
	}
	base, err := normalizeFileBaseURL(fileBaseURL)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, cursors: cursors, fileBaseURL: base}, nil
}

func normalizeFileBaseURL(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("file base URL must be an absolute HTTPS files root")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") {
		return "", errors.New("file base URL must be an absolute HTTPS files root without credentials, query, or fragment")
	}
	canonicalEscapedPath := (&url.URL{Path: parsed.Path}).EscapedPath()
	if parsed.Path == "" || parsed.Path == "/" || parsed.EscapedPath() != canonicalEscapedPath || strings.Contains(parsed.Path, "\\") || strings.HasSuffix(parsed.Path, "//") {
		return "", errors.New("file base URL must have an unambiguous non-root path")
	}
	for _, character := range parsed.Path {
		if character < 0x20 || character == 0x7f {
			return "", errors.New("file base URL must not contain control characters")
		}
	}
	trimmed := strings.TrimSuffix(parsed.Path, "/")
	if trimmed == "" || strings.Contains(trimmed, "//") {
		return "", errors.New("file base URL must have an unambiguous non-root path")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(trimmed, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("file base URL must not contain empty or dot path segments")
		}
	}
	parsed.Path = trimmed + "/"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (s *Service) originalURL(relativePath string) (string, error) {
	key, err := storage.ParseOriginalKey(relativePath)
	if err != nil {
		return "", NewInvariantError(errors.New("stored original key is invalid"))
	}
	return s.fileBaseURL + key.String(), nil
}

func (s *Service) renditionURL(relativePath string) (string, error) {
	key, err := storage.ParseRenditionKey(relativePath)
	if err != nil {
		return "", NewInvariantError(errors.New("stored rendition key is invalid"))
	}
	return s.fileBaseURL + key.String(), nil
}

func (s *Service) ListMedia(ctx context.Context, request MediaListRequest) (MediaPage, error) {
	if err := request.Validate(); err != nil {
		return MediaPage{}, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM profiles WHERE key=$1)`, request.Profile).Scan(&exists); err != nil {
		return MediaPage{}, classifyDatabaseError(err)
	}
	if !exists {
		return MediaPage{}, NewInvalidProfile(request.Profile)
	}

	filter := MediaCursorFilter{Profile: request.Profile, Deleted: request.Deleted}
	var position MediaCursorPosition
	hasCursor := request.Cursor != ""
	if hasCursor {
		var err error
		position, err = s.cursors.DecodeMedia(request.Cursor, filter)
		if err != nil {
			return MediaPage{}, NewInvalidCursor()
		}
	}
	rows, err := s.pool.Query(ctx, mediaListSQL, request.Profile, request.Deleted, request.Limit+1, hasCursor, position.TakenAt, nullableCursorID(hasCursor, position.ID))
	if err != nil {
		return MediaPage{}, classifyDatabaseError(err)
	}
	items, err := s.scanMediaRows(rows, request.Profile)
	if err != nil {
		return MediaPage{}, err
	}

	var next *string
	if len(items) > request.Limit {
		items = items[:request.Limit]
		last := items[len(items)-1]
		token, err := s.cursors.EncodeMedia(filter, MediaCursorPosition{TakenAt: last.TakenAt, ID: last.ID})
		if err != nil {
			return MediaPage{}, NewInvariantError(errors.New("could not encode media cursor"))
		}
		next = &token
	}
	return NewMediaPage(items, next), nil
}

func nullableCursorID(hasCursor bool, id string) any {
	if !hasCursor {
		return nil
	}
	return id
}

func (s *Service) scanMediaRows(rows pgx.Rows, expectedProfile string) ([]MediaSummary, error) {
	defer rows.Close()
	items := make([]MediaSummary, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		var item MediaSummary
		var rendition nullableRendition
		if err := rows.Scan(
			&item.ID, &item.MIMEType, &item.OriginalFilename, &item.SizeBytes, &item.Width, &item.Height, &item.DurationMS,
			&item.TakenAt, &item.TakenAtSource, &item.TakenAtTimezone, &item.CreatedAt, &item.DeletedAt, &item.PurgeAfter,
			&rendition.ID, &rendition.MediaID, &rendition.JobTargetID, &rendition.ProfileKey, &rendition.ProfileID,
			&rendition.JoinedProfileKey, &rendition.ProfileVersion, &rendition.MIMEType, &rendition.SizeBytes,
			&rendition.Width, &rendition.Height, &rendition.DurationMS, &rendition.SHA256, &rendition.RelativePath, &rendition.CreatedAt,
		); err != nil {
			return nil, NewInvariantError(errors.New("invalid media row"))
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, NewInvariantError(errors.New("media list produced duplicate media"))
		}
		seen[item.ID] = struct{}{}
		if !validMediaRow(item.ID, item.SizeBytes, item.Width, item.Height, item.TakenAt, item.TakenAtSource, item.TakenAtTimezone, item.DeletedAt, item.PurgeAfter) {
			return nil, NewInvariantError(errors.New("inconsistent media row"))
		}
		normalizeMediaSummary(&item)
		built, err := s.buildRendition(rendition, item.ID, expectedProfile)
		if err != nil {
			return nil, err
		}
		item.Rendition = built
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDatabaseError(err)
	}
	return items, nil
}

func (s *Service) GetMedia(ctx context.Context, mediaID string) (MediaDetail, error) {
	if !IsUUIDv4(mediaID) {
		return MediaDetail{}, NewInvalidID()
	}
	var result MediaDetail
	err := s.withReadTransaction(ctx, func(tx pgx.Tx) error {
		var originalID *string
		if err := tx.QueryRow(ctx, mediaDetailSQL, mediaID).Scan(
			&result.ID, &result.MIMEType, &result.OriginalFilename, &result.SizeBytes, &result.Width, &result.Height,
			&result.DurationMS, &result.TakenAt, &result.TakenAtSource, &result.TakenAtTimezone, &result.CreatedAt,
			&result.DeletedAt, &result.PurgeAfter, &originalID,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return NewMediaNotFound()
			}
			return classifyDatabaseError(err)
		}
		if originalID == nil {
			return NewInvariantError(errors.New("media has no original"))
		}
		if !validMediaRow(result.ID, result.SizeBytes, result.Width, result.Height, result.TakenAt, result.TakenAtSource, result.TakenAtTimezone, result.DeletedAt, result.PurgeAfter) || !IsUUIDv4(*originalID) {
			return NewInvariantError(errors.New("inconsistent media row"))
		}
		normalizeMediaDetail(&result)
		renditions, err := s.currentRenditions(ctx, tx, mediaID)
		if err != nil {
			return err
		}
		jobs, err := s.jobHeaders(ctx, tx, jobQuery{MediaID: mediaID, Limit: 100})
		if err != nil {
			return err
		}
		if err := s.attachTargets(ctx, tx, jobs); err != nil {
			return err
		}
		result.CurrentRenditions, result.Jobs = renditions, jobs
		result.InitializeArrays()
		return nil
	})
	return result, err
}

func (s *Service) GetOriginal(ctx context.Context, mediaID string) (Original, error) {
	if !IsUUIDv4(mediaID) {
		return Original{}, NewInvalidID()
	}
	var original Original
	var relativePath *string
	var id, storedMediaID, mimeType, sha *string
	var size *int64
	var created *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT o.id::text,o.media_id::text,o.mime_type,o.size_bytes,o.sha256,o.original_filename,o.relative_path,o.created_at
		FROM media m LEFT JOIN originals o ON o.media_id=m.id WHERE m.id=$1`, mediaID).Scan(
		&id, &storedMediaID, &mimeType, &size, &sha, &original.OriginalFilename, &relativePath, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Original{}, NewMediaNotFound()
	}
	if err != nil {
		return Original{}, classifyDatabaseError(err)
	}
	if id == nil {
		return Original{}, NewOriginalNotFound()
	}
	if storedMediaID == nil || mimeType == nil || size == nil || sha == nil || relativePath == nil || created == nil || !IsUUIDv4(*id) || !IsUUIDv4(*storedMediaID) || *storedMediaID != mediaID || *size < 0 || !validSHA256(*sha) {
		return Original{}, NewInvariantError(errors.New("inconsistent original row"))
	}
	fileURL, err := s.originalURL(*relativePath)
	if err != nil {
		return Original{}, err
	}
	original.ID, original.MediaID, original.MIMEType, original.SizeBytes, original.SHA256 = *id, *storedMediaID, *mimeType, *size, *sha
	original.FileURL, original.CreatedAt = fileURL, created.UTC()
	return original, nil
}

func (s *Service) GetCurrentRendition(ctx context.Context, mediaID, key string) (Rendition, error) {
	if !IsUUIDv4(mediaID) {
		return Rendition{}, NewInvalidID()
	}
	if !validProfileKey(key) {
		return Rendition{}, NewInvalidRequest(map[string]string{"profile": "invalid"})
	}
	rows, err := s.pool.Query(ctx, currentRenditionSQL, mediaID, key)
	if err != nil {
		return Rendition{}, classifyDatabaseError(err)
	}
	defer rows.Close()
	count := 0
	var result *Rendition
	for rows.Next() {
		count++
		if count > 1 {
			return Rendition{}, NewInvariantError(errors.New("multiple current renditions"))
		}
		var candidate nullableRendition
		if err := scanNullableRendition(rows, &candidate); err != nil {
			return Rendition{}, NewInvariantError(errors.New("invalid current rendition row"))
		}
		result, err = s.buildRendition(candidate, mediaID, key)
		if err != nil {
			return Rendition{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return Rendition{}, classifyDatabaseError(err)
	}
	if count == 0 {
		return Rendition{}, NewMediaNotFound()
	}
	if result == nil {
		return Rendition{}, NewRenditionNotReady()
	}
	return *result, nil
}

func (s *Service) GetRendition(ctx context.Context, renditionID string) (Rendition, error) {
	if !IsUUIDv4(renditionID) {
		return Rendition{}, NewInvalidID()
	}
	var row nullableRendition
	err := scanNullableRendition(s.pool.QueryRow(ctx, renditionByIDSQL, renditionID), &row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rendition{}, NewRenditionNotFound()
	}
	if err != nil {
		return Rendition{}, classifyDatabaseError(err)
	}
	result, err := s.buildRendition(row, "", "")
	if err != nil {
		return Rendition{}, err
	}
	if result == nil {
		return Rendition{}, NewInvariantError(errors.New("empty rendition row"))
	}
	return *result, nil
}

func (s *Service) currentRenditions(ctx context.Context, tx pgx.Tx, mediaID string) ([]Rendition, error) {
	rows, err := tx.Query(ctx, allCurrentRenditionsSQL, mediaID)
	if err != nil {
		return nil, classifyDatabaseError(err)
	}
	defer rows.Close()
	result := make([]Rendition, 0)
	seenKeys := make(map[string]struct{})
	for rows.Next() {
		var row nullableRendition
		if err := scanNullableRendition(rows, &row); err != nil {
			return nil, NewInvariantError(errors.New("invalid rendition row"))
		}
		built, err := s.buildRendition(row, mediaID, "")
		if err != nil {
			return nil, err
		}
		if built == nil {
			return nil, NewInvariantError(errors.New("empty current rendition row"))
		}
		if _, duplicate := seenKeys[built.Profile.Key]; duplicate {
			return nil, NewInvariantError(errors.New("duplicate current rendition key"))
		}
		seenKeys[built.Profile.Key] = struct{}{}
		result = append(result, *built)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDatabaseError(err)
	}
	return result, nil
}

func (s *Service) ListJobs(ctx context.Context, request JobListRequest) (JobPage, error) {
	if err := request.Validate(); err != nil {
		return JobPage{}, err
	}
	filter := jobsCursorFilter(request)
	var position JobsCursorPosition
	hasCursor := request.Cursor != ""
	if hasCursor {
		var err error
		position, err = s.cursors.DecodeJobs(request.Cursor, filter)
		if err != nil {
			return JobPage{}, NewInvalidCursor()
		}
	}
	var result JobPage
	err := s.withReadTransaction(ctx, func(tx pgx.Tx) error {
		jobs, err := s.jobHeaders(ctx, tx, jobQuery{
			Status: string(request.Status), MediaID: request.MediaID, Limit: request.Limit + 1,
			HasCursor: hasCursor, CreatedAt: position.CreatedAt, CursorID: position.ID,
		})
		if err != nil {
			return err
		}
		var next *string
		if len(jobs) > request.Limit {
			jobs = jobs[:request.Limit]
			last := jobs[len(jobs)-1]
			token, err := s.cursors.EncodeJobs(filter, JobsCursorPosition{CreatedAt: last.CreatedAt, ID: last.ID})
			if err != nil {
				return NewInvariantError(errors.New("could not encode jobs cursor"))
			}
			next = &token
		}
		if err := s.attachTargets(ctx, tx, jobs); err != nil {
			return err
		}
		result = NewJobPage(jobs, next)
		return nil
	})
	return result, err
}

func jobsCursorFilter(request JobListRequest) JobsCursorFilter {
	var status, mediaID *string
	if request.Status != "" {
		value := string(request.Status)
		status = &value
	}
	if request.MediaID != "" {
		value := request.MediaID
		mediaID = &value
	}
	return JobsCursorFilter{Status: status, MediaID: mediaID}
}

func (s *Service) GetJob(ctx context.Context, jobID string) (Job, error) {
	if !IsUUIDv4(jobID) {
		return Job{}, NewInvalidID()
	}
	var result Job
	err := s.withReadTransaction(ctx, func(tx pgx.Tx) error {
		jobs, err := s.jobHeaders(ctx, tx, jobQuery{ID: jobID, Limit: 1})
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return NewJobNotFound()
		}
		if err := s.attachTargets(ctx, tx, jobs); err != nil {
			return err
		}
		result = jobs[0]
		return nil
	})
	return result, err
}

type jobQuery struct {
	ID, Status, MediaID string
	Limit               int
	HasCursor           bool
	CreatedAt           time.Time
	CursorID            string
}

func (s *Service) jobHeaders(ctx context.Context, tx pgx.Tx, query jobQuery) ([]Job, error) {
	rows, err := tx.Query(ctx, jobHeadersSQL, query.ID, query.Status, query.MediaID, query.HasCursor, nullableTime(query.HasCursor, query.CreatedAt), nullableCursorID(query.HasCursor, query.CursorID), query.Limit)
	if err != nil {
		return nil, classifyDatabaseError(err)
	}
	defer rows.Close()
	jobs := make([]Job, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		job := NewJob()
		var errorCode, errorMessage *string
		if err := rows.Scan(&job.ID, &job.Type, &job.Status, &job.MediaID, &job.OriginalID, &job.Attempts, &job.MaxAttempts,
			&job.AvailableAt, &job.StartedAt, &job.FinishedAt, &errorCode, &errorMessage, &job.CancelledAt,
			&job.CancelReason, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, NewInvariantError(errors.New("invalid job row"))
		}
		if _, duplicate := seen[job.ID]; duplicate || !IsUUIDv4(job.ID) || !IsUUIDv4(job.MediaID) || (job.OriginalID != nil && !IsUUIDv4(*job.OriginalID)) || !job.Status.Valid() || (job.Type != JobTransform && job.Type != JobPurge) || (job.Type == JobPurge && job.OriginalID != nil) || job.Attempts < 0 || job.MaxAttempts < job.Attempts || job.MaxAttempts < 1 || (errorCode == nil) != (errorMessage == nil) {
			return nil, NewInvariantError(errors.New("inconsistent job row"))
		}
		seen[job.ID] = struct{}{}
		if errorCode != nil {
			job.Error = &ResourceError{Code: *errorCode, Message: *errorMessage}
		}
		normalizeJob(&job)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDatabaseError(err)
	}
	return jobs, nil
}

func nullableTime(ok bool, value time.Time) any {
	if !ok {
		return nil
	}
	return value
}

func (s *Service) attachTargets(ctx context.Context, tx pgx.Tx, jobs []Job) error {
	if len(jobs) == 0 {
		return nil
	}
	ids := make([]string, len(jobs))
	indexes := make(map[string]int, len(jobs))
	for index := range jobs {
		ids[index], indexes[jobs[index].ID] = jobs[index].ID, index
	}
	rows, err := tx.Query(ctx, jobTargetsSQL, ids)
	if err != nil {
		return classifyDatabaseError(err)
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	for rows.Next() {
		var target JobTarget
		var jobID string
		var profileID, profileKey *string
		var profileVersion *int
		var errorCode, errorMessage *string
		if err := rows.Scan(&target.ID, &jobID, &profileID, &profileKey, &profileVersion, &target.Status,
			&target.Attempts, &errorCode, &errorMessage, &target.RenditionID, &target.UpdatedAt); err != nil {
			return NewInvariantError(errors.New("invalid job target row"))
		}
		index, found := indexes[jobID]
		if !found || profileID == nil || profileKey == nil || profileVersion == nil || !IsUUIDv4(target.ID) || !IsUUIDv4(*profileID) || !validProfileKey(*profileKey) || *profileVersion < 1 || !validTargetStatus(target.Status) || target.Attempts < 0 || (target.RenditionID != nil && !IsUUIDv4(*target.RenditionID)) || (errorCode == nil) != (errorMessage == nil) || (target.Status != TargetFailed && errorCode != nil) {
			return NewInvariantError(errors.New("inconsistent job target row"))
		}
		if _, duplicate := seen[target.ID]; duplicate {
			return NewInvariantError(errors.New("duplicate job target row"))
		}
		seen[target.ID] = struct{}{}
		target.Profile = ProfileRef{ID: *profileID, Key: *profileKey, Version: *profileVersion}
		if errorCode != nil {
			target.Error = &ResourceError{Code: *errorCode, Message: *errorMessage}
		}
		target.UpdatedAt = target.UpdatedAt.UTC()
		jobs[index].Targets = append(jobs[index].Targets, target)
	}
	if err := rows.Err(); err != nil {
		return classifyDatabaseError(err)
	}
	for index := range jobs {
		if jobs[index].Type == JobPurge && len(jobs[index].Targets) != 0 || jobs[index].Type == JobTransform && len(jobs[index].Targets) == 0 {
			return NewInvariantError(errors.New("job target cardinality is inconsistent"))
		}
	}
	return nil
}

func (s *Service) ListProfiles(ctx context.Context, request ProfileListRequest) (ProfilePage, error) {
	if err := request.Validate(); err != nil {
		return ProfilePage{}, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text,key,version,status,input_mime_types,parameters_schema_version,processor,parameters::text,
		       created_at,activated_at,retired_at
		FROM profiles WHERE ($1::text='' OR status=$1) ORDER BY key ASC,version DESC`, request.Status)
	if err != nil {
		return ProfilePage{}, classifyDatabaseError(err)
	}
	defer rows.Close()
	profiles := make([]Profile, 0)
	for rows.Next() {
		profile := NewProfile()
		var parameters []byte
		if err := rows.Scan(&profile.ID, &profile.Key, &profile.Version, &profile.Status, &profile.InputMIMETypes,
			&profile.ParametersSchemaVersion, &profile.Processor, &parameters, &profile.CreatedAt, &profile.ActivatedAt, &profile.RetiredAt); err != nil {
			return ProfilePage{}, NewInvariantError(errors.New("invalid profile row"))
		}
		trimmed := bytes.TrimSpace(parameters)
		if !IsUUIDv4(profile.ID) || !validProfileKey(profile.Key) || profile.Version < 1 || !profile.Status.Valid() || len(profile.InputMIMETypes) == 0 || profile.ParametersSchemaVersion < 1 || profile.Processor == "" || len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return ProfilePage{}, NewInvariantError(errors.New("inconsistent profile row"))
		}
		profile.Parameters = append(json.RawMessage(nil), parameters...)
		profile.CreatedAt = profile.CreatedAt.UTC()
		utcTimePointer(&profile.ActivatedAt)
		utcTimePointer(&profile.RetiredAt)
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return ProfilePage{}, classifyDatabaseError(err)
	}
	return NewProfilePage(profiles), nil
}

func (s *Service) withReadTransaction(ctx context.Context, operation func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return classifyDatabaseError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := operation(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classifyDatabaseError(err)
	}
	return nil
}

type nullableRendition struct {
	ID, MediaID, JobTargetID, ProfileKey, ProfileID, JoinedProfileKey *string
	ProfileVersion                                                    *int
	MIMEType                                                          *string
	SizeBytes                                                         *int64
	Width, Height                                                     *int
	DurationMS                                                        *int64
	SHA256, RelativePath                                              *string
	CreatedAt                                                         *time.Time
}

type rowScanner interface{ Scan(...any) error }

func scanNullableRendition(scanner rowScanner, row *nullableRendition) error {
	return scanner.Scan(&row.ID, &row.MediaID, &row.JobTargetID, &row.ProfileKey, &row.ProfileID, &row.JoinedProfileKey,
		&row.ProfileVersion, &row.MIMEType, &row.SizeBytes, &row.Width, &row.Height, &row.DurationMS,
		&row.SHA256, &row.RelativePath, &row.CreatedAt)
}

func (s *Service) buildRendition(row nullableRendition, expectedMediaID, expectedProfileKey string) (*Rendition, error) {
	if row.ID == nil {
		if row.anyPresent() {
			return nil, NewInvariantError(errors.New("partially null rendition row"))
		}
		return nil, nil
	}
	if row.MediaID == nil || row.JobTargetID == nil || row.ProfileKey == nil || row.ProfileID == nil || row.JoinedProfileKey == nil || row.ProfileVersion == nil || row.MIMEType == nil || row.SizeBytes == nil || row.SHA256 == nil || row.RelativePath == nil || row.CreatedAt == nil {
		return nil, NewInvariantError(errors.New("partially null rendition row"))
	}
	if !IsUUIDv4(*row.ID) || !IsUUIDv4(*row.MediaID) || !IsUUIDv4(*row.JobTargetID) || !IsUUIDv4(*row.ProfileID) || !validProfileKey(*row.ProfileKey) || *row.ProfileVersion < 1 || *row.ProfileKey != *row.JoinedProfileKey || *row.SizeBytes < 0 || !validSHA256(*row.SHA256) || (row.Width == nil) != (row.Height == nil) || (row.DurationMS != nil && *row.DurationMS < 0) || (expectedMediaID != "" && *row.MediaID != expectedMediaID) || (expectedProfileKey != "" && *row.ProfileKey != expectedProfileKey) {
		return nil, NewInvariantError(errors.New("inconsistent rendition provenance"))
	}
	fileURL, err := s.renditionURL(*row.RelativePath)
	if err != nil {
		return nil, err
	}
	result := &Rendition{
		ID: *row.ID, MediaID: *row.MediaID, JobTargetID: *row.JobTargetID,
		Profile:  ProfileRef{ID: *row.ProfileID, Key: *row.ProfileKey, Version: *row.ProfileVersion},
		MIMEType: *row.MIMEType, SizeBytes: *row.SizeBytes, Width: row.Width, Height: row.Height,
		DurationMS: row.DurationMS, SHA256: *row.SHA256, FileURL: fileURL, CreatedAt: row.CreatedAt.UTC(),
	}
	return result, nil
}

func (row nullableRendition) anyPresent() bool {
	return row.MediaID != nil || row.JobTargetID != nil || row.ProfileKey != nil || row.ProfileID != nil || row.JoinedProfileKey != nil || row.ProfileVersion != nil || row.MIMEType != nil || row.SizeBytes != nil || row.Width != nil || row.Height != nil || row.DurationMS != nil || row.SHA256 != nil || row.RelativePath != nil || row.CreatedAt != nil
}

func normalizeMediaSummary(item *MediaSummary) {
	item.CreatedAt = item.CreatedAt.UTC()
	utcTimePointer(&item.TakenAt)
	utcTimePointer(&item.DeletedAt)
	utcTimePointer(&item.PurgeAfter)
}

func validMediaRow(id string, size int64, width, height *int, takenAt *time.Time, source TakenAtSource, timezone *string, deletedAt, purgeAfter *time.Time) bool {
	if !IsUUIDv4(id) || size < 0 || (width == nil) != (height == nil) || purgeAfter != nil && deletedAt == nil {
		return false
	}
	switch source {
	case TakenAtEmbeddedOffset:
		return takenAt != nil && timezone == nil
	case TakenAtDefaultTimezone:
		return takenAt != nil && timezone != nil
	case TakenAtUnknown:
		return takenAt == nil && timezone == nil
	default:
		return false
	}
}

func validTargetStatus(status JobTargetStatus) bool {
	return status == TargetPending || status == TargetSucceeded || status == TargetFailed
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

func normalizeMediaDetail(item *MediaDetail) {
	item.CreatedAt = item.CreatedAt.UTC()
	utcTimePointer(&item.TakenAt)
	utcTimePointer(&item.DeletedAt)
	utcTimePointer(&item.PurgeAfter)
}

func normalizeJob(job *Job) {
	job.AvailableAt, job.CreatedAt, job.UpdatedAt = job.AvailableAt.UTC(), job.CreatedAt.UTC(), job.UpdatedAt.UTC()
	utcTimePointer(&job.StartedAt)
	utcTimePointer(&job.FinishedAt)
	utcTimePointer(&job.CancelledAt)
}

func utcTimePointer(value **time.Time) {
	if *value != nil {
		utc := (*value).UTC()
		*value = &utc
	}
}

func classifyDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	var semantic *SemanticError
	if errors.As(err, &semantic) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.SafeToRetry(err) {
		return NewDatabaseUnavailableError(err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return NewDatabaseUnavailableError(err)
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		if strings.HasPrefix(pgError.Code, "08") || strings.HasPrefix(pgError.Code, "40") || pgError.Code == "55P03" || pgError.Code == "57014" {
			return NewDatabaseUnavailableError(err)
		}
	}
	return NewInvariantError(err)
}

const mediaProjection = `
	m.id::text,m.media_type,o.original_filename,o.size_bytes,o.width,o.height,o.duration_ms,
	m.taken_at,m.taken_at_source,m.taken_at_timezone,m.created_at,m.deleted_at,m.purge_after`

const nullableRenditionProjection = `
	r.id::text,r.media_id::text,r.job_target_id::text,r.profile_key,p.id::text,p.key,p.version,
	r.mime_type,r.size_bytes,r.width,r.height,r.duration_ms,r.sha256,r.relative_path,r.created_at`

const mediaListSQL = `
	SELECT ` + mediaProjection + `,` + nullableRenditionProjection + `
	FROM media m
	JOIN originals o ON o.media_id=m.id
	LEFT JOIN renditions r ON r.media_id=m.id AND r.profile_key=$1 AND r.is_current
	LEFT JOIN job_targets jt ON jt.id=r.job_target_id
	LEFT JOIN profiles p ON p.id=jt.profile_id
	WHERE ($2::text='include' OR ($2::text='exclude' AND m.deleted_at IS NULL) OR ($2::text='only' AND m.deleted_at IS NOT NULL))
	  AND ` + mediaCursorPredicate + `
	ORDER BY m.taken_at DESC NULLS LAST,m.id DESC
	LIMIT $3`

const mediaDetailSQL = `
	SELECT ` + mediaProjection + `,o.id::text
	FROM media m LEFT JOIN originals o ON o.media_id=m.id WHERE m.id=$1`

const currentRenditionSQL = `
	SELECT ` + nullableRenditionProjection + `
	FROM media m
	LEFT JOIN renditions r ON r.media_id=m.id AND r.profile_key=$2 AND r.is_current
	LEFT JOIN job_targets jt ON jt.id=r.job_target_id
	LEFT JOIN profiles p ON p.id=jt.profile_id
	WHERE m.id=$1`

const renditionByIDSQL = `
	SELECT ` + nullableRenditionProjection + `
	FROM renditions r LEFT JOIN job_targets jt ON jt.id=r.job_target_id LEFT JOIN profiles p ON p.id=jt.profile_id
	WHERE r.id=$1`

const allCurrentRenditionsSQL = `
	SELECT ` + nullableRenditionProjection + `
	FROM renditions r LEFT JOIN job_targets jt ON jt.id=r.job_target_id LEFT JOIN profiles p ON p.id=jt.profile_id
	WHERE r.media_id=$1 AND r.is_current ORDER BY r.profile_key ASC`

const jobHeadersSQL = `
	SELECT j.id::text,j.type,j.status,j.media_id_snapshot::text,j.original_id::text,j.attempts,j.max_attempts,
	       j.available_at,j.started_at,j.finished_at,j.error_code,j.error_message,j.cancelled_at,j.cancel_reason,j.created_at,j.updated_at
	FROM jobs j
	WHERE ($1::text='' OR j.id::text=$1) AND ($2::text='' OR j.status=$2) AND ($3::text='' OR j.media_id_snapshot::text=$3)
	  AND ` + jobsCursorPredicate + `
	ORDER BY j.created_at DESC,j.id DESC LIMIT $7`

const jobTargetsSQL = `
	SELECT jt.id::text,jt.job_id::text,p.id::text,p.key,p.version,jt.status,jt.attempts,jt.error_code,jt.error_message,r.id::text,jt.updated_at
	FROM job_targets jt LEFT JOIN profiles p ON p.id=jt.profile_id LEFT JOIN renditions r ON r.job_target_id=jt.id
	WHERE jt.job_id=ANY($1::uuid[])
	ORDER BY jt.job_id,p.key ASC,p.version DESC,jt.id ASC`
