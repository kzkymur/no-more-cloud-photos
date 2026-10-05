package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewRepositoryOptions(t *testing.T) {
	if _, err := NewRepository(nil, Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil pool error = %v", err)
	}
	pool := &pgxpool.Pool{}
	for _, options := range []Options{
		{LeaseDuration: -time.Second},
		{LeaseDuration: time.Nanosecond},
		{ReclaimBatch: -1},
		{ReclaimBatch: 51},
	} {
		if _, err := NewRepository(pool, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid options %+v error = %v", options, err)
		}
	}
	repository, err := NewRepository(pool, Options{})
	if err != nil || repository.leaseDuration != 2*time.Minute || repository.reclaimBatch != 50 || repository.jitter == nil || repository.uuid == nil {
		t.Fatalf("default repository = %+v, %v", repository, err)
	}
}

func TestBackoffMaximumIsBoundedWithoutOverflow(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: 5 * time.Second},
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 8, want: 10*time.Minute + 40*time.Second},
		{attempt: 9, want: 15 * time.Minute},
		{attempt: 1_000_000, want: 15 * time.Minute},
	}
	for _, test := range tests {
		if got := backoffMaximum(test.attempt); got != test.want {
			t.Errorf("backoffMaximum(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestRetryDelayValidatesInjectedFullJitter(t *testing.T) {
	repository := &Repository{jitter: func(maximum time.Duration) time.Duration { return maximum }}
	if got, err := repository.retryDelay(3); err != nil || got != 20*time.Second {
		t.Fatalf("retryDelay valid boundary = %s, %v", got, err)
	}
	repository.jitter = func(maximum time.Duration) time.Duration { return maximum + 1 }
	if _, err := repository.retryDelay(3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retryDelay excessive jitter error = %v", err)
	}
	repository.jitter = func(time.Duration) time.Duration { return -1 }
	if _, err := repository.retryDelay(3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retryDelay negative jitter error = %v", err)
	}
}

func TestFailureCodesHaveOnlyFixedSafeMessages(t *testing.T) {
	want := map[FailureCode]string{
		FailureProcessTimeout:     "processing timed out",
		FailureProcessFailed:      "processing failed",
		FailureProcessOutputLimit: "processing output limit exceeded",
		FailureLeaseExpired:       "job lease expired",
		FailureWorkerShutdown:     "worker shut down before completion",
	}
	if len(safeFailureMessages) != len(want) {
		t.Fatalf("safe message count = %d, want %d", len(safeFailureMessages), len(want))
	}
	for code, message := range want {
		if safeFailureMessages[code] != message {
			t.Errorf("message for %q = %q", code, safeFailureMessages[code])
		}
	}
	if _, ok := workerFailure(FailureLeaseExpired); ok {
		t.Fatal("worker may persist lease_expired")
	}
	if _, ok := workerFailure(FailureCode("/private/path: stderr secret")); ok {
		t.Fatal("arbitrary failure code accepted")
	}
}

func TestValidateTypes(t *testing.T) {
	got, err := validateTypes([]Type{TypeTransform, TypeTransform, TypePurge})
	if err != nil || len(got) != 2 || got[0] != "transform" || got[1] != "purge" {
		t.Fatalf("validateTypes = %#v, %v", got, err)
	}
	if _, err := validateTypes([]Type{"unsupported"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported type error = %v", err)
	}
}

func TestJSONAuditArgumentsMustBeObject(t *testing.T) {
	for _, value := range []string{"", "null", "[]", `"value"`, "{bad"} {
		if jsonObject([]byte(value)) {
			t.Errorf("jsonObject(%q) = true", value)
		}
	}
	if !jsonObject([]byte(`{"job_id":"redacted"}`)) {
		t.Fatal("valid object rejected")
	}
}

func TestDatabaseErrorClassification(t *testing.T) {
	for _, code := range []string{"08006", "40001", "53000", "55P03", "57014", "57P01", "57P05"} {
		err := classifyDatabaseError(&pgconn.PgError{Code: code})
		if !errors.Is(err, ErrDatabaseUnavailable) {
			t.Errorf("SQLSTATE %s classified as %v", code, err)
		}
	}
	if err := classifyDatabaseError(&pgconn.PgError{Code: "23514"}); !errors.Is(err, ErrInvariant) {
		t.Fatalf("constraint error classified as %v", err)
	}
	if err := classifyDatabaseError(errors.New("scan includes /secret/path")); !errors.Is(err, ErrInvariant) {
		t.Fatalf("scan error classified as %v", err)
	}
	if err := classifyDatabaseError(context.DeadlineExceeded); !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("deadline classified as %v", err)
	}
}

func TestNewUUIDv4(t *testing.T) {
	for range 20 {
		id, err := newUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || id[14] != '4' || !containsByte("89ab", id[19]) {
			t.Fatalf("not UUIDv4: %q", id)
		}
	}
}

func containsByte(value string, target byte) bool {
	for index := range len(value) {
		if value[index] == target {
			return true
		}
	}
	return false
}
