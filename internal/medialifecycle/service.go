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

type lifecycleRepository interface {
	Delete(context.Context, string) (DeleteResult, error)
	Restore(context.Context, string) (RestoreResult, error)
	EnqueuePurge(context.Context, string) (EnqueueResult, error)
	StartPurge(context.Context, string) (PurgeLease, error)
}

type Service struct {
	repository lifecycleRepository
	dbBudget   time.Duration
}

func NewService(pool *pgxpool.Pool, fileBaseURL string) (*Service, error) {
	repository, err := NewPostgresRepository(pool, fileBaseURL)
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository, dbBudget: defaultDatabaseBudget}, nil
}

func newService(repository lifecycleRepository) *Service {
	return &Service{repository: repository, dbBudget: defaultDatabaseBudget}
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

func (s *Service) StartPurge(ctx context.Context, jobID string) (PurgeLease, error) {
	if !readapi.IsUUIDv4(jobID) {
		return PurgeLease{}, newInvalidID()
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.dbBudget)
	defer cancel()
	lease, err := s.repository.StartPurge(dbCtx, jobID)
	return lease, classifyError(err)
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var semantic *SemanticError
	var unknown *CommitOutcomeUnknown
	if errors.Is(err, ErrNoPurgeWork) || errors.As(err, &semantic) || errors.As(err, &unknown) {
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
