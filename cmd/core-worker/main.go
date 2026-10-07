package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
	"github.com/kzkymur/no-more-cloud-photos/internal/medialifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/profile"
	"github.com/kzkymur/no-more-cloud-photos/internal/purgeexecutor"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformcapability"
	"github.com/kzkymur/no-more-cloud-photos/internal/transformexecutor"
	"github.com/kzkymur/no-more-cloud-photos/internal/videoprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		bootstrapLogger().Error("core Worker stopped with an error", slog.Any("error", err))
		os.Exit(1)
	}
}

func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func run(ctx context.Context) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger, err := logging.NewJSON(os.Stdout, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure database pool")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database is unavailable")
	}
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		return err
	}
	status, err := migrator.Status(ctx)
	if err != nil {
		return fmt.Errorf("check migrations: %w", err)
	}
	if !status.Ready() {
		return fmt.Errorf("database migrations are not current")
	}
	store, err := storage.Open(cfg.StorageRoot, storage.Options{})
	if err != nil {
		return fmt.Errorf("storage root is unavailable: %w", err)
	}
	defer store.Close()
	if err := store.Probe(ctx); err != nil {
		return fmt.Errorf("storage root is unavailable: %w", err)
	}
	repository, err := job.NewRepository(pool, job.Options{FileBaseURL: cfg.FileBaseURL, Checkpoint: store.DatabaseCheckpoint})
	if err != nil {
		return fmt.Errorf("configure job repository: %w", err)
	}
	executor, envelope, err := configureTransformExecutor(ctx, cfg, repository, store)
	if err != nil {
		return err
	}
	claimRepository, err := repository.BindTransformClaims(envelope)
	if err != nil {
		return fmt.Errorf("bind transform claims: %w", err)
	}
	jobWorker, err := worker.New(claimRepository, map[job.Type]worker.Executor{job.TypeTransform: executor}, worker.Options{}, logger)
	if err != nil {
		return fmt.Errorf("configure job worker: %w", err)
	}
	purgeService, err := medialifecycle.NewService(pool, cfg.FileBaseURL)
	if err != nil {
		return fmt.Errorf("configure purge service: %w", err)
	}
	purgeWorker, err := worker.New(purgeexecutor.Repository{Service: purgeService}, map[job.Type]worker.Executor{
		job.TypePurge: purgeexecutor.Executor{Service: purgeService, Store: store},
	}, worker.Options{DatabaseTimeout: 30 * time.Second}, logger)
	if err != nil {
		return fmt.Errorf("configure purge worker: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "core Worker ready", cfg.LogAttrs()...)
	if err := runWorkers(ctx, jobWorker, purgeWorker); err != nil {
		return fmt.Errorf("run job worker: %w", err)
	}
	logger.Info("core Worker stopped")
	return nil
}

type workerLoop interface{ Run(context.Context) error }

func runWorkers(ctx context.Context, loops ...workerLoop) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(loops))
	for _, loop := range loops {
		go func(loop workerLoop) { results <- loop.Run(runCtx) }(loop)
	}
	var first error
	for index := range loops {
		err := <-results
		if index == 0 {
			cancel()
		}
		if err != nil && first == nil {
			first = err
		}
		if err == nil && ctx.Err() == nil && first == nil {
			first = fmt.Errorf("worker loop exited unexpectedly")
		}
	}
	return first
}

func configureTransformExecutor(ctx context.Context, cfg config.WorkerConfig, repository *job.Repository, store *storage.Store) (*transformexecutor.Executor, transformcapability.Envelope, error) {
	still, err := stillprocessor.New(stillprocessor.Config{
		Helper: cfg.StillHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: stillprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("configure still processor: %w", err)
	}
	animation, err := animationprocessor.New(animationprocessor.Config{
		Helper: cfg.AnimationHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: animationprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("configure animation processor: %w", err)
	}
	video, err := videoprocessor.New(videoprocessor.Config{
		Helper: cfg.VideoHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: videoprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("configure video processor: %w", err)
	}
	stillCapabilities, err := still.Capabilities(ctx)
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("check still processor capabilities: %w", err)
	}
	animationCapabilities, err := animation.Capabilities(ctx)
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("check animation processor capabilities: %w", err)
	}
	videoCapabilities, err := video.Capabilities(ctx)
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("check video processor capabilities: %w", err)
	}
	envelope, err := validateTransformStartup(ctx, repository, stillCapabilities, animationCapabilities, videoCapabilities)
	if err != nil {
		return nil, transformcapability.Envelope{}, err
	}
	executor, err := transformexecutor.New(repository, transformexecutor.StoreAdapter{Store: store}, still, animation, video, transformexecutor.Options{})
	if err != nil {
		return nil, transformcapability.Envelope{}, fmt.Errorf("configure transform executor: %w", err)
	}
	return executor, envelope, nil
}

type claimableProfileLoader interface {
	ClaimableTransformProfiles(context.Context) ([]profile.Definition, error)
}

func validateTransformStartup(ctx context.Context, repository claimableProfileLoader, still stillprocessor.Capabilities, animation animationprocessor.Capabilities, video videoprocessor.Capabilities) (transformcapability.Envelope, error) {
	definitions, err := repository.ClaimableTransformProfiles(ctx)
	if err != nil {
		return transformcapability.Envelope{}, fmt.Errorf("load claimable transform profiles: %w", err)
	}
	envelope, err := transformcapability.ValidateEnvelope(definitions, still, animation, video)
	if err != nil {
		return transformcapability.Envelope{}, fmt.Errorf("validate transform capability envelope: %w", err)
	}
	return envelope, nil
}
