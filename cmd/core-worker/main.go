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
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
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
	logger.LogAttrs(ctx, slog.LevelInfo, "core Worker ready", cfg.LogAttrs()...)
	<-ctx.Done()
	logger.Info("core Worker stopped")
	return nil
}
