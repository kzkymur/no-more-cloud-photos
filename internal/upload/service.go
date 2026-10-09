//go:build linux

package upload

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/metadata"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

const (
	MaxOriginalBytes        = int64(20 * 1024 * 1024 * 1024)
	defaultDBBudget         = 30 * time.Second
	abortBudget             = 5 * time.Second
	maxTimezoneTries        = 3
	uploadHeartbeatInterval = 30 * time.Second
)

// Request is the validated upload part passed by the HTTP layer. Filename must
// already be normalized with NormalizeFilename.
type Request struct {
	Body           io.Reader
	Filename       *string
	IdempotencyKey string
	RequestID      string
}

type stagedOriginal interface {
	io.Writer
	Seal(context.Context, storage.Validation) error
	UseReadOnlyFile(func(*os.File) error) error
	PublishSealed(context.Context, storage.OriginalExtension) (string, error)
	Abort(context.Context) error
}

type originalStore interface {
	Begin(context.Context, string, string) (stagedOriginal, error)
}

type metadataProber interface {
	Probe(context.Context, *os.File, string) (metadata.Result, error)
}

// Service streams and probes uploads outside a transaction and delegates the
// short, ordered acceptance transaction to its repository.
type Service struct {
	store             originalStore
	prober            metadataProber
	repository        acceptanceRepository
	newID             func() (string, error)
	maxBytes          int64
	dbBudget          time.Duration
	heartbeatInterval time.Duration
	timezoneTries     int
}

type uploadHeartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (heartbeat *uploadHeartbeat) Stop() {
	heartbeat.once.Do(func() { close(heartbeat.stop) })
	<-heartbeat.done
}

func (heartbeat *uploadHeartbeat) Err() error {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	return heartbeat.err
}

func (s *Service) startAttemptHeartbeat(ctx context.Context, cancel context.CancelCauseFunc, attemptID string) *uploadHeartbeat {
	heartbeat := &uploadHeartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(context.WithoutCancel(ctx), abortBudget)
				err := s.repository.HeartbeatAttempt(heartbeatCtx, attemptID)
				heartbeatCancel()
				if err != nil {
					heartbeat.mu.Lock()
					heartbeat.err = err
					heartbeat.mu.Unlock()
					cancel(err)
					return
				}
			}
		}
	}()
	return heartbeat
}

// NewService constructs the production upload service.
func NewService(db *pgxpool.Pool, store *storage.Store, prober *metadata.Prober) (*Service, error) {
	if db == nil || store == nil || prober == nil {
		return nil, errors.New("upload service dependencies must not be nil")
	}
	return newService(storageAdapter{store}, prober, newPGRepository(db, store.DatabaseCheckpoint)), nil
}

func newService(store originalStore, prober metadataProber, repository acceptanceRepository) *Service {
	return &Service{
		store: store, prober: prober, repository: repository, newID: NewUUIDv4,
		maxBytes: MaxOriginalBytes, dbBudget: defaultDBBudget,
		heartbeatInterval: uploadHeartbeatInterval, timezoneTries: maxTimezoneTries,
	}
}

func (s *Service) Accept(ctx context.Context, request Request) (Outcome, error) {
	requestCtx := ctx
	if err := validateRequest(request); err != nil {
		return Outcome{}, err
	}
	originalID, err := s.newID()
	if err != nil {
		return Outcome{}, internalFailure(err)
	}
	attemptID, err := s.newID()
	if err != nil {
		return Outcome{}, internalFailure(err)
	}
	mediaID, err := s.newID()
	if err != nil {
		return Outcome{}, internalFailure(err)
	}
	published := false
	registerCtx, cancelRegister := context.WithTimeout(ctx, s.dbBudget)
	err = s.repository.RegisterAttempt(registerCtx, attemptID, originalID)
	cancelRegister()
	if err != nil {
		return Outcome{}, s.databaseFailure(ctx, registerCtx, err)
	}
	operationCtx, cancelOperation := context.WithCancelCause(ctx)
	heartbeat := s.startAttemptHeartbeat(operationCtx, cancelOperation, attemptID)
	ctx = operationCtx
	attemptFinished := false
	defer func() {
		if attemptFinished {
			return
		}
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortBudget)
		defer cancel()
		terminal := "aborted"
		if published {
			terminal = "published"
		}
		_ = s.repository.CompleteAttempt(finishCtx, attemptID, terminal)
	}()

	temporary, err := s.store.Begin(ctx, originalID, attemptID)
	if err != nil {
		cancelOperation(nil)
		heartbeat.Stop()
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		if ctx.Err() != nil {
			return Outcome{}, timeoutFailure(err)
		}
		return Outcome{}, dependencyFailure(err)
	}
	defer func() {
		if !published {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortBudget)
			defer cancel()
			if temporary.Abort(abortCtx) == nil {
				_ = s.repository.CompleteAttempt(abortCtx, attemptID, "aborted")
				attemptFinished = true
			}
		}
	}()
	defer func() {
		cancelOperation(nil)
		heartbeat.Stop()
	}()

	digest := sha256.New()
	destination := &streamDestination{temporary: temporary, digest: digest}
	written, copyErr := io.Copy(destination, io.LimitReader(request.Body, s.maxBytes+1))
	if copyErr != nil {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		if ctx.Err() != nil || errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
			return Outcome{}, timeoutFailure(copyErr)
		}
		if destination.writeErr != nil {
			return Outcome{}, dependencyFailure(copyErr)
		}
		return Outcome{}, &Failure{Status: 400, Code: "invalid_multipart", Message: "upload body could not be read", Cause: copyErr}
	}
	if written == 0 {
		return Outcome{}, &Failure{Status: 400, Code: "invalid_multipart", Message: "uploaded file is empty"}
	}
	if written > s.maxBytes {
		return Outcome{}, &Failure{Status: 413, Code: "upload_too_large", Message: "uploaded file exceeds the size limit"}
	}
	var contentSHA [sha256.Size]byte
	copy(contentSHA[:], digest.Sum(nil))
	if err := temporary.Seal(ctx, storage.Validation{ExpectedSize: written, ExpectedSHA256: &contentSHA}); err != nil {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		return Outcome{}, classifySealError(ctx, err)
	}

	timezoneCtx, cancelTimezone := context.WithTimeout(ctx, s.dbBudget)
	timezone, err := s.repository.DefaultTimezone(timezoneCtx)
	cancelTimezone()
	if err != nil {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		return Outcome{}, s.databaseFailure(requestCtx, timezoneCtx, err)
	}
	var probed metadata.Result
	err = temporary.UseReadOnlyFile(func(file *os.File) error {
		var probeErr error
		probed, probeErr = s.prober.Probe(ctx, file, timezone)
		return probeErr
	})
	if err != nil {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		return Outcome{}, classifyProbeError(ctx, err)
	}
	extension, err := storage.OriginalExtensionForMIME(probed.MIMEType)
	if err != nil {
		return Outcome{}, &Failure{Status: 415, Code: "unsupported_media_type", Message: "media type is not supported", Cause: err}
	}

	dbCtx, cancelDB := context.WithTimeout(ctx, s.dbBudget)
	defer cancelDB()
	for attempt := 0; attempt < s.timezoneTries; attempt++ {
		exifJSON, jsonErr := probed.EXIFJSON()
		if jsonErr != nil {
			return Outcome{}, internalFailure(jsonErr)
		}
		sourceJSON, jsonErr := probed.SourceMetadataJSON()
		if jsonErr != nil {
			return Outcome{}, internalFailure(jsonErr)
		}
		input := acceptance{
			Key: request.IdempotencyKey, RequestID: request.RequestID,
			RequestHash: CanonicalRequestHashV1(contentSHA, uint64(written), request.Filename),
			SHA256:      contentSHA, Size: written, Filename: request.Filename,
			OriginalID: originalID, AttemptID: attemptID, MediaID: mediaID, Timezone: timezone, Metadata: probed,
			EXIFJSON: exifJSON, SourceJSON: sourceJSON,
		}
		outcome, finalizeErr := s.repository.Finalize(dbCtx, input, func() (string, error) {
			key, publishErr := temporary.PublishSealed(dbCtx, extension)
			if publishErr == nil {
				published = true
			} else {
				var storageErr *storage.PublishError
				if errors.As(publishErr, &storageErr) && storageErr.Published {
					published = true
				}
			}
			return key, publishErr
		})
		if finalizeErr == nil {
			attemptFinished = true
			return outcome, nil
		}
		var changed *timezoneChangedError
		if errors.As(finalizeErr, &changed) {
			timezone = changed.Timezone
			capture, resolveErr := metadata.ResolveCapture(probed.Derived.Capture.Candidates, timezone)
			if resolveErr != nil {
				return Outcome{}, internalFailure(resolveErr)
			}
			probed.Derived.Capture = capture
			continue
		}
		var unknown *OutcomeUnknown
		if errors.As(finalizeErr, &unknown) {
			return Outcome{}, unknown
		}
		var rolledBack *CommitRolledBack
		if errors.As(finalizeErr, &rolledBack) {
			return Outcome{}, rolledBack
		}
		var failure *Failure
		if errors.As(finalizeErr, &failure) {
			return Outcome{}, failure
		}
		if published {
			return Outcome{}, dependencyFailure(finalizeErr)
		}
		if errors.Is(finalizeErr, storage.ErrCollision) || errors.Is(finalizeErr, storage.ErrValidation) ||
			errors.Is(finalizeErr, storage.ErrInvalidKey) || errors.Is(finalizeErr, storage.ErrUnexpectedType) {
			return Outcome{}, internalFailure(finalizeErr)
		}
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return Outcome{}, dependencyFailure(heartbeatErr)
		}
		return Outcome{}, s.databaseFailure(requestCtx, dbCtx, finalizeErr)
	}
	return Outcome{}, dependencyFailure(errTimezoneChanged)
}

func validateRequest(request Request) error {
	if request.Body == nil {
		return &Failure{Status: 400, Code: "invalid_multipart", Message: "upload body is missing"}
	}
	if err := ValidateIdempotencyKey(request.IdempotencyKey); err != nil {
		return &Failure{Status: 400, Code: "invalid_idempotency_key", Message: "idempotency key is invalid", Cause: err}
	}
	if !validRequestID(request.RequestID) {
		return &Failure{Status: 400, Code: "invalid_request", Message: "request ID is invalid"}
	}
	normalized, err := NormalizeFilename(request.Filename)
	if err != nil || !equalFilename(normalized, request.Filename) {
		return &Failure{Status: 400, Code: "invalid_filename", Message: "filename is not normalized", Cause: err}
	}
	return nil
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func equalFilename(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

type streamDestination struct {
	temporary io.Writer
	digest    io.Writer
	writeErr  error
}

func (w *streamDestination) Write(value []byte) (int, error) {
	n, err := w.temporary.Write(value)
	if err != nil {
		w.writeErr = err
		return n, err
	}
	if n != len(value) {
		w.writeErr = io.ErrShortWrite
		return n, io.ErrShortWrite
	}
	if _, err := w.digest.Write(value); err != nil {
		panic("sha256 hash write failed")
	}
	return n, nil
}

func classifyProbeError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return timeoutFailure(err)
	}
	if errors.Is(err, metadata.ErrUnsupportedMediaType) {
		return &Failure{Status: 415, Code: "unsupported_media_type", Message: "media type is not supported", Cause: err}
	}
	if errors.Is(err, metadata.ErrInvalidMedia) || errors.Is(err, metadata.ErrPolicyViolation) ||
		errors.Is(err, metadata.ErrProbeTimeout) || errors.Is(err, metadata.ErrProbeOutputLimit) ||
		errors.Is(err, metadata.ErrProbeFailed) {
		return &Failure{Status: 422, Code: "invalid_media", Message: "uploaded content is not valid supported media", Cause: err}
	}
	return dependencyFailure(err)
}

func classifySealError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return timeoutFailure(err)
	}
	return dependencyFailure(err)
}

func timeoutFailure(cause error) *Failure {
	return &Failure{Status: 408, Code: "upload_timeout", Message: "upload timed out", Cause: cause}
}

func dependencyFailure(cause error) *Failure {
	return &Failure{Status: 503, Code: "unavailable", Message: "service is temporarily unavailable", Cause: cause}
}

func internalFailure(cause error) *Failure {
	return &Failure{Status: 500, Code: "internal_error", Message: "an internal error occurred", Cause: cause}
}

func (s *Service) databaseFailure(requestCtx, dbCtx context.Context, cause error) error {
	if requestCtx.Err() != nil {
		return timeoutFailure(cause)
	}
	if dbCtx.Err() != nil {
		return dependencyFailure(cause)
	}
	var postgresError *pgconn.PgError
	if errors.As(cause, &postgresError) {
		code := postgresError.Code
		if len(code) >= 2 && (code[:2] == "08" || code[:2] == "40") || code == "55P03" || code == "57014" {
			return dependencyFailure(cause)
		}
		return internalFailure(cause)
	}
	return dependencyFailure(cause)
}

type storageAdapter struct{ store *storage.Store }

func (a storageAdapter) Begin(ctx context.Context, originalID, attemptID string) (stagedOriginal, error) {
	original, err := storage.ParseOriginalID(originalID)
	if err != nil {
		return nil, fmt.Errorf("parse generated original ID: %w", err)
	}
	attempt, err := storage.ParseAttemptID(attemptID)
	if err != nil {
		return nil, fmt.Errorf("parse generated attempt ID: %w", err)
	}
	upload, err := a.store.BeginOriginalUpload(ctx, original, attempt)
	if err != nil {
		return nil, err
	}
	return storageUploadAdapter{upload}, nil
}

type storageUploadAdapter struct{ upload *storage.OriginalUpload }

func (a storageUploadAdapter) Write(value []byte) (int, error) { return a.upload.Write(value) }
func (a storageUploadAdapter) UseReadOnlyFile(use func(*os.File) error) error {
	return a.upload.UseReadOnlyFile(use)
}
func (a storageUploadAdapter) Seal(ctx context.Context, validation storage.Validation) error {
	_, err := a.upload.Seal(ctx, validation)
	return err
}
func (a storageUploadAdapter) PublishSealed(ctx context.Context, extension storage.OriginalExtension) (string, error) {
	key, _, err := a.upload.PublishSealed(ctx, extension)
	return key.String(), err
}
func (a storageUploadAdapter) Abort(ctx context.Context) error { return a.upload.Abort(ctx) }
