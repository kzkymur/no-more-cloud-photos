package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/job"
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
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
	repository, err := job.NewRepository(pool, job.Options{FileBaseURL: cfg.FileBaseURL})
	if err != nil {
		return fmt.Errorf("configure job repository: %w", err)
	}
	// Processor executors are registered by issues #11-#14 after their runtime
	// capability checks and atomic publication path exist. An empty registry
	// still reclaims expired leases but cannot claim or no-op-complete work.
	jobWorker, err := worker.New(repository, nil, worker.Options{}, logger)
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
