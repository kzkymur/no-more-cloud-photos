package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestNewJSONLevelFiltering(t *testing.T) {
	tests := []struct {
		level string
		want  []string
	}{
		{level: "debug", want: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{level: "info", want: []string{"INFO", "WARN", "ERROR"}},
		{level: "warn", want: []string{"WARN", "ERROR"}},
		{level: "error", want: []string{"ERROR"}},
		{level: "ERROR", want: []string{"ERROR"}},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := NewJSON(&output, tt.level)
			if err != nil {
				t.Fatalf("NewJSON() error = %v", err)
			}
			logger.Debug("debug message")
			logger.Info("info message", slog.String("component", "test"))
			logger.Warn("warn message")
			logger.Error("error message")

			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != len(tt.want) {
				t.Fatalf("got %d records, want %d: %q", len(lines), len(tt.want), output.String())
			}
			for i, line := range lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("record %d is not JSON: %v", i, err)
				}
				if record["level"] != tt.want[i] {
					t.Errorf("record %d level = %v, want %s", i, record["level"], tt.want[i])
				}
				if _, ok := record["msg"]; !ok {
					t.Errorf("record %d has no msg: %v", i, record)
				}
			}
		})
	}
}

func TestNewJSONPreservesStructuredAttributes(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSON(&output, "info")
	if err != nil {
		t.Fatalf("NewJSON() error = %v", err)
	}
	logger.Info("started", slog.String("component", "api"), slog.Int("workers", 1))

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if record["component"] != "api" || record["workers"] != float64(1) {
		t.Fatalf("structured attributes = %v", record)
	}
}

func TestNewJSONRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name  string
		write io.Writer
		level string
	}{
		{name: "nil writer", level: "info"},
		{name: "empty level", write: &bytes.Buffer{}},
		{name: "trace level", write: &bytes.Buffer{}, level: "trace"},
		{name: "whitespace level", write: &bytes.Buffer{}, level: " info "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewJSON(tt.write, tt.level); err == nil {
				t.Fatal("NewJSON() error = nil")
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{input: "debug", want: slog.LevelDebug},
		{input: "info", want: slog.LevelInfo},
		{input: "warn", want: slog.LevelWarn},
		{input: "error", want: slog.LevelError},
	}
	for _, tt := range tests {
		got, err := ParseLevel(tt.input)
		if err != nil || got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v, nil", tt.input, got, err, tt.want)
		}
	}
}
