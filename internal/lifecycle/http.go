// Package lifecycle coordinates process shutdown.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// RunHTTP serves until ctx is cancelled, then drains requests within timeout.
func RunHTTP(ctx context.Context, server *http.Server, timeout time.Duration, logger *slog.Logger) error {
	if server == nil {
		return errors.New("HTTP server is nil")
	}
	if timeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}
	if logger == nil {
		return errors.New("logger is nil")
	}

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP during shutdown: %w", err)
	}
	return nil
}
