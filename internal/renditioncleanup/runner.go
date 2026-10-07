package renditioncleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

// Unlink is an alias so medialifecycle can implement Service without importing
// this package solely for a named callback type.
type Unlink = func(context.Context, string, string, int64) (bool, error)

const (
	DefaultSweepInterval       = 30 * time.Second
	DefaultSweepLimit          = 50
	DefaultTransientBackoff    = 250 * time.Millisecond
	DefaultMaxTransientBackoff = 30 * time.Second
	DefaultPoisonCooldown      = time.Minute
	DefaultMaxPoisonCooldown   = time.Hour
	maxTrackedPoisonCandidates = 1000
)

// Service owns candidate selection and the database operation boundary. It
// returns an empty ID with a nil error when no candidate is due. Once a
// candidate is selected, its ID must be returned even when the operation fails.
// preferredID, when non-empty, must be considered before every other candidate;
// excludedIDs must not be selected. The callback removes one exact rendition
// snapshot and reports whether it was already missing; it must be called
// synchronously.
type Service interface {
	CleanupNextRendition(context.Context, string, []string, Unlink) (string, error)
}

type Store interface {
	DeleteRendition(context.Context, storage.RenditionKey, storage.DeleteExpectation) (storage.DeleteResult, error)
}

type Options struct {
	SweepInterval       time.Duration
	SweepLimit          int
	TransientBackoff    time.Duration
	MaxTransientBackoff time.Duration
	PoisonCooldown      time.Duration
	MaxPoisonCooldown   time.Duration
	Now                 func() time.Time
	Sleep               func(context.Context, time.Duration) error
	Random              func() float64
}

type Runner struct {
	service Service
	store   Store
	options Options
	logger  *slog.Logger
	poison  map[string]poisonState
}

type poisonState struct {
	failures int
	until    time.Time
}

func New(service Service, store Store, options Options, logger *slog.Logger) (*Runner, error) {
	if service == nil || store == nil {
		return nil, errors.New("rendition cleanup dependencies are required")
	}
	options = withDefaults(options)
	if !validOptions(options) {
		return nil, errors.New("invalid rendition cleanup options")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{service: service, store: store, options: options, logger: logger, poison: make(map[string]poisonState)}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("rendition cleanup context is nil")
	}
	preferredID := ""
	transientFailures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		exhausted := true
		for range r.options.SweepLimit {
			if ctx.Err() != nil {
				return nil
			}
			id, err := r.service.CleanupNextRendition(ctx, preferredID, r.exclusions(), r.unlink)
			if ctx.Err() != nil {
				return nil
			}
			switch {
			case err == nil && id == "":
				preferredID = ""
				transientFailures = 0
				exhausted = false
			case err == nil:
				delete(r.poison, id)
				preferredID = ""
				transientFailures = 0
			case isUncertain(err):
				if id == "" {
					return fmt.Errorf("rendition cleanup uncertainty omitted candidate ID: %w", err)
				}
				preferredID = id
				transientFailures++
				r.logger.WarnContext(ctx, "rendition cleanup outcome uncertain", slog.String("rendition_id", id))
				if !r.wait(ctx, r.backoff(r.options.TransientBackoff, r.options.MaxTransientBackoff, transientFailures)) {
					return nil
				}
			case isTransient(err):
				transientFailures++
				r.logger.WarnContext(ctx, "rendition cleanup temporarily unavailable")
				if !r.wait(ctx, r.backoff(r.options.TransientBackoff, r.options.MaxTransientBackoff, transientFailures)) {
					return nil
				}
			case id != "" && isPoison(err):
				preferredID = ""
				transientFailures = 0
				state := r.deferPoison(id)
				r.logger.ErrorContext(ctx, "rendition cleanup candidate deferred", slog.String("rendition_id", id), slog.Any("error", err), slog.Time("retry_at", state.until))
			default:
				return fmt.Errorf("rendition cleanup failed: %w", err)
			}
			if err == nil && id == "" {
				break
			}
		}
		if exhausted {
			r.logger.DebugContext(ctx, "rendition cleanup sweep limit reached", slog.Int("limit", r.options.SweepLimit))
		}
		if !r.wait(ctx, r.options.SweepInterval) {
			return nil
		}
	}
}

func (r *Runner) deferPoison(id string) poisonState {
	state, tracked := r.poison[id]
	if !tracked && len(r.poison) >= maxTrackedPoisonCandidates {
		var oldestID string
		var oldest time.Time
		for candidateID, candidate := range r.poison {
			if oldestID == "" || candidate.until.Before(oldest) || candidate.until.Equal(oldest) && candidateID < oldestID {
				oldestID, oldest = candidateID, candidate.until
			}
		}
		delete(r.poison, oldestID)
	}
	state.failures++
	state.until = r.options.Now().Add(r.backoff(r.options.PoisonCooldown, r.options.MaxPoisonCooldown, state.failures))
	r.poison[id] = state
	return state
}

func (r *Runner) unlink(ctx context.Context, renditionID, relativePath string, sizeBytes int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key, err := storage.ParseRenditionKey(relativePath)
	if err != nil || key.RenditionID().String() != renditionID || sizeBytes < 0 {
		return false, storage.ErrValidation
	}
	result, err := r.store.DeleteRendition(ctx, key, storage.DeleteExpectation{ExpectedSize: sizeBytes})
	return result.Missing, err
}

func (r *Runner) exclusions() []string {
	now := r.options.Now()
	result := make([]string, 0, len(r.poison))
	for id, state := range r.poison {
		if now.Before(state.until) {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func (r *Runner) backoff(base, maximum time.Duration, failures int) time.Duration {
	delay := base
	for count := 1; count < failures && delay < maximum; count++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	jitter := r.options.Random()
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	return delay/2 + time.Duration(jitter*float64(delay-delay/2))
}

func (r *Runner) wait(ctx context.Context, delay time.Duration) bool {
	return r.options.Sleep(ctx, delay) == nil && ctx.Err() == nil
}

func isUncertain(err error) bool {
	var unknown *medialifecycle.CommitOutcomeUnknown
	return errors.Is(err, storage.ErrOutcomeUncertain) || errors.As(err, &unknown)
}

func isTransient(err error) bool {
	return medialifecycle.IsKind(err, medialifecycle.KindDatabaseUnavailable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, storage.ErrNoSpace) || errors.Is(err, storage.ErrQuota) ||
		errors.Is(err, storage.ErrPermission) || errors.Is(err, storage.ErrReadOnly) ||
		errors.Is(err, storage.ErrDurability)
}

func isPoison(err error) bool {
	return errors.Is(err, storage.ErrValidation) || errors.Is(err, storage.ErrSymlink) ||
		errors.Is(err, storage.ErrUnexpectedType)
}

func withDefaults(options Options) Options {
	if options.SweepInterval == 0 {
		options.SweepInterval = DefaultSweepInterval
	}
	if options.SweepLimit == 0 {
		options.SweepLimit = DefaultSweepLimit
	}
	if options.TransientBackoff == 0 {
		options.TransientBackoff = DefaultTransientBackoff
	}
	if options.MaxTransientBackoff == 0 {
		options.MaxTransientBackoff = DefaultMaxTransientBackoff
	}
	if options.PoisonCooldown == 0 {
		options.PoisonCooldown = DefaultPoisonCooldown
	}
	if options.MaxPoisonCooldown == 0 {
		options.MaxPoisonCooldown = DefaultMaxPoisonCooldown
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleep
	}
	if options.Random == nil {
		options.Random = rand.Float64
	}
	return options
}

func validOptions(options Options) bool {
	return options.SweepInterval > 0 && options.SweepLimit > 0 &&
		options.TransientBackoff > 0 && options.MaxTransientBackoff >= options.TransientBackoff &&
		options.PoisonCooldown > 0 && options.MaxPoisonCooldown >= options.PoisonCooldown &&
		options.Now != nil && options.Sleep != nil && options.Random != nil
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
