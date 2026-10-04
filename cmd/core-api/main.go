package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/httpapi"
	"github.com/kzkymur/no-more-cloud-photos/internal/lifecycle"
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
	"github.com/kzkymur/no-more-cloud-photos/internal/metadata"
	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
	"github.com/kzkymur/no-more-cloud-photos/internal/upload"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		bootstrapLogger().Error("core API stopped with an error", slog.Any("error", err))
		os.Exit(1)
	}
}

func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func run(ctx context.Context) error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger, err := logging.NewJSON(os.Stdout, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "starting core API", cfg.LogAttrs()...)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure database pool")
	}
	defer pool.Close()
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.StorageRoot, storage.Options{})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()
	prober, err := metadata.NewProber(metadata.DefaultPolicy(), metadata.DefaultToolPaths())
	if err != nil {
		return fmt.Errorf("configure metadata prober: %w", err)
	}
	uploadService, err := upload.NewService(pool, store, prober)
	if err != nil {
		return fmt.Errorf("configure upload service: %w", err)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewHandler(httpapi.Dependencies{Database: pool, Migrations: migrator, Storage: store, Upload: uploadService}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	if err := lifecycle.RunHTTP(ctx, server, cfg.ShutdownTimeout, logger); err != nil {
		return err
	}
	logger.Info("core API stopped")
	return nil
}
