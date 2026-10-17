package medialifecycle

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/readapi"
)

const defaultDatabaseBudget = 30 * time.Second
const purgeFileStepBudget = 20 * time.Second

type lifecycleRepository interface {
	Delete(context.Context, string) (DeleteResult, error)
	Restore(context.Context, string) (RestoreResult, error)
	EnqueuePurge(context.Context, string) (EnqueueResult, error)
	EnqueueDuePurge(context.Context, string) (EnqueueResult, error)
	ScanDuePurges(context.Context, time.Time, *DuePurgeCursor, int) ([]DuePurgeCandidate, error)
	DatabaseNow(context.Context) (time.Time, error)
	DiscoverPurgeJobs(context.Context, int) ([]string, error)
	ReclaimExpiredPurges(context.Context, int) ([]string, error)
	StartPurge(context.Context, string) (PurgeLease, error)
	RunPurgeFileStep(context.Context, string, string, PurgeFileAction) (PurgeStepResult, error)
	FinalizePurge(context.Context, string, string) error
	HeartbeatPurge(context.Context, string, string) (time.Time, error)
	FinishPurgeAttempt(context.Context, string, string, string) error
}

type cleanupRepository interface {
	CleanupNextRendition(context.Context, string, []string, func(context.Context, string, string, int64) (bool, error)) (string, error)
}

type Service struct {
	repository lifecycleRepository
	cleanup    cleanupRepository
	dbBudget   time.Duration
}

func NewService(pool *pgxpool.Pool, fileBaseURL string) (*Service, error) {
	repository, err := NewPostgresRepository(pool, fileBaseURL)
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository, cleanup: repository, dbBudget: defaultDatabaseBudget}, nil
}

func newService(repository lifecycleRepository) *Service {
	service := &Service{repository: repository, dbBudget: defaultDatabaseBudget}
	service.cleanup, _ = repository.(cleanupRepository)
	return service
}

// CleanupNextRendition executes one exact cleanup operation. Filesystem work is
// synchronous inside the database transaction so its locks cover unlink and
// parent-directory durability.
func (s *Service) CleanupNextRendition(ctx context.Context, preferredID string, excludedIDs []string, unlink func(context.Context, string, string, int64) (bool, error)) (string, error) {
	if s.cleanup == nil || unlink == nil {
		return "", newInvariant(errors.New("rendition cleanup is not configured"))
	}
	if preferredID != "" && !readapi.IsUUIDv4(preferredID) {
		return "", newInvalidID()
	}
	for _, id := range excludedIDs {
		if !readapi.IsUUIDv4(id) {
			return "", newInvalidID()
		}
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	id, err := s.cleanup.CleanupNextRendition(dbCtx, preferredID, excludedIDs, unlink)
	return id, classifyError(err)
}

func (s *Service) Delete(ctx context.Context, mediaID string) (DeleteResult, error) {
	if !readapi.IsUUIDv4(mediaID) {
		return DeleteResult{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.Delete(dbCtx, mediaID)
	return result, classifyError(err)
}

func (s *Service) Restore(ctx context.Context, mediaID string) (RestoreResult, error) {
	if !readapi.IsUUIDv4(mediaID) {
		return RestoreResult{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.Restore(dbCtx, mediaID)
	return result, classifyError(err)
}

func (s *Service) EnqueuePurge(ctx context.Context, mediaID string) (EnqueueResult, error) {
	if !readapi.IsUUIDv4(mediaID) {
		return EnqueueResult{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.EnqueuePurge(dbCtx, mediaID)
	return result, classifyError(err)
}

func (s *Service) EnqueueDuePurge(ctx context.Context, mediaID string) (EnqueueResult, error) {
	if !readapi.IsUUIDv4(mediaID) {
		return EnqueueResult{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.EnqueueDuePurge(dbCtx, mediaID)
	return result, classifyError(err)
}

func (s *Service) ScanDuePurges(ctx context.Context, dueThrough time.Time, after *DuePurgeCursor, limit int) ([]DuePurgeCandidate, error) {
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.ScanDuePurges(dbCtx, dueThrough, after, limit)
	return result, classifyError(err)
}

func (s *Service) DatabaseNow(ctx context.Context) (time.Time, error) {
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.DatabaseNow(dbCtx)
	return result, classifyError(err)
}

func (s *Service) StartNextPurge(ctx context.Context, limit int) (PurgeLease, error) {
	if limit <= 0 || limit > purgeSchedulerBatchMax {
		return PurgeLease{}, newInvariant(errors.New("invalid purge scheduler limit"))
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	if _, err := s.repository.ReclaimExpiredPurges(dbCtx, limit); err != nil {
		return PurgeLease{}, classifyError(err)
	}
	jobIDs, err := s.repository.DiscoverPurgeJobs(dbCtx, limit)
	if err != nil {
		return PurgeLease{}, classifyError(err)
	}
	for _, jobID := range jobIDs {
		lease, err := s.repository.StartPurge(dbCtx, jobID)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrNoPurgeWork) {
			return PurgeLease{}, classifyError(err)
		}
	}
	return PurgeLease{}, ErrNoPurgeWork
}

func (s *Service) StartPurge(ctx context.Context, jobID string) (PurgeLease, error) {
	if !readapi.IsUUIDv4(jobID) {
		return PurgeLease{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	lease, err := s.repository.StartPurge(dbCtx, jobID)
	return lease, classifyError(err)
}

func (s *Service) RunPurgeFileStep(ctx context.Context, jobID, token string, action PurgeFileAction) (PurgeStepResult, error) {
	if !readapi.IsUUIDv4(jobID) || !readapi.IsUUIDv4(token) || action == nil {
		return PurgeStepResult{}, newInvalidID()
	}
	stepCtx, cancel := context.WithTimeout(ctx, purgeFileStepBudget)
	defer cancel()
	result, err := s.repository.RunPurgeFileStep(stepCtx, jobID, token, action)
	return result, classifyError(err)
}

func (s *Service) FinalizePurge(ctx context.Context, jobID, token string) error {
	if !readapi.IsUUIDv4(jobID) || !readapi.IsUUIDv4(token) {
		return newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	return classifyError(s.repository.FinalizePurge(dbCtx, jobID, token))
}

func (s *Service) HeartbeatPurge(ctx context.Context, jobID, token string) (time.Time, error) {
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	result, err := s.repository.HeartbeatPurge(dbCtx, jobID, token)
	return result, classifyError(err)
}

func (s *Service) FinishPurgeAttempt(ctx context.Context, jobID, token, code string) error {
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	return classifyError(s.repository.FinishPurgeAttempt(dbCtx, jobID, token, code))
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var semantic *SemanticError
	var unknown *CommitOutcomeUnknown
	if errors.Is(err, ErrNoPurgeWork) || errors.Is(err, ErrPurgeLeaseLost) || errors.As(err, &semantic) || errors.As(err, &unknown) {
		return err
	}
	var rolledBack *CommitRolledBack
	if errors.As(err, &rolledBack) {
		return newUnavailable(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.SafeToRetry(err) {
		return newUnavailable(err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return newUnavailable(err)
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		code := postgresError.Code
		if strings.HasPrefix(code, "08") || strings.HasPrefix(code, "40") || strings.HasPrefix(code, "53") || code == "55P03" || code == "57014" || strings.HasPrefix(code, "57P0") {
			return newUnavailable(err)
		}
	}
	return newInvariant(err)
}
