package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewJSON constructs a structured logger at one of the supported levels.
func NewJSON(w io.Writer, level string) (*slog.Logger, error) {
	if w == nil {
		return nil, fmt.Errorf("log writer is nil")
	}
	parsed, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parsed})
	return slog.New(handler), nil
}

// ParseLevel translates the public configuration values into slog levels.
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log level must be one of debug, info, warn, or error")
	}
}
