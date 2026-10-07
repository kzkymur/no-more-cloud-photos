package medialifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const unitMediaID = "10000000-0000-4000-8000-000000000001"

type stubRepository struct {
	deleteResult  DeleteResult
	restoreResult RestoreResult
	enqueueResult EnqueueResult
	startResult   PurgeLease
	err           error
	calls         int
}

func (r *stubRepository) Delete(context.Context, string) (DeleteResult, error) {
	r.calls++
	return r.deleteResult, r.err
}

func (r *stubRepository) Restore(context.Context, string) (RestoreResult, error) {
	r.calls++
	return r.restoreResult, r.err
}

func (r *stubRepository) EnqueuePurge(context.Context, string) (EnqueueResult, error) {
	r.calls++
	return r.enqueueResult, r.err
}

func (r *stubRepository) EnqueueDuePurge(context.Context, string) (EnqueueResult, error) {
	r.calls++
	return r.enqueueResult, r.err
}

func (r *stubRepository) ScanDuePurges(context.Context, time.Time, *DuePurgeCursor, int) ([]DuePurgeCandidate, error) {
	r.calls++
	return nil, r.err
}

func (r *stubRepository) DatabaseNow(context.Context) (time.Time, error) {
	r.calls++
	return time.Time{}, r.err
}

func (r *stubRepository) StartPurge(context.Context, string) (PurgeLease, error) {
	r.calls++
	return r.startResult, r.err
}

func TestServiceRejectsInvalidIDBeforeRepository(t *testing.T) {
	repository := &stubRepository{}
	service := newService(repository)
	if _, err := service.Delete(context.Background(), "not-a-uuid"); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("Delete error = %#v", err)
	}
	if _, err := service.Restore(context.Background(), "not-a-uuid"); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("Restore error = %#v", err)
	}
	if _, err := service.EnqueuePurge(context.Background(), "not-a-uuid"); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("EnqueuePurge error = %#v", err)
	}
	if _, err := service.EnqueueDuePurge(context.Background(), "not-a-uuid"); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("EnqueueDuePurge error = %#v", err)
	}
	if _, err := service.StartPurge(context.Background(), "not-a-uuid"); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("StartPurge error = %#v", err)
	}
	if repository.calls != 0 {
		t.Fatalf("repository calls = %d, want 0", repository.calls)
	}
}

func TestServicePreservesNoPurgeWork(t *testing.T) {
	repository := &stubRepository{err: ErrNoPurgeWork}
	if _, err := newService(repository).StartPurge(context.Background(), unitMediaID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("StartPurge error = %#v", err)
	}
	if _, err := newService(repository).EnqueueDuePurge(context.Background(), unitMediaID); !errors.Is(err, ErrNoPurgeWork) {
		t.Fatalf("EnqueueDuePurge error = %#v", err)
	}
}

func TestServicePreservesResultsAndSemanticErrors(t *testing.T) {
	want := EnqueueResult{Disposition: EnqueueExistingRunning}
	repository := &stubRepository{enqueueResult: want}
	service := newService(repository)
	got, err := service.EnqueuePurge(context.Background(), unitMediaID)
	if err != nil || got.Disposition != want.Disposition {
		t.Fatalf("EnqueuePurge = %#v, %v", got, err)
	}

	wantErr := newPurgeFailed(unitMediaID)
	repository.err = wantErr
	if _, err := service.EnqueuePurge(context.Background(), unitMediaID); !errors.Is(err, wantErr) {
		t.Fatalf("semantic error = %#v", err)
	}
}

func TestDatabaseErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind ErrorKind
	}{
		{name: "serialization", err: &pgconn.PgError{Code: "40001"}, kind: KindDatabaseUnavailable},
		{name: "connection", err: &pgconn.PgError{Code: "08006"}, kind: KindDatabaseUnavailable},
		{name: "constraint", err: &pgconn.PgError{Code: "23514"}, kind: KindInvariant},
		{name: "ordinary", err: errors.New("broken row"), kind: KindInvariant},
		{name: "known rollback", err: &CommitRolledBack{Cause: errors.New("rollback")}, kind: KindDatabaseUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyError(test.err); !IsKind(got, test.kind) {
				t.Fatalf("classifyError(%v) = %#v", test.err, got)
			}
		})
	}
	unknown := &CommitOutcomeUnknown{Cause: errors.New("lost response")}
	if got := classifyError(unknown); !errors.Is(got, unknown) {
		t.Fatalf("commit unknown was replaced: %#v", got)
	}
}

func TestSemanticDetailsAreDefensiveCopies(t *testing.T) {
	err := newPurgeAlreadyStarted(unitMediaID)
	details := err.Details()
	details["job_id"] = "changed"
	if got := err.Details()["job_id"]; got != unitMediaID {
		t.Fatalf("stored job_id = %v", got)
	}
}

func TestPostgresRepositoryConstructorValidation(t *testing.T) {
	if _, err := NewPostgresRepository(nil, "https://files.example/files"); err == nil {
		t.Fatal("nil pool was accepted")
	}
	invalidURLs := []string{"", "http://files.example/files", "https://files.example/", "https://files.example/files?token=x", "https://files.example/files/%2e%2e"}
	for _, value := range invalidURLs {
		if _, err := normalizeFileBaseURL(value); err == nil {
			t.Fatalf("invalid file base URL %q was accepted", value)
		}
	}
	got, err := normalizeFileBaseURL("https://files.example/root/files/")
	if err != nil || got != "https://files.example/root/files/" {
		t.Fatalf("normalized URL = %q, %v", got, err)
	}
}
