package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
	"github.com/kzkymur/no-more-cloud-photos/internal/logging"
)

const (
	exitOK       = 0
	exitFailure  = 1
	exitUsage    = 2
	exitConflict = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "migrate" || (args[1] != "status" && args[1] != "up") {
		fmt.Fprintln(stderr, "usage: nmcp-admin migrate (status|up) [--json]")
		return exitUsage
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	command := args[1]
	flags := flag.NewFlagSet("migrate "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write JSON output")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return exitUsage
	}

	cfg, err := config.LoadAdmin()
	if err != nil {
		logger.Error("load configuration", slog.Any("error", err))
		return exitFailure
	}
	configuredLogger, err := logging.NewJSON(stderr, cfg.LogLevel)
	if err != nil {
		logger.Error("configure logging", slog.Any("error", err))
		return exitFailure
	}
	logger = configuredLogger
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("configure database pool")
		return exitFailure
	}
	defer pool.Close()
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		logger.Error("configure migration runner", slog.Any("error", err))
		return exitFailure
	}
	if command == "up" {
		if err := migrator.Up(ctx); err != nil {
			logger.Error("apply migrations", slog.Any("error", err))
			return exitFailure
		}
	}
	status, err := migrator.Status(ctx)
	if err != nil {
		logger.Error("read migration status", slog.Any("error", err))
		return exitFailure
	}
	if status.Drift {
		logger.Error("migration history drift", slog.String("reason", status.DriftReason))
		return exitFailure
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			logger.Error("write output", slog.Any("error", err))
			return exitFailure
		}
	} else {
		fmt.Fprintf(stdout, "current=%d expected=%d pending=%t\n", status.CurrentVersion, status.ExpectedVersion, status.Pending)
	}
	if command == "status" && status.Pending {
		return exitConflict
	}
	return exitOK
}
