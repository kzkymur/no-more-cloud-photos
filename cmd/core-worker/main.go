package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/animationprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
	"github.com/kzkymur/no-more-cloud-photos/internal/stillprocessor"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
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
	executor, err := configureTransformExecutor(ctx, cfg, repository, store)
	if err != nil {
		return err
	}
	jobWorker, err := worker.New(repository, map[job.Type]worker.Executor{job.TypeTransform: executor}, worker.Options{}, logger)
	if err != nil {
		return fmt.Errorf("configure job worker: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "core Worker ready", cfg.LogAttrs()...)
	if err := jobWorker.Run(ctx); err != nil {
		return fmt.Errorf("run job worker: %w", err)
	}
	logger.Info("core Worker stopped")
	return nil
}

func configureTransformExecutor(ctx context.Context, cfg config.WorkerConfig, repository *job.Repository, store *storage.Store) (*transformexecutor.Executor, error) {
	still, err := stillprocessor.New(stillprocessor.Config{
		Helper: cfg.StillHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: stillprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("configure still processor: %w", err)
	}
	animation, err := animationprocessor.New(animationprocessor.Config{
		Helper: cfg.AnimationHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: animationprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("configure animation processor: %w", err)
	}
	video, err := videoprocessor.New(videoprocessor.Config{
		Helper: cfg.VideoHelperPath, Prlimit: cfg.PrlimitPath, SRGBICC: cfg.SRGBICCPath,
		SRGBICCSHA256: cfg.SRGBICCSHA256, Policy: videoprocessor.DefaultPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("configure video processor: %w", err)
	}
	if _, err := still.Capabilities(ctx); err != nil {
		return nil, fmt.Errorf("check still processor capabilities: %w", err)
	}
	if _, err := animation.Capabilities(ctx); err != nil {
		return nil, fmt.Errorf("check animation processor capabilities: %w", err)
	}
	if _, err := video.Capabilities(ctx); err != nil {
		return nil, fmt.Errorf("check video processor capabilities: %w", err)
	}
	executor, err := transformexecutor.New(repository, transformexecutor.StoreAdapter{Store: store}, still, animation, video, transformexecutor.Options{})
	if err != nil {
		return nil, fmt.Errorf("configure transform executor: %w", err)
	}
	return executor, nil
}
